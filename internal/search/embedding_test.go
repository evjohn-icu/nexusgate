package search

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
	"sync"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// fakeEmbedder is a deterministic, offline text embedder: each token hashes
// into a 256-dimension vector, distinct from the discovery FNV heuristic, so
// tests can prove the embedding channel contributes an orthogonal signal.
type fakeEmbedder struct {
	dim int
}

func (f fakeEmbedder) Name() string  { return "fake" }
func (f fakeEmbedder) Model() string { return "fake-embed-v1" }

func (f fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i, text := range texts {
		out[i] = fakeVector(text, f.dim)
	}
	return out, nil
}

func fakeVector(text string, dim int) []float64 {
	vector := make([]float64, dim)
	seen := map[string]bool{}
	for _, token := range fakeTokens(text) {
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		h := fnv.New64a()
		_, _ = h.Write([]byte(token))
		sum := h.Sum64()
		index := int(sum % uint64(dim))
		if sum&1 == 0 {
			vector[index] += 1
		} else {
			vector[index] -= 1
		}
	}
	var norm float64
	for _, v := range vector {
		norm += v * v
	}
	if norm == 0 {
		return vector
	}
	scale := 1 / math.Sqrt(norm)
	for i := range vector {
		vector[i] *= scale
	}
	return vector
}

func fakeTokens(text string) []string {
	// Whole ASCII words plus CJK bigrams, deterministically.
	var out []string
	current := []rune{}
	flush := func() {
		runes := current
		current = nil
		if len(runes) == 0 {
			return
		}
		word := string(runes)
		if len(runes) > 3 {
			for i := 0; i+1 < len(runes); i++ {
				out = append(out, string(runes[i:i+2]))
			}
			return
		}
		out = append(out, word)
	}
	for _, r := range text {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= '\u3040' && r <= '\u9fff' {
			current = append(current, r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func fakeVector32(text string, dim int) []float32 {
	vector := fakeVector(text, dim)
	out := make([]float32, len(vector))
	for i, v := range vector {
		out[i] = float32(v)
	}
	return out
}

// fakeEmbeddingStore is a minimal scripted ShotStore for embedding-channel
// tests: it serves scripted embedding rows and returns empty results for
// every other method. It is separate from service_test.go's fakeStore so the
// embedding tests stand alone from the pipeline fixtures.
type fakeEmbeddingStore struct {
	embeddingRows map[string][]ShotEmbeddingRow
}

func (f *fakeEmbeddingStore) ScoreCandidates(_ context.Context, _ string, _ domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	return nil, nil
}
func (f *fakeEmbeddingStore) ScoreCandidatesV2(_ context.Context, _ string, _ domain.FacetFilter, _ domain.AssetContextFilter) ([]domain.ShotSearchResult, error) {
	return f.ScoreCandidates(context.Background(), "", domain.FacetFilter{})
}

func (f *fakeEmbeddingStore) LexicalRankedShots(_ context.Context, _ string, _ [5]float64, _ int) ([]domain.ShotSearchResult, error) {
	return nil, nil
}

func (f *fakeEmbeddingStore) TranscriptRankedShots(_ context.Context, _ string, _ int) ([]domain.ShotSearchResult, error) {
	return nil, nil
}

func (f *fakeEmbeddingStore) MetadataRankedShots(_ context.Context, _ string, _ int) ([]domain.ShotSearchResult, error) {
	return nil, nil
}

func (f *fakeEmbeddingStore) ShotTranscriptSpans(_ context.Context, _ string, _, _ int64) ([]domain.AlignmentWord, error) {
	return nil, nil
}

func (f *fakeEmbeddingStore) ShotTranscriptSpansBatch(_ context.Context, requests []TranscriptSpanRequest) (map[string][]domain.AlignmentWord, error) {
	return make(map[string][]domain.AlignmentWord, len(requests)), nil
}

func (f *fakeEmbeddingStore) NeighborShots(_ context.Context, _ string, _ int) (*domain.AssetShot, *domain.AssetShot, error) {
	return nil, nil, nil
}

func (f *fakeEmbeddingStore) NeighborShotsBatch(_ context.Context, requests []NeighborRequest) (map[string]Neighbors, error) {
	return make(map[string]Neighbors, len(requests)), nil
}

func (f *fakeEmbeddingStore) ShotSession(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (f *fakeEmbeddingStore) ShotSessions(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (f *fakeEmbeddingStore) UpsertShotTextEmbeddings(_ context.Context, _ []ShotEmbeddingRow) error {
	return nil
}

func (f *fakeEmbeddingStore) ListShotTextEmbeddings(_ context.Context, model string) ([]ShotEmbeddingRow, error) {
	return f.embeddingRows[model], nil
}

func (f *fakeEmbeddingStore) AllShotTextDocuments(_ context.Context) ([]ShotSearchDocument, error) {
	return nil, nil
}

func (f *fakeEmbeddingStore) ShotTextDocumentsByAsset(_ context.Context, _ string) ([]ShotSearchDocument, error) {
	return nil, nil
}

func (f *fakeEmbeddingStore) ShotTextEmbeddingHashes(_ context.Context, _, _ string) (map[string]string, error) {
	return nil, nil
}

func TestEmbeddingCosineDimensionMismatch(t *testing.T) {
	query := fakeVector("car crossing the street", 256)
	stored := fakeVector32("car crossing the street", 256)
	if got := embeddingCosine(query, stored); math.Abs(got-1) > 1e-9 {
		t.Fatalf("matching 256-dim vectors must give cosine 1, got %v", got)
	}
	if got := embeddingCosine(fakeVector("car", 768), fakeVector32("car", 256)); got != 0 {
		t.Fatalf("768 vs 256 mismatch must score 0, got %v", got)
	}
	if got := embeddingCosine(fakeVector("car", 768), fakeVector32("car", 1024)); got != 0 {
		t.Fatalf("768 vs 1024 mismatch must score 0, got %v", got)
	}
	if got := embeddingCosine(nil, nil); got != 0 {
		t.Fatalf("empty vectors must score 0, got %v", got)
	}
	if got := embeddingCosine(fakeVector("car", 256), nil); got != 0 {
		t.Fatalf("query vs empty vector must score 0, got %v", got)
	}
}

func TestEmbeddingRetrieverSkipsMismatchedRows(t *testing.T) {
	store := &fakeEmbeddingStore{
		embeddingRows: map[string][]ShotEmbeddingRow{
			"fake-embed-v1": {
				{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-match", Description: "car crossing the street"}}, Model: "fake-embed-v1", Vector: fakeVector32("car crossing the street", 4), SourceTextHash: "h1"},
				{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-mismatch", Description: "dog playing in the park"}}, Model: "fake-embed-v1", Vector: fakeVector32("dog playing in the park", 8), SourceTextHash: "h2"},
			},
		},
	}
	retriever := NewTextEmbeddingRetriever(store, fakeEmbedder{dim: 4})
	candidates, err := retriever.Retrieve(context.Background(), Compile("car"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("only the dimension-matching row may be scored, got %d candidates: %+v", len(candidates), candidates)
	}
	if candidates[0].ShotID != "s-match" {
		t.Fatalf("matched row must be the only scored candidate, got %+v", candidates)
	}
	if candidates[0].Signals[SignalTextEmbedding] <= 0 {
		t.Fatalf("matched row must carry a positive embedding signal, got %+v", candidates[0].Signals)
	}
	// A library holding only mismatched rows must yield nothing: the
	// mismatched vector neither distorts the cutoff nor enters the output.
	only := &fakeEmbeddingStore{
		embeddingRows: map[string][]ShotEmbeddingRow{
			"fake-embed-v1": {
				{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-mismatch", Description: "dog playing in the park"}}, Model: "fake-embed-v1", Vector: fakeVector32("dog playing in the park", 8), SourceTextHash: "h2"},
			},
		},
	}
	onlyRetriever := NewTextEmbeddingRetriever(only, fakeEmbedder{dim: 4})
	candidates, err = onlyRetriever.Retrieve(context.Background(), Compile("car"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("a mismatched-only library must yield no candidates, got %+v", candidates)
	}
}

func TestEmbeddingRetrieverExcludesInvalidScoresAndIsDeterministic(t *testing.T) {
	rows := []ShotEmbeddingRow{
		{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "z"}}, Vector: []float32{1, 0}},
		{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "a"}}, Vector: []float32{1, 0}},
		{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "negative"}}, Vector: []float32{-1, 0}},
		{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "zero"}}, Vector: []float32{0, 0}},
		{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "nan"}}, Vector: []float32{float32(math.NaN()), 1}},
		{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "inf"}}, Vector: []float32{float32(math.Inf(1)), 1}},
	}
	store := &fakeEmbeddingStore{embeddingRows: map[string][]ShotEmbeddingRow{"fake-embed-v1": rows}}
	got, err := NewTextEmbeddingRetriever(store, fixedEmbedder{vector: []float64{1, 0}}).Retrieve(context.Background(), Compile("x"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ShotID != "a" || got[1].ShotID != "z" {
		t.Fatalf("invalid or nondeterministic results: %+v", got)
	}
}

type fixedEmbedder struct{ vector []float64 }

func (f fixedEmbedder) Name() string  { return "fixed" }
func (f fixedEmbedder) Model() string { return "fake-embed-v1" }
func (f fixedEmbedder) Embed(context.Context, []string) ([][]float64, error) {
	return [][]float64{f.vector}, nil
}

func TestEmbeddingRetrieverRanksByCosine(t *testing.T) {
	store := &fakeStore{
		embeddingRows: map[string][]ShotEmbeddingRow{
			"fake-embed-v1": {
				{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-car", Description: "car crossing the street"}}, Model: "fake-embed-v1", Vector: fakeVector32("car crossing the street", 256), SourceTextHash: "h1"},
				{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-dog", Description: "dog playing in the park"}}, Model: "fake-embed-v1", Vector: fakeVector32("dog playing in the park", 256), SourceTextHash: "h2"},
				{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-empty", Description: "empty room"}}, Model: "fake-embed-v1", Vector: fakeVector32("empty room", 256), SourceTextHash: "h3"},
			},
		},
	}
	retriever := NewTextEmbeddingRetriever(store, fakeEmbedder{dim: 256})
	candidates, err := retriever.Retrieve(context.Background(), Compile("car"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) == 0 {
		t.Fatal("embedding channel must return candidates when vectors exist")
	}
	if candidates[0].ShotID != "s-car" {
		t.Fatalf("car query must rank the car shot first, got %+v", candidates)
	}
	if candidates[0].Signals[SignalTextEmbedding] <= 0 {
		t.Fatalf("embedding signal must be positive, got %+v", candidates[0].Signals)
	}
}

func TestEmbeddingRetrieverDegradesWithoutVectors(t *testing.T) {
	store := &fakeStore{} // no rows under the model
	retriever := NewTextEmbeddingRetriever(store, fakeEmbedder{dim: 256})
	candidates, err := retriever.Retrieve(context.Background(), Compile("car"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("no stored vectors must mean no candidates, got %d", len(candidates))
	}
}

func TestEmbeddingRetrieverNilEmbedderNoOp(t *testing.T) {
	store := &fakeStore{embeddingRows: map[string][]ShotEmbeddingRow{
		"x": {{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s1", Description: "anything"}}, Model: "x", Vector: fakeVector32("anything", 256)}},
	}}
	retriever := NewTextEmbeddingRetriever(store, nil)
	candidates, err := retriever.Retrieve(context.Background(), Compile("car"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("nil embedder must mean no candidates, got %d", len(candidates))
	}
}

func TestShotTextSourceAndHash(t *testing.T) {
	doc := ShotSearchDocument{ShotID: "s1", Description: "red car", Tags: []string{"traffic"}, Objects: []string{"car"}}
	source := ShotTextSource(doc)
	if source != "red car traffic car" {
		t.Fatalf("source = %q", source)
	}
	if ShotTextSourceHash(doc) != ShotTextSourceHash(doc) {
		t.Fatal("hash must be deterministic")
	}
	changed := doc
	changed.Objects = []string{"truck"}
	if ShotTextSourceHash(changed) == ShotTextSourceHash(doc) {
		t.Fatal("changed derived text must change the hash")
	}
}

// spaceRecordingStore records which space each ListShotTextEmbeddings call
// asked for, so a test can prove a read never crosses spaces.
type spaceRecordingStore struct {
	fakeEmbeddingStore
	mu      sync.Mutex
	queried []string
}

func (s *spaceRecordingStore) ListShotTextEmbeddings(ctx context.Context, space string) ([]ShotEmbeddingRow, error) {
	s.mu.Lock()
	s.queried = append(s.queried, space)
	s.mu.Unlock()
	return s.fakeEmbeddingStore.ListShotTextEmbeddings(ctx, space)
}

// identifiedFakeEmbedder is the identity-aware shape a routed provider
// reports: every batch names the space that actually served it.
type identifiedFakeEmbedder struct {
	dim   int
	space string
	fails bool
}

func (f *identifiedFakeEmbedder) Name() string  { return "identified" }
func (f *identifiedFakeEmbedder) Model() string { return "display-model" }
func (f *identifiedFakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	vectors, _, err := f.EmbedInSpace(ctx, texts)
	return vectors, err
}
func (f *identifiedFakeEmbedder) EmbeddingSpaceID() string { return f.space }
func (f *identifiedFakeEmbedder) EmbedInSpace(_ context.Context, texts []string) ([][]float64, string, error) {
	if f.fails {
		return nil, "", errors.New("primary down")
	}
	vectors := make([][]float64, len(texts))
	for i := range texts {
		vectors[i] = fakeVector(texts[i], f.dim)
	}
	return vectors, f.space, nil
}

// TestEmbedTextInSpaceChoosesTheEmbedder: an identified embedder's vectors
// carry the serving space; a plain embedder keeps its Model() space — the
// single-provider shape every library/test caller already had.
func TestEmbedTextInSpaceChoosesTheEmbedder(t *testing.T) {
	identified := &identifiedFakeEmbedder{dim: 8, space: "text-space-v1:routed"}
	vectors, space, err := embedTextInSpace(context.Background(), identified, []string{"car"})
	if err != nil || space != "text-space-v1:routed" {
		t.Fatalf("identified space=%q err=%v", space, err)
	}
	if len(vectors) != 1 {
		t.Fatalf("identified vectors=%v", vectors)
	}
	plain := fakeEmbedder{dim: 8}
	_, space, err = embedTextInSpace(context.Background(), plain, []string{"car"})
	if err != nil || space != plain.Model() {
		t.Fatalf("plain space=%q err=%v, want Model() %q", space, err, plain.Model())
	}
}

// TestEmbeddingRetrieverReadsOnlyReturnedSpace: the retriever embeds the
// query, asks for rows of the space the batch reported, and never scores
// vectors of another space — even equal-dimensional ones.
func TestEmbeddingRetrieverReadsOnlyReturnedSpace(t *testing.T) {
	store := &spaceRecordingStore{fakeEmbeddingStore: fakeEmbeddingStore{
		embeddingRows: map[string][]ShotEmbeddingRow{
			"text-space-v1:routed": {
				{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-car", Description: "car crossing the street"}}, Model: "text-space-v1:routed", Vector: fakeVector32("car crossing the street", 4), SourceTextHash: "h1"},
			},
			"text-space-v1:other": {
				{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-other", Description: "car crossing the street"}}, Model: "text-space-v1:other", Vector: fakeVector32("car crossing the street", 4), SourceTextHash: "h2"},
			},
		},
	}}
	retriever := NewTextEmbeddingRetriever(store, &identifiedFakeEmbedder{dim: 4, space: "text-space-v1:routed"})
	candidates, err := retriever.Retrieve(context.Background(), Compile("car"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ShotID != "s-car" {
		t.Fatalf("only the serving space's row may be scored, got %+v", candidates)
	}
	if len(store.queried) != 1 || store.queried[0] != "text-space-v1:routed" {
		t.Fatalf("read must name exactly the served space, got %v", store.queried)
	}
}
