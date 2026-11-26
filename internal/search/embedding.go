package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
)

// TextEmbeddingRetriever is the fifth retrieval channel: real text embeddings
// of the derived shot text, scored by cosine against the embedded query.
// It is a retrieval signal only — the evidence gate never treats an embedding
// hit as observation evidence, exactly like the semantic channel before it.
//
// The scan is a full-library cosine pass in Go over the served space's
// vectors (SQLite-first: no vector DB). That is the deliberate v1 trade-off;
// a library of tens of thousands of shots stays comfortably inside a few
// milliseconds per query at 256-1024 dimensions, and the boundary is where a
// future ANN index plugs in without touching the engine.
//
// Degradation is graceful: with no embedder configured, or with no vectors
// stored under the current space, the channel returns nothing and fusion
// simply never sees the signal — recall keeps working.
type TextEmbeddingRetriever struct {
	store    ShotStore
	embedder TextEmbedder
}

// IdentifiedTextEmbedder is the optional identity-aware extension of
// TextEmbedder. A routed provider (a channel that may fall back from the
// primary route to a different backend within one operation) cannot promise
// which backend will serve a batch before it runs, so it carries the
// provenance of the batch that actually came back:
//
//   - EmbedInSpace returns the vectors together with the space ID of the
//     backend that produced them — the same operation, so the identity can
//     never disagree with the vectors.
//   - EmbeddingSpaceID plans the primary route's space without any provider
//     call. It is what a rebuild compares against, so a rebuild knows which
//     shots are stale in the space it would write next.
//
// Vectors live in one space and are scored against queries of that space —
// never across spaces (see embedTextInSpace).
type IdentifiedTextEmbedder interface {
	TextEmbedder
	EmbeddingSpaceID() string
	EmbedInSpace(ctx context.Context, texts []string) ([][]float64, string, error)
}

// embedTextInSpace embeds one batch and reports the space the vectors live
// in. An embedder implementing IdentifiedTextEmbedder decides its own space
// (the backend that actually served); a plain TextEmbedder is a
// single-provider caller whose vectors always live under Model() — the
// shape every test/library embedder has always had.
func embedTextInSpace(ctx context.Context, embedder TextEmbedder, texts []string) ([][]float64, string, error) {
	if identified, ok := embedder.(IdentifiedTextEmbedder); ok {
		return identified.EmbedInSpace(ctx, texts)
	}
	vectors, err := embedder.Embed(ctx, texts)
	return vectors, embedder.Model(), err
}

func NewTextEmbeddingRetriever(store ShotStore, embedder TextEmbedder) *TextEmbeddingRetriever {
	return &TextEmbeddingRetriever{store: store, embedder: embedder}
}

func (r *TextEmbeddingRetriever) Name() string { return SignalTextEmbedding }

// embeddingCutoffFraction bounds a dense channel's contribution to fusion.
// Unlike lexical/transcript (which rank only shots that matched), an
// embedding channel scores EVERY shot with some small cosine — and RRF gives
// every ranked shot a score, so the un-thresholded long tail of
// near-orthogonal noise would drown the sparse channels' strong top ranks.
// The cutoff keeps only the meaningful cluster (down to 80% of the channel's
// own best similarity) and drops the rest.
const embeddingCutoffFraction = 0.8

func (r *TextEmbeddingRetriever) Retrieve(ctx context.Context, q SearchQuery, limit int) ([]Candidate, error) {
	if r.embedder == nil {
		return nil, nil
	}
	vectors, spaceID, err := embedTextInSpace(ctx, r.embedder, []string{q.Raw})
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, nil
	}
	queryVector := vectors[0]
	if err := validateQueryEmbedding(queryVector); err != nil {
		return []Candidate{}, err
	}
	rows, err := r.store.ListShotTextEmbeddings(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	// Full-library cosine scan. A stored vector whose dimension differs from
	// the query vector's is a different space's artifact (a provider update
	// without a rebuild) — it is skipped before the cutoff/cluster logic, so
	// it neither distorts maxSimilarity nor enters the scored output.
	maxSimilarity := 0.0
	scored := make([]Candidate, 0, len(rows))
	for _, row := range rows {
		if len(row.Vector) != len(queryVector) {
			continue
		}
		similarity := embeddingCosine(queryVector, row.Vector)
		if similarity <= 0 || math.IsNaN(similarity) || math.IsInf(similarity, 0) {
			continue
		}
		if similarity > maxSimilarity {
			maxSimilarity = similarity
		}
		candidate := toCandidate(row.Shot)
		candidate.Signals[SignalTextEmbedding] = similarity
		candidate.Score = similarity
		scored = append(scored, candidate)
	}
	cutoff := maxSimilarity * embeddingCutoffFraction
	kept := scored[:0]
	for _, candidate := range scored {
		if candidate.Score < cutoff {
			continue
		}
		kept = append(kept, candidate)
	}
	scored = kept
	sortCandidates(scored)
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}

func validateQueryEmbedding(vector []float64) error {
	if len(vector) == 0 {
		return fmt.Errorf("invalid query embedding: empty vector")
	}
	var norm float64
	for _, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("invalid query embedding: non-finite component")
		}
		norm += value * value
	}
	if math.IsNaN(norm) || math.IsInf(norm, 0) || norm == 0 {
		return fmt.Errorf("invalid query embedding: zero or non-finite norm")
	}
	return nil
}

// ShotTextSource builds the embedding source text from a shot document —
// the exact string the source_text_hash covers and the embedder receives.
// It is the single definition of "the derived text of a shot": the store's
// projections (AllShotTextDocuments / ShotTextDocumentsByAsset) fill exactly
// these fields, and rebuilds never disagree with persisted hashes.
func ShotTextSource(doc ShotSearchDocument) string {
	parts := []string{doc.Description}
	parts = append(parts, doc.Tags...)
	parts = append(parts, doc.Objects...)
	parts = append(parts, doc.Actions...)
	parts = append(parts, doc.Mood...)
	return strings.Join(parts, " ")
}

// ShotTextSourceHash is the rebuild trigger: it changes exactly when the
// derived text changes, so a reanalysis of one asset re-embeds only the
// shots whose description/tags/objects/actions/mood actually moved.
func ShotTextSourceHash(doc ShotSearchDocument) string {
	sum := sha256.Sum256([]byte(ShotTextSource(doc)))
	return hex.EncodeToString(sum[:16])
}

// embeddingCosine is cosine similarity between the provider's float64 query
// vector and the stored float32 shot vector. Dimension equality is exact: a
// mismatch — a same-model dimension change, e.g. a provider update without a
// rebuild — is rejected with a score of 0, never computed over a truncated
// shared prefix. Truncating would silently cross-score vectors that were
// produced under different dimensionalities.
func embeddingCosine(query []float64, vector []float32) float64 {
	n := len(query)
	if n != len(vector) {
		return 0
	}
	if n == 0 {
		return 0
	}
	var dot, queryNorm, vectorNorm float64
	for i := 0; i < n; i++ {
		value := float64(vector[i])
		if math.IsNaN(query[i]) || math.IsInf(query[i], 0) || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0
		}
		dot += query[i] * value
		queryNorm += query[i] * query[i]
		vectorNorm += value * value
	}
	if queryNorm == 0 || vectorNorm == 0 {
		return 0
	}
	result := dot / math.Sqrt(queryNorm*vectorNorm)
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0
	}
	return result
}
