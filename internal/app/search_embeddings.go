package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/providers"
	"github.com/evjohn-icu/nexusgate/internal/search"
)

// searchEmbedderAdapter adapts the Hub's embedding provider (legacy config or
// provider channel — the channel runtime wrapper resolves both) to the search
// engine's TextEmbedder contract, plus its identity-aware extension: the
// wrapper decides the space a batch lands in (the backend that actually
// served it), so both Embed and EmbedInSpace are needed.
//
// One behavioural override carries over from the single-provider adapter: an
// unconfigured embedding capability is an EXPECTED state, not a fault, so
// EmbedInSpace reports "no vectors, no space" instead of an error. Without
// this, a Hub that never configured embedding would fail every semantic/fact
// search — the retriever treats an empty response as "this channel has
// nothing to say", exactly like the transcript channel without aligned words.
type searchEmbedderAdapter struct {
	embedder providers.Embedder
}

func (a searchEmbedderAdapter) Name() string { return a.embedder.Name() }

// Model is kept for display and single-provider consumers (tag curation
// still keys its own storage by it); vector identity never reads it — the
// routed wrapper's space ID is what embeddings are stored and scored under.
func (a searchEmbedderAdapter) Model() string { return a.embedder.Model() }

func (a searchEmbedderAdapter) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	vectors, _, err := a.EmbedInSpace(ctx, texts)
	return vectors, err
}

// EmbeddingSpaceID plans the space the next successful batch would land in
// (first channel route, else legacy provider). For a plain single-provider
// embedder there is no routing, so its display model is the space — the
// identity single-provider callers have always had.
func (a searchEmbedderAdapter) EmbeddingSpaceID() string {
	if identified, ok := a.embedder.(search.IdentifiedTextEmbedder); ok {
		return identified.EmbeddingSpaceID()
	}
	return a.embedder.Model()
}

func (a searchEmbedderAdapter) EmbedInSpace(ctx context.Context, texts []string) ([][]float64, string, error) {
	if identified, ok := a.embedder.(search.IdentifiedTextEmbedder); ok {
		vectors, spaceID, err := identified.EmbedInSpace(ctx, texts)
		if err != nil {
			if errors.Is(err, ErrProviderChannelNotConfigured) {
				return nil, "", nil
			}
			return nil, "", err
		}
		return vectors, spaceID, nil
	}
	vectors, err := a.embedder.Embed(ctx, texts)
	if err != nil {
		if errors.Is(err, ErrProviderChannelNotConfigured) {
			return nil, "", nil
		}
		return nil, "", err
	}
	return vectors, a.embedder.Model(), nil
}

// plannedEmbeddingSpace resolves the space the current embedding backend
// would write next, or "" when no backend is configured. The "" shortcut
// matters for the pipeline hook: it turns "never configured" into a silent
// no-op before any store query runs.
func (s *Service) plannedEmbeddingSpace() (store search.ShotStore, adapter searchEmbedderAdapter, spaceID string, ok bool) {
	store, ok = s.repo.(search.ShotStore)
	if !ok || s.embedder == nil {
		return nil, searchEmbedderAdapter{}, "", false
	}
	adapter = searchEmbedderAdapter{embedder: s.embedder}
	spaceID = adapter.EmbeddingSpaceID()
	if spaceID == "" {
		return nil, searchEmbedderAdapter{}, "", false
	}
	return store, adapter, spaceID, true
}

// ensureShotTextEmbeddings is the pipeline's post-commit hook: after shot
// rows become canonical it re-embeds exactly the shots whose derived text
// changed in the planned (primary) space. It is synchronous (the pipeline's
// "process exited means no call in flight" invariant) and never fails the
// job — the embedding layer is derived, rebuildable and best-effort, so every
// failure path ends in a log line instead of an error.
//
// The comparison happens in the planned space so a shot whose primary-space
// row is missing or stale is re-embedded even if a fallback space already
// holds a usable vector: the primary is what a recovered rebuild will write,
// and while the primary is down the batch itself lands in whichever space
// actually served it (the write uses the returned space ID).
func (s *Service) ensureShotTextEmbeddings(ctx context.Context, assetID string) {
	store, adapter, spaceID, ok := s.plannedEmbeddingSpace()
	if !ok {
		return
	}
	docs, err := store.ShotTextDocumentsByAsset(ctx, assetID)
	if err != nil {
		slog.Warn("shot text embedding: read documents", "asset_id", assetID, "error", err)
		return
	}
	existing, err := store.ShotTextEmbeddingHashes(ctx, spaceID, assetID)
	if err != nil {
		slog.Warn("shot text embedding: read hashes", "asset_id", assetID, "error", err)
		return
	}
	var changed []search.ShotSearchDocument
	for _, doc := range docs {
		if existing[doc.ShotID] != search.ShotTextSourceHash(doc) {
			changed = append(changed, doc)
		}
	}
	if len(changed) == 0 {
		return
	}
	slog.Debug("shot text embedding: incremental embed", "asset_id", assetID, "shots", len(changed))
	if _, err := embedShotDocuments(ctx, store, adapter, changed); err != nil {
		// Never fail the job: log and move on. The full rebuild command
		// (nexusgate search rebuild-embeddings) can repair the gap.
		slog.Warn("shot text embedding: embed failed (analysis job unaffected)", "asset_id", assetID, "error", err)
	}
}

// RebuildShotTextEmbeddings is the full retrieval_generation rebuild: every
// shot whose planned-space row is missing or stale is re-embedded, in bounded
// batches. "Missing or stale" is read through ListShotTextEmbeddings, which
// only returns rows whose source hash still matches the current projection —
// so a shot whose row went stale (including one that carries only a stale
// fallback-space vector) is rebuilt, while healthy rows and other spaces'
// rows are left exactly as they are. Switching the embedding backend
// therefore means running this once — the canonical analysis is never
// touched, and no Hub start ever triggers the rebuild implicitly: it stays
// the documented operator command.
func (s *Service) RebuildShotTextEmbeddings(ctx context.Context) (int, error) {
	store, adapter, spaceID, ok := s.plannedEmbeddingSpace()
	if !ok {
		return 0, errors.New("search engine or embedding provider not available")
	}
	docs, err := store.AllShotTextDocuments(ctx)
	if err != nil {
		return 0, err
	}
	existing, err := store.ListShotTextEmbeddings(ctx, spaceID)
	if err != nil {
		return 0, err
	}
	hashes := make(map[string]string, len(existing))
	for _, row := range existing {
		hashes[row.Shot.ID] = row.SourceTextHash
	}
	var changed []search.ShotSearchDocument
	for _, doc := range docs {
		if hashes[doc.ShotID] != search.ShotTextSourceHash(doc) {
			changed = append(changed, doc)
		}
	}
	if len(changed) == 0 {
		return 0, nil
	}
	total := 0
	const batchSize = 64
	for start := 0; start < len(changed); start += batchSize {
		end := start + batchSize
		if end > len(changed) {
			end = len(changed)
		}
		batch := changed[start:end]
		persisted, err := embedShotDocuments(ctx, store, adapter, batch)
		if err != nil {
			return total, err
		}
		total += persisted
	}
	return total, nil
}

// embedShotDocuments embeds one batch of shot documents and persists the rows
// under the space that actually served the batch. An unconfigured provider
// reports no vectors (the adapter's contract): that is the expected no-op
// state, not an error — the caller logs nothing and moves on, and the rebuild
// command reports zero embeddings so an operator knows there is nothing to
// build.
//
// A batch that is malformed or ragged is an error BEFORE any write: fewer or
// more vectors than inputs, a non-finite or zero-norm vector, vectors that
// disagree on dimension, or empty vectors mixed with real ones would either
// poison the space with unusable rows or silently drop data — every row of a
// batch must be usable, or none of it is written. A batch whose vectors are
// all empty keeps the prior no-vector semantics: the provider said nothing,
// so there is nothing to persist and nothing failed.
func embedShotDocuments(ctx context.Context, store search.ShotStore, embedder searchEmbedderAdapter, docs []search.ShotSearchDocument) (int, error) {
	texts := make([]string, len(docs))
	for i, doc := range docs {
		texts[i] = search.ShotTextSource(doc)
	}
	vectors, spaceID, err := embedder.EmbedInSpace(ctx, texts)
	if err != nil {
		return 0, err
	}
	if len(vectors) == 0 {
		// Embedding not configured: nothing to persist, nothing failed.
		return 0, nil
	}
	if spaceID == "" {
		return 0, errors.New("embedding provider returned vectors without a space identity")
	}
	if len(vectors) != len(docs) {
		return 0, fmt.Errorf("embedding provider returned %d vectors for %d inputs", len(vectors), len(docs))
	}
	nonEmpty := make([][]float64, 0, len(vectors))
	for i, vector := range vectors {
		if len(vector) == 0 {
			continue
		}
		var norm float64
		for _, value := range vector {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return 0, fmt.Errorf("embedding vector %d has a non-finite component", i)
			}
			norm += value * value
		}
		if norm == 0 {
			return 0, fmt.Errorf("embedding vector %d has zero norm", i)
		}
		nonEmpty = append(nonEmpty, vector)
	}
	if len(nonEmpty) == 0 {
		// Every vector empty: the provider has nothing to say for this batch.
		return 0, nil
	}
	if len(nonEmpty) != len(vectors) {
		return 0, fmt.Errorf("embedding batch is ragged: %d of %d vectors are empty", len(nonEmpty), len(vectors))
	}
	dimension := len(nonEmpty[0])
	for _, vector := range nonEmpty[1:] {
		if len(vector) != dimension {
			return 0, fmt.Errorf("embedding batch mixes dimensions %d and %d", dimension, len(vector))
		}
	}
	rows := make([]search.ShotEmbeddingRow, 0, len(docs))
	for i, doc := range docs {
		rows = append(rows, search.ShotEmbeddingRow{
			Shot:           domainShotForEmbedding(doc),
			Model:          spaceID,
			Vector:         toFloat32(vectors[i]),
			SourceTextHash: search.ShotTextSourceHash(doc),
		})
	}
	if err := store.UpsertShotTextEmbeddings(ctx, rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// domainShotForEmbedding builds the minimal shot row an embedding row needs
// (the engine re-reads full rows from ListShotTextEmbeddings; the upsert only
// stores identity).
func domainShotForEmbedding(doc search.ShotSearchDocument) domain.ShotSearchResult {
	return domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: doc.ShotID}}
}

func toFloat32(values []float64) []float32 {
	out := make([]float32, len(values))
	for i, v := range values {
		out[i] = float32(v)
	}
	return out
}
