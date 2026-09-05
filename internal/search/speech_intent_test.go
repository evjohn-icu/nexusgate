package search

import (
	"context"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// recordingTranscriptStore is a minimal ShotStore that records the last query
// string handed to TranscriptRankedShots, so the transcript channel's match
// string is observable without touching the real SQLite store.
type recordingTranscriptStore struct {
	lastTranscriptQuery string
	transcript          []domain.ShotSearchResult
}

func (f *recordingTranscriptStore) ScoreCandidates(_ context.Context, _ string, _ domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	return nil, nil
}
func (f *recordingTranscriptStore) ScoreCandidatesV2(_ context.Context, _ string, _ domain.FacetFilter, _ domain.AssetContextFilter) ([]domain.ShotSearchResult, error) {
	return f.ScoreCandidates(context.Background(), "", domain.FacetFilter{})
}

func (f *recordingTranscriptStore) LexicalRankedShots(_ context.Context, _ string, _ [5]float64, _ int) ([]domain.ShotSearchResult, error) {
	return nil, nil
}

func (f *recordingTranscriptStore) TranscriptRankedShots(_ context.Context, q string, _ int) ([]domain.ShotSearchResult, error) {
	f.lastTranscriptQuery = q
	return f.transcript, nil
}

func (f *recordingTranscriptStore) MetadataRankedShots(_ context.Context, _ string, _ int) ([]domain.ShotSearchResult, error) {
	return nil, nil
}

func (f *recordingTranscriptStore) ShotTranscriptSpans(_ context.Context, _ string, _, _ int64) ([]domain.AlignmentWord, error) {
	return nil, nil
}

func (f *recordingTranscriptStore) ShotTranscriptSpansBatch(_ context.Context, requests []TranscriptSpanRequest) (map[string][]domain.AlignmentWord, error) {
	return make(map[string][]domain.AlignmentWord, len(requests)), nil
}

func (f *recordingTranscriptStore) NeighborShots(_ context.Context, _ string, _ int) (*domain.AssetShot, *domain.AssetShot, error) {
	return nil, nil, nil
}

func (f *recordingTranscriptStore) NeighborShotsBatch(_ context.Context, requests []NeighborRequest) (map[string]Neighbors, error) {
	return make(map[string]Neighbors, len(requests)), nil
}

func (f *recordingTranscriptStore) ShotSession(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (f *recordingTranscriptStore) ShotSessions(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (f *recordingTranscriptStore) UpsertShotTextEmbeddings(_ context.Context, _ []ShotEmbeddingRow) error {
	return nil
}

func (f *recordingTranscriptStore) ListShotTextEmbeddings(_ context.Context, _ string) ([]ShotEmbeddingRow, error) {
	return nil, nil
}

func (f *recordingTranscriptStore) AllShotTextDocuments(_ context.Context) ([]ShotSearchDocument, error) {
	return nil, nil
}

func (f *recordingTranscriptStore) ShotTextDocumentsByAsset(_ context.Context, _ string) ([]ShotSearchDocument, error) {
	return nil, nil
}

func (f *recordingTranscriptStore) ShotTextEmbeddingHashes(_ context.Context, _, _ string) (map[string]string, error) {
	return nil, nil
}

func transcriptShot(id string, startMS, endMS int64) domain.ShotSearchResult {
	return domain.ShotSearchResult{
		AssetShot:       domain.AssetShot{ID: id, AssetID: "a1", StartMS: startMS, EndMS: endMS},
		TranscriptScore: 0.9,
	}
}

// TestTranscriptRetrieverPrefersSpeechPhrase pins the A2 speech-intent flow:
// when the compiler has extracted a speech phrase, the transcript channel must
// match the phrase (我们明天出发) and never the raw query with its marker words
// (谁说过“我们明天出发”), which would pollute the token match.
func TestTranscriptRetrieverPrefersSpeechPhrase(t *testing.T) {
	store := &recordingTranscriptStore{
		transcript: []domain.ShotSearchResult{transcriptShot("s1", 10_000, 12_000)},
	}
	r := NewTranscriptRetriever(store)
	candidates, err := r.Retrieve(context.Background(), SearchQuery{
		Raw:          "谁说过“我们明天出发”",
		SpeechPhrase: "我们明天出发",
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if store.lastTranscriptQuery != "我们明天出发" {
		t.Fatalf("transcript channel matched %q, want the compiled phrase %q", store.lastTranscriptQuery, "我们明天出发")
	}
	if len(candidates) != 1 || candidates[0].ShotID != "s1" {
		t.Fatalf("want 1 transcript candidate s1, got %+v", candidates)
	}
}

// TestTranscriptRetrieverFallsBackToRaw pins backward compatibility: without a
// compiled phrase the channel must pass the raw query through unchanged, so
// non-speech queries (and the golden set) keep their current behavior.
func TestTranscriptRetrieverFallsBackToRaw(t *testing.T) {
	store := &recordingTranscriptStore{
		transcript: []domain.ShotSearchResult{transcriptShot("s1", 10_000, 12_000)},
	}
	r := NewTranscriptRetriever(store)
	if _, err := r.Retrieve(context.Background(), SearchQuery{Raw: "她说的话很重要"}, 10); err != nil {
		t.Fatal(err)
	}
	if store.lastTranscriptQuery != "她说的话很重要" {
		t.Fatalf("transcript channel matched %q, want the raw query %q", store.lastTranscriptQuery, "她说的话很重要")
	}
}
