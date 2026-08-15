package search

import (
	"context"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// facetUniverseStore is a minimal scripted ShotStore for the facet-universe
// test: only the channels the universe logic touches are wired; every other
// method returns its zero value because the retriever never calls them.
type facetUniverseStore struct {
	candidates []domain.ShotSearchResult
	lexical    []domain.ShotSearchResult
}

func (f *facetUniverseStore) ScoreCandidates(_ context.Context, _ string, _ domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	return f.candidates, nil
}

func (f *facetUniverseStore) LexicalRankedShots(_ context.Context, _ string, _ [5]float64, _ int) ([]domain.ShotSearchResult, error) {
	return f.lexical, nil
}

func (f *facetUniverseStore) TranscriptRankedShots(_ context.Context, _ string, _ int) ([]domain.ShotSearchResult, error) {
	return nil, nil
}

func (f *facetUniverseStore) MetadataRankedShots(_ context.Context, _ string, _ int) ([]domain.ShotSearchResult, error) {
	return nil, nil
}

func (f *facetUniverseStore) ShotTranscriptSpans(_ context.Context, _ string, _, _ int64) ([]domain.AlignmentWord, error) {
	return nil, nil
}

func (f *facetUniverseStore) ShotTranscriptSpansBatch(_ context.Context, requests []TranscriptSpanRequest) (map[string][]domain.AlignmentWord, error) {
	return make(map[string][]domain.AlignmentWord, len(requests)), nil
}

func (f *facetUniverseStore) NeighborShots(_ context.Context, _ string, _ int) (*domain.AssetShot, *domain.AssetShot, error) {
	return nil, nil, nil
}

func (f *facetUniverseStore) NeighborShotsBatch(_ context.Context, requests []NeighborRequest) (map[string]Neighbors, error) {
	return make(map[string]Neighbors, len(requests)), nil
}

func (f *facetUniverseStore) ShotSession(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (f *facetUniverseStore) ShotSessions(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (f *facetUniverseStore) UpsertShotTextEmbeddings(_ context.Context, _ []ShotEmbeddingRow) error {
	return nil
}

func (f *facetUniverseStore) ListShotTextEmbeddings(_ context.Context, _ string) ([]ShotEmbeddingRow, error) {
	return nil, nil
}

func (f *facetUniverseStore) AllShotTextDocuments(_ context.Context) ([]ShotSearchDocument, error) {
	return nil, nil
}

func (f *facetUniverseStore) ShotTextDocumentsByAsset(_ context.Context, _ string) ([]ShotSearchDocument, error) {
	return nil, nil
}

func (f *facetUniverseStore) ShotTextEmbeddingHashes(_ context.Context, _, _ string) (map[string]string, error) {
	return nil, nil
}

// facetUniverseCandidates returns the two shots of the facet-universe tests:
// shotA is facet-matched with no semantic signal (SemanticScore 0), shotB has
// a real semantic signal (0.5). Both carry a lexical score so an exact match
// can rank shotA once it survives the universe gate.
func facetUniverseCandidates() []domain.ShotSearchResult {
	return []domain.ShotSearchResult{
		{AssetShot: domain.AssetShot{ID: "shotA", AssetID: "a1"}, SemanticScore: 0, LexicalScore: 0.8},
		{AssetShot: domain.AssetShot{ID: "shotB", AssetID: "a1"}, SemanticScore: 0.5, LexicalScore: 0.3},
	}
}

// TestHeuristicSemanticRetrieverFacetUniverseRetainsSemanticZero pins the
// facet-universe rule: with facets set, a semantic-0 row stays a candidate
// (universe membership) with an explicit Signal value 0 — present, not absent
// — so the service's universe includes it and fusion treats it consistently.
func TestHeuristicSemanticRetrieverFacetUniverseRetainsSemanticZero(t *testing.T) {
	store := &facetUniverseStore{candidates: facetUniverseCandidates()}
	retriever := NewHeuristicSemanticRetriever(store)
	q := SearchQuery{
		Raw:     "bmp",
		Filters: SearchFilters{Facets: domain.FacetFilter{AssetTypes: []string{"bmp"}}},
	}
	got, err := retriever.Retrieve(context.Background(), q, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("facets set: want 2 candidates, got %d", len(got))
	}
	byID := make(map[string]Candidate, len(got))
	for _, c := range got {
		byID[c.ShotID] = c
	}
	shotA, ok := byID["shotA"]
	if !ok {
		t.Fatal("facets set: semantic-0 shotA must stay in the candidate universe")
	}
	if value, present := shotA.Signals[SignalHeuristicSemantic]; !present {
		t.Fatal("facets set: shotA must carry the heuristic_semantic signal key (explicit 0, not absent)")
	} else if value != 0 {
		t.Fatalf("facets set: shotA signal = %v, want 0 (no fabricated semantic score)", value)
	}
	shotB, ok := byID["shotB"]
	if !ok {
		t.Fatal("facets set: shotB must be retained")
	}
	if shotB.Signals[SignalHeuristicSemantic] != 0.5 {
		t.Fatalf("facets set: shotB signal = %v, want 0.5", shotB.Signals[SignalHeuristicSemantic])
	}
}

// TestHeuristicSemanticRetrieverNoFacetsDropsSemanticZero pins the unchanged
// non-facet gate: without facets the semantic>0 filter still drops shotA.
func TestHeuristicSemanticRetrieverNoFacetsDropsSemanticZero(t *testing.T) {
	store := &facetUniverseStore{candidates: facetUniverseCandidates()}
	retriever := NewHeuristicSemanticRetriever(store)
	got, err := retriever.Retrieve(context.Background(), SearchQuery{Raw: "bmp"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("no facets: want 1 candidate, got %d", len(got))
	}
	if got[0].ShotID != "shotB" {
		t.Fatalf("no facets: kept %q, want shotB (semantic 0 must be dropped)", got[0].ShotID)
	}
	if got[0].Signals[SignalHeuristicSemantic] != 0.5 {
		t.Fatalf("no facets: shotB signal = %v, want 0.5", got[0].Signals[SignalHeuristicSemantic])
	}
}

// TestFacetUniverseServiceSurvivesLexicalOnlyShotEndToEnd proves the universe
// contract end to end: with facets set, shotA (semantic 0, lexical 0.8)
// survives the semantic channel and, being in the universe, is not dropped by
// the service's universe gate, so an exact lexical match under a facet ranks.
// The fact gate is off — the universe gate, not evidence, is what this pins.
func TestFacetUniverseServiceSurvivesLexicalOnlyShotEndToEnd(t *testing.T) {
	store := &facetUniverseStore{
		candidates: facetUniverseCandidates(),
		lexical:    facetUniverseCandidates(),
	}
	opts := DefaultOptions()
	opts.GateFact = false
	svc := NewService(store, opts)
	response, err := svc.Search(context.Background(), SearchRequest{
		Query: "bmp",
		Limit: 5,
		Facets: domain.FacetFilter{
			AssetTypes: []string{"bmp"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	foundA := false
	for _, item := range response.Results {
		if item.ShotID == "shotA" {
			foundA = true
			if value, present := item.Scores[SignalHeuristicSemantic]; !present || value != 0 {
				t.Fatalf("shotA heuristic_semantic score = %v (present=%v), want explicit 0", value, present)
			}
		}
	}
	if !foundA {
		t.Fatalf("facets set: exact lexical match shotA must surface in results, got %+v", response.Results)
	}
}
