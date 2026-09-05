package search

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// similarFakeStore is a scripted ShotStore for the similar-shots tests. It
// implements the full current interface (including ShotSessions, which joined
// mid-wave: assets with no shoot session are simply absent from the map, so a
// scripted empty map is the honest stand-in) but only the methods the two
// Similar* paths touch are scripted; the rest are deterministic no-ops.
type similarFakeStore struct {
	candidates    []domain.ShotSearchResult
	embeddingRows map[string][]ShotEmbeddingRow
	sessions      map[string]string
}

func (f *similarFakeStore) ScoreCandidates(_ context.Context, _ string, _ domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	return f.candidates, nil
}
func (f *similarFakeStore) ScoreCandidatesV2(_ context.Context, _ string, _ domain.FacetFilter, _ domain.AssetContextFilter) ([]domain.ShotSearchResult, error) {
	return f.ScoreCandidates(context.Background(), "", domain.FacetFilter{})
}

func (f *similarFakeStore) LexicalRankedShots(_ context.Context, _ string, _ [5]float64, _ int) ([]domain.ShotSearchResult, error) {
	return nil, nil
}

func (f *similarFakeStore) TranscriptRankedShots(_ context.Context, _ string, _ int) ([]domain.ShotSearchResult, error) {
	return nil, nil
}

func (f *similarFakeStore) MetadataRankedShots(_ context.Context, _ string, _ int) ([]domain.ShotSearchResult, error) {
	return nil, nil
}

func (f *similarFakeStore) ShotTranscriptSpans(_ context.Context, _ string, _, _ int64) ([]domain.AlignmentWord, error) {
	return nil, nil
}

func (f *similarFakeStore) ShotTranscriptSpansBatch(_ context.Context, requests []TranscriptSpanRequest) (map[string][]domain.AlignmentWord, error) {
	return make(map[string][]domain.AlignmentWord, len(requests)), nil
}

func (f *similarFakeStore) NeighborShots(_ context.Context, _ string, _ int) (*domain.AssetShot, *domain.AssetShot, error) {
	return nil, nil, nil
}

func (f *similarFakeStore) NeighborShotsBatch(_ context.Context, requests []NeighborRequest) (map[string]Neighbors, error) {
	return make(map[string]Neighbors, len(requests)), nil
}

func (f *similarFakeStore) ShotSession(_ context.Context, assetID string) (string, error) {
	return f.sessions[assetID], nil
}

func (f *similarFakeStore) ShotSessions(_ context.Context, _ []string) (map[string]string, error) {
	return f.sessions, nil
}

func (f *similarFakeStore) UpsertShotTextEmbeddings(_ context.Context, _ []ShotEmbeddingRow) error {
	return nil
}

func (f *similarFakeStore) ListShotTextEmbeddings(_ context.Context, model string) ([]ShotEmbeddingRow, error) {
	return f.embeddingRows[model], nil
}

func (f *similarFakeStore) AllShotTextDocuments(_ context.Context) ([]ShotSearchDocument, error) {
	return nil, nil
}

func (f *similarFakeStore) ShotTextDocumentsByAsset(_ context.Context, _ string) ([]ShotSearchDocument, error) {
	return nil, nil
}

func (f *similarFakeStore) ShotTextEmbeddingHashes(_ context.Context, _, _ string) (map[string]string, error) {
	return nil, nil
}

// similarShot builds a scored shot row; SemanticScore is the only field the
// heuristic path ranks on.
func similarShot(id, assetID string, semanticScore float64) domain.ShotSearchResult {
	return domain.ShotSearchResult{
		AssetShot:     domain.AssetShot{ID: id, AssetID: assetID, Ordinal: 0, StartMS: 0, EndMS: 10_000},
		SemanticScore: semanticScore,
	}
}

// similarEmbeddingRows scripts one embedding row per shot text under the fake
// model, exactly like the embedding channel's own tests.
func similarEmbeddingRows() map[string][]ShotEmbeddingRow {
	return map[string][]ShotEmbeddingRow{
		"fake-embed-v1": {
			{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-car", AssetID: "a1", Ordinal: 0, StartMS: 0, EndMS: 10_000, Description: "car crossing the street"}}, Model: "fake-embed-v1", Vector: fakeVector32("car crossing the street", 256), SourceTextHash: "h1"},
			{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-dog", AssetID: "a2", Ordinal: 0, StartMS: 0, EndMS: 10_000, Description: "dog playing in the park"}}, Model: "fake-embed-v1", Vector: fakeVector32("dog playing in the park", 256), SourceTextHash: "h2"},
			{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-empty", AssetID: "a3", Ordinal: 0, StartMS: 0, EndMS: 10_000, Description: "empty room"}}, Model: "fake-embed-v1", Vector: fakeVector32("empty room", 256), SourceTextHash: "h3"},
		},
	}
}

func TestSimilarByTextRanksTopNByCosine(t *testing.T) {
	store := &similarFakeStore{embeddingRows: similarEmbeddingRows()}
	opts := DefaultOptions()
	opts.Embedder = fakeEmbedder{dim: 256}
	svc := NewService(store, opts)

	candidates, err := svc.SimilarByText(context.Background(), "car", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("only positive cosine candidates must be returned, got %d", len(candidates))
	}
	if candidates[0].ShotID != "s-car" {
		t.Fatalf("car query must rank the car shot first, got %+v", candidates)
	}
	want := embeddingCosine(fakeVector("car", 256), fakeVector32("car crossing the street", 256))
	if candidates[0].Score != want {
		t.Fatalf("Score = %v, want cosine %v", candidates[0].Score, want)
	}
	for i, candidate := range candidates {
		signal, ok := candidate.Signals[SignalTextEmbedding]
		if !ok {
			t.Fatalf("candidate %d must carry the text_embedding signal, got %+v", i, candidate.Signals)
		}
		if signal != candidate.Score {
			t.Fatalf("candidate %d signal %v != Score %v", i, signal, candidate.Score)
		}
		if i > 0 && candidates[i-1].Score < candidate.Score {
			t.Fatalf("candidates must be score-descending: %+v", candidates)
		}
	}
}

func TestSimilarByTextExcludesInvalidScoresAndIsDeterministic(t *testing.T) {
	rows := similarEmbeddingRows()["fake-embed-v1"]
	invalid := make([]float32, 256)
	rows = append(rows,
		ShotEmbeddingRow{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "negative"}}, Vector: func() []float32 { v := append([]float32(nil), invalid...); v[0] = -1; return v }()},
		ShotEmbeddingRow{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "zero"}}, Vector: invalid},
		ShotEmbeddingRow{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "nan"}}, Vector: func() []float32 { v := append([]float32(nil), invalid...); v[0] = float32(math.NaN()); return v }()},
	)
	store := &similarFakeStore{embeddingRows: map[string][]ShotEmbeddingRow{"fake-embed-v1": rows}}
	opts := DefaultOptions()
	opts.Embedder = fixedEmbedder{vector: fakeVector("car", 256)}
	got, err := NewService(store, opts).SimilarByText(context.Background(), "x", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range got {
		if candidate.ShotID == "negative" || candidate.ShotID == "zero" || candidate.ShotID == "nan" {
			t.Fatalf("invalid candidate returned: %+v", candidate)
		}
	}
}

func TestSimilarByTextRejectsInvalidQuery(t *testing.T) {
	opts := DefaultOptions()
	opts.Embedder = fixedEmbedder{vector: []float64{0, 0}}
	_, err := NewService(&similarFakeStore{embeddingRows: similarEmbeddingRows()}, opts).SimilarByText(context.Background(), "x", 10)
	if err == nil {
		t.Fatal("zero query vector must be rejected")
	}
}

func TestSimilarByTextNilEmbedderReturnsSentinel(t *testing.T) {
	svc := NewService(&similarFakeStore{embeddingRows: similarEmbeddingRows()}, DefaultOptions())
	_, err := svc.SimilarByText(context.Background(), "car", 10)
	if !errors.Is(err, ErrNoEmbeddingSearch) {
		t.Fatalf("nil embedder must return ErrNoEmbeddingSearch, got %v", err)
	}
}

func TestSimilarByTextNoRowsReturnsSentinel(t *testing.T) {
	store := &similarFakeStore{} // no vectors under the model
	opts := DefaultOptions()
	opts.Embedder = fakeEmbedder{dim: 256}
	svc := NewService(store, opts)
	_, err := svc.SimilarByText(context.Background(), "car", 10)
	if !errors.Is(err, ErrNoEmbeddingSearch) {
		t.Fatalf("empty vector library must return ErrNoEmbeddingSearch, got %v", err)
	}
}

func TestSimilarByTextDefaultLimit(t *testing.T) {
	store := &similarFakeStore{embeddingRows: similarEmbeddingRows()}
	opts := DefaultOptions()
	opts.Embedder = fakeEmbedder{dim: 256}
	svc := NewService(store, opts)
	candidates, err := svc.SimilarByText(context.Background(), "car", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("only positive cosine candidates must be returned, got %d results", len(candidates))
	}
}

func TestSimilarByHeuristicRanksBySemanticScore(t *testing.T) {
	store := &similarFakeStore{candidates: []domain.ShotSearchResult{
		similarShot("s-mid", "a1", 0.5),
		similarShot("s-top", "a2", 0.9),
		similarShot("s-dead", "a3", 0.0),
		similarShot("s-low", "a4", 0.2),
	}}
	svc := NewService(store, DefaultOptions())

	candidates, err := svc.SimilarByHeuristic(context.Background(), "car", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("limit 2 must return 2 candidates (zero-score shot dropped), got %d", len(candidates))
	}
	if candidates[0].ShotID != "s-top" || candidates[1].ShotID != "s-mid" {
		t.Fatalf("must rank by SemanticScore descending, got %+v", candidates)
	}
	for _, candidate := range candidates {
		signal, ok := candidate.Signals[SignalHeuristicSemantic]
		if !ok {
			t.Fatalf("candidate must carry the heuristic_semantic signal, got %+v", candidate.Signals)
		}
		if signal != candidate.Score {
			t.Fatalf("signal %v != Score %v", signal, candidate.Score)
		}
	}
}

// TestSimilarFallbackFlow documents the integrator's wiring contract: try the
// NN path, and on ErrNoEmbeddingSearch — not on any error — fall back to the
// heuristic vectors.
func TestSimilarFallbackFlow(t *testing.T) {
	store := &similarFakeStore{
		embeddingRows: map[string][]ShotEmbeddingRow{}, // no vectors -> sentinel
		candidates:    []domain.ShotSearchResult{similarShot("s-fallback", "a1", 0.7)},
	}
	opts := DefaultOptions()
	opts.Embedder = fakeEmbedder{dim: 256}
	svc := NewService(store, opts)

	if _, err := svc.SimilarByText(context.Background(), "car", 10); !errors.Is(err, ErrNoEmbeddingSearch) {
		t.Fatalf("want ErrNoEmbeddingSearch to trigger the fallback, got %v", err)
	}
	fallback, err := svc.SimilarByHeuristic(context.Background(), "car", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fallback) != 1 || fallback[0].ShotID != "s-fallback" {
		t.Fatalf("heuristic fallback must serve the ScoreCandidates universe, got %+v", fallback)
	}
	if fallback[0].Score != 0.7 {
		t.Fatalf("fallback Score = %v, want SemanticScore 0.7", fallback[0].Score)
	}
}
