package search

import (
	"context"
	"reflect"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

type testModalityStore struct {
	universe     map[string]bool
	visual       []ModalityWindow
	audio        []ModalityWindow
	soundEvents  []ModalityWindow
	soundByLabel map[string][]ModalityWindow
	speakerTurns []ModalityWindow
	gotSpace     string
	gotVector    []float32
	gotLabels    []string
	gotSpeaker   SpeakerFilter
	gotUniverse  map[string]bool
	gotLimit     int
}

func (s *testModalityStore) FilteredShotUniverse(_ context.Context, _ domain.FacetFilter, _ domain.AssetContextFilter) (map[string]bool, error) {
	return s.universe, nil
}

func (s *testModalityStore) VisualWindows(_ context.Context, spaceID string, vector []float32, universe map[string]bool, limit int) ([]ModalityWindow, error) {
	s.gotSpace, s.gotVector, s.gotUniverse, s.gotLimit = spaceID, vector, universe, limit
	return s.visual, nil
}

func (s *testModalityStore) AudioWindows(_ context.Context, spaceID string, vector []float32, universe map[string]bool, limit int) ([]ModalityWindow, error) {
	s.gotSpace, s.gotVector, s.gotUniverse, s.gotLimit = spaceID, vector, universe, limit
	return s.audio, nil
}

func (s *testModalityStore) SoundEvents(_ context.Context, labels []string, universe map[string]bool, limit int) ([]ModalityWindow, error) {
	s.gotLabels, s.gotUniverse, s.gotLimit = labels, universe, limit
	if s.soundByLabel != nil {
		for _, label := range labels {
			if windows, ok := s.soundByLabel[label]; ok {
				return windows, nil
			}
		}
		return nil, nil
	}
	return s.soundEvents, nil
}

func (s *testModalityStore) SpeakerTurns(_ context.Context, filter SpeakerFilter, universe map[string]bool, limit int) ([]ModalityWindow, error) {
	s.gotSpeaker, s.gotUniverse, s.gotLimit = filter, universe, limit
	return s.speakerTurns, nil
}

func TestMultimodalRetrieverDeduplicatesShotsAndPreservesWindows(t *testing.T) {
	store := &testModalityStore{
		universe: map[string]bool{"shot-a": true, "shot-b": true},
		visual: []ModalityWindow{
			{AssetID: "asset-a", ShotID: "shot-a", RecordID: "frame-1", StartMS: 0, EndMS: 100, Score: 0.4},
			{AssetID: "asset-a", ShotID: "shot-a", RecordID: "frame-2", StartMS: 100, EndMS: 200, Score: 0.9},
			{AssetID: "asset-b", ShotID: "shot-b", RecordID: "frame-3", StartMS: 300, EndMS: 400, Score: 0.7},
		},
	}
	retriever := NewModalityRetriever(store, ModalityVisual, "siglip-space", []float32{0.25, 0.75})
	candidates, windows, truncated, err := retriever.RetrieveWithWindows(context.Background(), SearchQuery{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("a pool no larger than the limit must not report truncation")
	}
	if len(candidates) != 2 || candidates[0].ShotID != "shot-a" || candidates[1].ShotID != "shot-b" {
		t.Fatalf("deduplicated candidates = %+v, want shot-a then shot-b", candidates)
	}
	if candidates[0].Signals[SignalVisual] != 0.9 || candidates[0].Score != 0.9 || candidates[0].StartMS != 100 || candidates[0].EndMS != 200 {
		t.Fatalf("candidate did not use maximum-score window: %+v", candidates[0])
	}
	if len(windows) != 3 {
		t.Fatalf("retained matching windows = %d, want all 3 across deduplicated shots: %+v", len(windows), windows)
	}
	if windows[0].RecordID != "frame-1" || windows[1].RecordID != "frame-2" || windows[2].RecordID != "frame-3" {
		t.Fatalf("windows not deterministically ordered: %+v", windows)
	}
	if store.gotSpace != "siglip-space" || !reflect.DeepEqual(store.gotVector, []float32{0.25, 0.75}) || store.gotUniverse["shot-a"] != true || store.gotLimit != 4 {
		t.Fatalf("visual store call args space=%q vector=%v universe=%v limit=%d", store.gotSpace, store.gotVector, store.gotUniverse, store.gotLimit)
	}
	if got := retriever.Name(); got != SignalVisual {
		t.Fatalf("Name() = %q, want %q", got, SignalVisual)
	}
}

func TestMultimodalRetrieverSoundLabelsAndSpeakerFilter(t *testing.T) {
	store := &testModalityStore{
		universe:     map[string]bool{"shot": true},
		soundEvents:  []ModalityWindow{{AssetID: "asset", ShotID: "shot", RecordID: "rain-1", StartMS: 0, EndMS: 200, Score: 0.8}},
		speakerTurns: []ModalityWindow{{AssetID: "asset", ShotID: "shot", RecordID: "turn-1", StartMS: 50, EndMS: 100, Score: 0.6}},
	}
	sound := NewModalityRetriever(store, ModalitySoundEvent, "", nil)
	candidates, _, _, err := sound.RetrieveWithWindows(context.Background(), SearchQuery{Raw: "horn before rain"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || !reflect.DeepEqual(store.gotLabels, []string{"/m/06mb1", "/m/0912c9"}) {
		t.Fatalf("sound retrieval candidates=%v labels=%v", candidates, store.gotLabels)
	}
	speakerFilter := SpeakerFilter{AssetID: "asset", DiarizationGenerationID: "generation", LocalSpeakerID: "speaker-1"}
	speaker := &ModalityRetriever{Store: store, Modality: ModalitySpeaker, SpeakerFilter: speakerFilter}
	candidates, _, _, err = speaker.RetrieveWithWindows(context.Background(), SearchQuery{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || store.gotSpeaker != speakerFilter || candidates[0].Signals[SignalSpeaker] != 0.6 {
		t.Fatalf("speaker retrieval candidates=%+v filter=%+v", candidates, store.gotSpeaker)
	}
}

func TestMultimodalRetrieverRejectsUnconfiguredDenseChannels(t *testing.T) {
	store := &testModalityStore{}
	for _, modality := range []string{ModalityVisual, ModalityAudio} {
		t.Run(modality, func(t *testing.T) {
			r := &ModalityRetriever{Store: store, Modality: modality}
			if _, err := r.Retrieve(context.Background(), SearchQuery{}, 1); err == nil {
				t.Fatal("dense modality retrieval accepted a missing space/vector")
			}
		})
	}
}

func TestMultimodalRetrieverReportsTruncatedStoragePool(t *testing.T) {
	store := &testModalityStore{
		universe: map[string]bool{"shot-a": true},
		visual: []ModalityWindow{
			{AssetID: "asset-a", ShotID: "shot-a", RecordID: "frame-1", StartMS: 0, EndMS: 100, Score: 0.9},
			{AssetID: "asset-a", ShotID: "shot-a", RecordID: "frame-2", StartMS: 100, EndMS: 200, Score: 0.8},
			{AssetID: "asset-a", ShotID: "shot-a", RecordID: "frame-3", StartMS: 200, EndMS: 300, Score: 0.7},
		},
	}
	retriever := NewModalityRetriever(store, ModalityVisual, "space", []float32{1})
	candidates, windows, truncated, err := retriever.RetrieveWithWindows(context.Background(), SearchQuery{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	// The store returned the probe row (3 rows for limit 2): the pool was
	// cut even though one shot occupies every row. The probe row is
	// discarded and the candidate count alone never decides coverage.
	if !truncated {
		t.Fatal("a full probe row must report storage truncation")
	}
	if len(candidates) != 1 || len(windows) != 2 {
		t.Fatalf("candidates=%d windows=%d, want the probe row discarded", len(candidates), len(windows))
	}
	if windows[0].RecordID != "frame-1" || windows[1].RecordID != "frame-2" {
		t.Fatalf("probe row not the discarded tail: %+v", windows)
	}
	store2 := &testModalityStore{visual: windows[:2]}
	store2.universe = map[string]bool{"shot-a": true}
	retriever2 := NewModalityRetriever(store2, ModalityVisual, "space", []float32{1})
	if _, _, truncated, err = retriever2.RetrieveWithWindows(context.Background(), SearchQuery{}, 2); err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("a pool at exactly the limit must not report truncation")
	}
	if _, _, truncated, err = retriever2.RetrieveWithWindows(context.Background(), SearchQuery{}, -1); err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("unlimited retrieval never truncates")
	}
}

func TestMultimodalRetrieverCandidatesUseCanonicalShotBounds(t *testing.T) {
	store := &testModalityStore{
		universe: map[string]bool{"shot-1": true, "shot-2": true},
		soundEvents: []ModalityWindow{
			// One 5–15 s rain event spans two canonical shots; each pair row
			// keeps the event's own interval as evidence while the shot
			// bounds name the claimed shot.
			{AssetID: "asset", ShotID: "shot-1", RecordID: "event-rain", Modality: ModalitySoundEvent, StartMS: 5000, EndMS: 15000, ShotStartMS: 0, ShotEndMS: 10000, Score: 0.9},
			{AssetID: "asset", ShotID: "shot-2", RecordID: "event-rain", Modality: ModalitySoundEvent, StartMS: 5000, EndMS: 15000, ShotStartMS: 10000, ShotEndMS: 20000, Score: 0.9},
		},
	}
	retriever := NewModalityRetriever(store, ModalitySoundEvent, "", nil)
	candidates, windows, _, err := retriever.RetrieveWithWindows(context.Background(), SearchQuery{Raw: "rain"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates=%d, want one per overlapping shot", len(candidates))
	}
	for i, candidate := range candidates {
		want := []struct{ start, end int64 }{{0, 10000}, {10000, 20000}}[i]
		if candidate.StartMS != want.start || candidate.EndMS != want.end {
			t.Fatalf("candidate %d bounds=%d-%d, want canonical shot bounds %d-%d", i, candidate.StartMS, candidate.EndMS, want.start, want.end)
		}
		if candidate.Signals[SignalSoundEvent] != 0.9 {
			t.Fatalf("candidate %d lost its window score: %+v", i, candidate)
		}
	}
	if len(windows) != 2 || windows[0].StartMS != 5000 || windows[0].EndMS != 15000 {
		t.Fatalf("windows must keep the original event interval: %+v", windows)
	}
}

func TestSoundLabelVocabulary(t *testing.T) {
	tests := []struct {
		name  string
		words []string
		want  []string
	}{
		{name: "Chinese rain and English horn", words: []string{"大雨声", "a car horn"}, want: []string{"/m/06mb1", "/m/0912c9"}},
		{name: "English dog and glass", words: []string{"dog barking", "breaking glass"}, want: []string{"/m/039jq", "/m/07rn7sz", "/m/0bt9lr"}},
		{name: "all starter classes", words: []string{"thunder", "siren", "speech", "music", "footsteps", "wind", "water", "engine"}, want: []string{"/m/02mk9", "/m/03kmc9", "/m/03m9d0z", "/m/04rlf", "/m/07pbtc8", "/m/0838f", "/m/09x0r", "/m/0ngt1"}},
		{name: "unsupported term", words: []string{"an unknown sound"}, want: nil},
		{name: "English word boundary", words: []string{"dogmatic"}, want: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := SoundLabelVocabulary(test.words); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("SoundLabelVocabulary(%v) = %v, want %v", test.words, got, test.want)
			}
		})
	}
}

func TestMultimodalBuildMatchesDeduplicatesAndSorts(t *testing.T) {
	lowConfidence := 0.2
	highConfidence := 0.8
	windows := []ModalityWindow{
		{AssetID: "asset", ShotID: "shot", RecordID: "record-z", Modality: ModalitySoundEvent, StartMS: 20, EndMS: 30, Score: 0.7},
		{AssetID: "asset", ShotID: "shot", RecordID: "record-a", Modality: ModalityVisual, StartMS: 0, EndMS: 10, Score: 0.4, Confidence: &lowConfidence},
		{AssetID: "asset", ShotID: "shot", RecordID: "record-a", Modality: ModalityVisual, StartMS: 5, EndMS: 15, Score: 0.9, Confidence: &highConfidence},
	}
	matches := BuildMatches("condition", windows)
	if len(matches) != 2 {
		t.Fatalf("matches = %+v, want duplicate record removed", matches)
	}
	if matches[0].RecordID != "record-a" || matches[0].RetrievalScore != 0.9 || matches[0].ModelConfidence != &highConfidence {
		t.Fatalf("best record match = %+v", matches[0])
	}
	if matches[1].RecordID != "record-z" || matches[1].ConditionID != "condition" {
		t.Fatalf("matches not deterministically ordered or conditioned: %+v", matches)
	}
}

func TestMultimodalMergeCoverageNormalizesAndSorts(t *testing.T) {
	got := MergeCoverage(
		Coverage{Modality: ModalityVisual, State: "complete", Intervals: []Interval{{StartMS: 100, EndMS: 200}, {StartMS: 0, EndMS: 100}}},
		Coverage{Modality: ModalityVisual, State: "partial", Intervals: []Interval{{StartMS: 250, EndMS: 300}}},
		Coverage{Modality: ModalitySoundEvent, State: "unavailable"},
	)
	want := []Coverage{
		{Modality: ModalitySoundEvent, State: "unavailable", Intervals: []Interval{}},
		{Modality: ModalityVisual, State: "partial", Intervals: []Interval{{StartMS: 0, EndMS: 200}, {StartMS: 250, EndMS: 300}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeCoverage() = %+v, want %+v", got, want)
	}
}

func TestTemporalConditionsAreConjunctiveWithoutPredicates(t *testing.T) {
	store := &testModalityStore{soundByLabel: map[string][]ModalityWindow{
		"/m/06mb1":  {{AssetID: "asset-a", ShotID: "shot-a", RecordID: "rain-a", StartMS: 0, EndMS: 1000, Score: 0.9}},
		"/m/0912c9": {{AssetID: "asset-b", ShotID: "shot-b", RecordID: "horn-b", StartMS: 0, EndMS: 1000, Score: 0.9}},
	}}
	opts := DefaultOptions()
	opts.ModalityStore = store
	svc := NewService(&fakeStore{}, opts)
	response, err := svc.Search(context.Background(), SearchRequest{
		Query: "rain horn", Modalities: []string{ModalitySoundEvent},
		Temporal: &TemporalQuery{Conditions: []TemporalCondition{
			{ID: "rain", Modality: ModalitySoundEvent, Query: "rain"},
			{ID: "horn", Modality: ModalitySoundEvent, Query: "horn"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 0 {
		t.Fatalf("conditions satisfied in different assets must not match: %+v", response.Results)
	}
}
