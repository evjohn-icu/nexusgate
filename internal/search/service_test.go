package search

import (
	"context"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// fakeStore is a scripted ShotStore for pipeline tests. It holds the legacy
// candidate universe plus per-channel results; every method is deterministic.
type fakeStore struct {
	candidates    []domain.ShotSearchResult
	lexical       []domain.ShotSearchResult
	transcript    []domain.ShotSearchResult
	metadata      []domain.ShotSearchResult
	spansByID     map[string][]domain.AlignmentWord
	sessions      map[string]string
	shotsByID     map[string]domain.AssetShot
	embeddingRows map[string][]ShotEmbeddingRow
}

func (f *fakeStore) ScoreCandidates(_ context.Context, _ string, _ domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	return f.candidates, nil
}

func (f *fakeStore) LexicalRankedShots(_ context.Context, _ string, _ [5]float64, _ int) ([]domain.ShotSearchResult, error) {
	return f.lexical, nil
}

func (f *fakeStore) TranscriptRankedShots(_ context.Context, _ string, _ int) ([]domain.ShotSearchResult, error) {
	return f.transcript, nil
}

func (f *fakeStore) MetadataRankedShots(_ context.Context, _ string, _ int) ([]domain.ShotSearchResult, error) {
	return f.metadata, nil
}

func (f *fakeStore) ShotTranscriptSpans(_ context.Context, assetID string, startMS, endMS int64) ([]domain.AlignmentWord, error) {
	if f.spansByID != nil {
		if spans, ok := f.spansByID[assetID+"|"+itoa(startMS)+"|"+itoa(endMS)]; ok {
			return spans, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) NeighborShots(_ context.Context, assetID string, ordinal int) (*domain.AssetShot, *domain.AssetShot, error) {
	if f.shotsByID == nil {
		return nil, nil, nil
	}
	var prev, next *domain.AssetShot
	for _, shot := range f.shotsByID {
		if shot.AssetID != assetID {
			continue
		}
		if shot.Ordinal < ordinal && (prev == nil || shot.Ordinal > prev.Ordinal) {
			copy := shot
			prev = &copy
		}
		if shot.Ordinal > ordinal && (next == nil || shot.Ordinal < next.Ordinal) {
			copy := shot
			next = &copy
		}
	}
	return prev, next, nil
}

func (f *fakeStore) ShotSession(_ context.Context, assetID string) (string, error) {
	return f.sessions[assetID], nil
}

func (f *fakeStore) UpsertShotTextEmbeddings(_ context.Context, _ []ShotEmbeddingRow) error {
	return nil
}

func (f *fakeStore) ListShotTextEmbeddings(_ context.Context, model string) ([]ShotEmbeddingRow, error) {
	return f.embeddingRows[model], nil
}

func (f *fakeStore) AllShotTextDocuments(_ context.Context) ([]ShotSearchDocument, error) {
	return nil, nil
}

func (f *fakeStore) ShotTextDocumentsByAsset(_ context.Context, _ string) ([]ShotSearchDocument, error) {
	return nil, nil
}

func (f *fakeStore) ShotTextEmbeddingHashes(_ context.Context, _, _ string) (map[string]string, error) {
	return nil, nil
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	negative := v < 0
	if negative {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func shot(id, assetID string, startMS, endMS int64, objects []string, description string) domain.ShotSearchResult {
	return domain.ShotSearchResult{
		AssetShot: domain.AssetShot{
			ID: id, AssetID: assetID, Ordinal: 0, StartMS: startMS, EndMS: endMS,
			Description: description, Objects: objects,
		},
		SemanticScore: 0.8,
		LexicalScore:  0.7,
	}
}

func TestSearchV2ResponseShape(t *testing.T) {
	store := &fakeStore{
		candidates: []domain.ShotSearchResult{shot("s1", "a1", 10_000, 12_000, []string{"car"}, "red car crossing")},
		lexical:    []domain.ShotSearchResult{shot("s1", "a1", 10_000, 12_000, []string{"car"}, "red car crossing")},
		sessions:   map[string]string{"a1": "session-1"},
	}
	svc := NewService(store, DefaultOptions())
	response, err := svc.Search(context.Background(), SearchRequest{Query: "汽车经过街道", IncludeEvidence: true, IncludeContext: true, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if response.Query.Intent != IntentFact {
		t.Fatalf("intent = %s, want fact", response.Query.Intent)
	}
	if len(response.Results) != 1 {
		t.Fatalf("want 1 result, got %d", len(response.Results))
	}
	item := response.Results[0]
	if item.ShotID != "s1" || item.Rank != 1 || item.AssetID != "a1" {
		t.Fatalf("bad result item: %+v", item)
	}
	if item.Scores[SignalRRF] != item.Score {
		t.Fatalf("rrf score must equal fused score: %+v", item.Scores)
	}
	if len(item.Evidence) == 0 {
		t.Fatal("evidence must be present when include_evidence is set")
	}
	if item.Context == nil {
		t.Fatal("context must be present when include_context is set")
	}
	if response.SearchID == "" || len(response.QueryHash) != 16 {
		t.Fatalf("search_id=%q query_hash=%q", response.SearchID, response.QueryHash)
	}
}

func TestSearchV2EmptyQuery(t *testing.T) {
	svc := NewService(&fakeStore{}, DefaultOptions())
	response, err := svc.Search(context.Background(), SearchRequest{Query: ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 0 {
		t.Fatalf("empty query must return no results, got %d", len(response.Results))
	}
}

func TestSearchV2UnknownMode(t *testing.T) {
	svc := NewService(&fakeStore{}, DefaultOptions())
	if _, err := svc.Search(context.Background(), SearchRequest{Query: "car", Mode: "bogus"}); err == nil {
		t.Fatal("unknown mode must error")
	}
}

func TestSearchV2GateDropsFactShotWithoutEvidence(t *testing.T) {
	store := &fakeStore{
		candidates: []domain.ShotSearchResult{
			shot("s1", "a1", 0, 10_000, []string{"car"}, "red car crossing"),
			shot("s2", "a2", 0, 10_000, nil, "busy downtown transportation scene"),
		},
		lexical: []domain.ShotSearchResult{
			shot("s1", "a1", 0, 10_000, []string{"car"}, "red car crossing"),
			shot("s2", "a2", 0, 10_000, nil, "busy downtown transportation scene"),
		},
	}
	svc := NewService(store, DefaultOptions())
	response, err := svc.Search(context.Background(), SearchRequest{Query: "汽车经过街道", Limit: 5, IncludeEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].ShotID != "s1" {
		t.Fatalf("gate must drop the unconfirmed semantic-only shot, got %+v", response.Results)
	}
}

func TestSearchV2GateOffKeepsUnconfirmed(t *testing.T) {
	store := &fakeStore{
		candidates: []domain.ShotSearchResult{
			shot("s1", "a1", 0, 10_000, []string{"car"}, "red car crossing"),
			shot("s2", "a2", 0, 10_000, nil, "busy downtown transportation scene"),
		},
		lexical: []domain.ShotSearchResult{
			shot("s1", "a1", 0, 10_000, []string{"car"}, "red car crossing"),
			shot("s2", "a2", 0, 10_000, nil, "busy downtown transportation scene"),
		},
	}
	opts := DefaultOptions()
	opts.GateFact = false
	svc := NewService(store, opts)
	response, err := svc.Search(context.Background(), SearchRequest{Query: "汽车经过街道", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 2 {
		t.Fatalf("gate off must keep both candidates, got %d", len(response.Results))
	}
}

func TestSearchV2NegativeQueryExcludesObserved(t *testing.T) {
	store := &fakeStore{
		candidates: []domain.ShotSearchResult{
			shot("s1", "a1", 0, 10_000, []string{"ocean"}, "empty beach no people"),
			shot("s2", "a2", 0, 10_000, []string{"person"}, "person walking on beach"),
		},
		lexical: []domain.ShotSearchResult{
			shot("s1", "a1", 0, 10_000, []string{"ocean"}, "empty beach no people"),
			shot("s2", "a2", 0, 10_000, []string{"person"}, "person walking on beach"),
		},
	}
	svc := NewService(store, DefaultOptions())
	response, err := svc.Search(context.Background(), SearchRequest{Query: "没有人的海边空镜", Limit: 5, IncludeEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].ShotID != "s1" {
		t.Fatalf("must-not person must exclude the observed shot, got %+v", response.Results)
	}
	// The kept shot's person evidence must be unknown (never "确认无人").
	for _, e := range response.Results[0].Evidence {
		if e.Value == "person" && e.Negated && e.State != EvidenceUnknown {
			t.Fatalf("kept shot must report person as unknown, got %s", e.State)
		}
	}
}

func TestLegacySearchMatchesContract(t *testing.T) {
	store := &fakeStore{
		candidates: []domain.ShotSearchResult{
			shot("s1", "a1", 0, 10_000, []string{"car"}, "red car crossing"),
			shot("s2", "a2", 0, 10_000, nil, "busy downtown transportation scene"),
		},
	}
	svc := NewService(store, DefaultOptions())
	results, err := svc.LegacySearch(context.Background(), "car", 10, domain.FacetFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("legacy search must return all candidates with Score>0, got %d", len(results))
	}
	// 0.70*0.8 + 0.30*0.7 = 0.77 for both.
	if diff(results[0].Score, 0.77) > 1e-12 {
		t.Fatalf("legacy blend score = %v, want 0.77", results[0].Score)
	}
}
