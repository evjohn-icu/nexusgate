package search

import (
	"context"
	"reflect"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

type stubEncoder struct {
	space  string
	vector []float32
}

func (e stubEncoder) Encode(_ context.Context, _, _, _ string) (string, []float32, error) {
	return e.space, e.vector, nil
}

func assetLocalSpeaker() *SpeakerFilter {
	return &SpeakerFilter{AssetID: "asset", DiarizationGenerationID: "gen", LocalSpeakerID: "speaker-1"}
}

// TestRetrieveModalitiesImplicitSpeakerBundle: a speaker_filter by itself must
// request the speaker channel and return the complete eligible-shot allowlist,
// nontruncated, so every fused channel can be restricted to it.
func TestRetrieveModalitiesImplicitSpeakerBundle(t *testing.T) {
	store := &testModalityStore{
		universe: map[string]bool{"shot-a": true, "shot-b": true, "shot-c": true},
		speakerTurns: []ModalityWindow{
			{AssetID: "asset", ShotID: "shot-a", RecordID: "turn-1", StartMS: 0, EndMS: 1000, Score: 1},
			{AssetID: "asset", ShotID: "shot-b", RecordID: "turn-2", StartMS: 2000, EndMS: 3000, Score: 1},
			{AssetID: "asset-2", ShotID: "shot-c", RecordID: "turn-3", StartMS: 0, EndMS: 1000, Score: 1},
		},
	}
	opts := DefaultOptions()
	opts.ModalityStore = store
	svc := NewService(&fakeStore{}, opts)
	bundles, err := svc.retrieveModalities(context.Background(), SearchRequest{SpeakerFilter: assetLocalSpeaker()}, SearchQuery{Raw: "anything", Limit: 5}, 2, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 1 || bundles[0].signal != ModalitySpeaker {
		t.Fatalf("bundles=%+v, want one implicit speaker bundle", bundles)
	}
	allowlist := speakerAllowlist(bundles)
	if allowlist == nil {
		t.Fatal("speaker bundle must publish a shot allowlist")
	}
	for _, shot := range []string{"shot-a", "shot-b", "shot-c"} {
		if !allowlist[shot] {
			t.Fatalf("allowlist missing %s: %v", shot, allowlist)
		}
	}
	if len(bundles[0].candidates) != 3 {
		t.Fatalf("speaker matching was truncated to the request limit: %d candidates", len(bundles[0].candidates))
	}
	if bundles[0].coverage.State != "complete" || bundles[0].truncated {
		t.Fatalf("nontruncated speaker bundle coverage=%+v truncated=%v", bundles[0].coverage, bundles[0].truncated)
	}
	if store.gotLimit != -1 {
		t.Fatalf("speaker store limit=%d, want the nontruncated sentinel", store.gotLimit)
	}
}

// TestRetrieveModalitiesNoStoreWithSpeakerFilter: an unsupported speaker
// selector must fail typed instead of returning unfiltered shots.
func TestRetrieveModalitiesNoStoreWithSpeakerFilter(t *testing.T) {
	svc := NewService(&fakeStore{}, DefaultOptions())
	_, err := svc.retrieveModalities(context.Background(), SearchRequest{SpeakerFilter: assetLocalSpeaker()}, SearchQuery{Raw: "anything"}, 5, nil, false)
	if err == nil {
		t.Fatal("a speaker filter without a store must not return unrestricted bundles")
	}
}

// TestRetrieveModalitiesTruncatedCoverage: a store returning the probe row
// means the pool was cut, even when one shot occupied every row.
func TestRetrieveModalitiesTruncatedCoverage(t *testing.T) {
	store := &testModalityStore{
		universe: map[string]bool{"shot-a": true},
		visual: []ModalityWindow{
			{AssetID: "asset", ShotID: "shot-a", RecordID: "frame-1", StartMS: 0, EndMS: 100, Score: 0.9},
			{AssetID: "asset", ShotID: "shot-a", RecordID: "frame-2", StartMS: 100, EndMS: 200, Score: 0.8},
		},
	}
	opts := DefaultOptions()
	opts.ModalityStore = store
	opts.ModalityEncoder = stubEncoder{space: "space", vector: []float32{1}}
	svc := NewService(&fakeStore{}, opts)
	bundles, err := svc.retrieveModalities(context.Background(), SearchRequest{Modalities: []string{ModalityVisual}}, SearchQuery{Raw: "car"}, 1, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 1 {
		t.Fatalf("bundles=%d", len(bundles))
	}
	if !bundles[0].truncated || bundles[0].coverage.State != "partial" {
		t.Fatalf("coverage=%+v truncated=%v, want partial", bundles[0].coverage, bundles[0].truncated)
	}
}

// TestTemporalEvidenceLexicalSupportingShots: a compound raw text that matches
// no single shot still yields the temporal evidence through its conditions.
func TestTemporalEvidenceLexicalSupportingShots(t *testing.T) {
	store := &fakeStore{lexical: []domain.ShotSearchResult{
		shot("shot-a", "asset", 0, 1000, nil, "a red car"), shot("shot-b", "asset", 1000, 2000, nil, "a blue person"),
		shot("shot-c", "asset-2", 0, 1000, nil, "a red car"),
	}}
	store.lexical[0].LexicalScore = 0.9
	store.lexical[1].LexicalScore = 0.7
	store.lexical[2].LexicalScore = 0.8
	opts := DefaultOptions()
	opts.ModalityStore = &testModalityStore{universe: map[string]bool{}}
	svc := NewService(store, opts)
	assets, supporting, matches, err := svc.temporalEvidence(context.Background(),
		SearchRequest{Query: "red car while blue person", Temporal: &TemporalQuery{
			Conditions: []TemporalCondition{
				{ID: "left", Modality: ModalityLexical, Query: "red car"},
				{ID: "right", Modality: ModalityLexical, Query: "blue person"},
			},
			Predicates: []TemporalPredicate{{ID: "p", Relation: RelationBefore, LeftID: "left", RightID: "right", MaxGapMS: 5000}},
		}},
		SearchQuery{Raw: "red car while blue person"}, 5, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || !assets["asset"] {
		t.Fatalf("allowed assets=%v, want only asset", assets)
	}
	if len(supporting) != 2 || supporting[0].ShotID != "shot-a" || supporting[1].ShotID != "shot-b" {
		t.Fatalf("supporting shots=%+v, want shot-a then shot-b", supporting)
	}
	if supporting[0].Description != "a red car" || supporting[0].StartMS != 0 || supporting[0].EndMS != 1000 {
		t.Fatalf("supporting shot lost its metadata or canonical bounds: %+v", supporting[0])
	}
	if supporting[0].Signals[SignalLexical] != 0.9 || supporting[1].Signals[SignalLexical] != 0.7 {
		t.Fatalf("supporting shots lost their lexical scores: %+v", supporting)
	}
	conditions := map[string]bool{}
	for _, match := range matches {
		if match.AssetID != "asset" {
			t.Fatalf("match %v escaped the allowed asset", match)
		}
		if match.EvidenceStatus != string(EvidenceStatusPossible) {
			t.Fatalf("lexical matches must be possible, not timed: %+v", match)
		}
		conditions[match.ConditionID] = true
	}
	// The fake lexical store ignores the condition text, so each condition
	// matches both allowed shots: one match per (condition, shot).
	if len(conditions) != 2 || len(matches) != 4 {
		t.Fatalf("matches=%+v, want both conditions represented", matches)
	}
}

// TestTemporalEvidenceTranscriptLocatedSpans: transcript conditions use the
// established whole-word/ASR semantics and select only the matching spans.
func TestTemporalEvidenceTranscriptLocatedSpans(t *testing.T) {
	store := &fakeStore{transcript: []domain.ShotSearchResult{
		shot("shot-a", "asset", 0, 4000, nil, "speech"), shot("shot-b", "asset-2", 0, 4000, nil, "speech"),
	}}
	store.transcript[0].TranscriptScore = 0.9
	store.transcript[1].TranscriptScore = 0.5
	// fakeStore keys span rows by asset|start|end (see ShotTranscriptSpansBatch).
	store.spansByID = map[string][]domain.AlignmentWord{
		"asset|0|4000": {
			{StartMS: 100, EndMS: 500, Text: "we"},
			{StartMS: 600, EndMS: 1200, Text: "leave"},
			{StartMS: 1500, EndMS: 2100, Text: "tomorrow"},
		},
		"asset-2|0|4000": {
			{StartMS: 100, EndMS: 500, Text: "we"},
			{StartMS: 600, EndMS: 1200, Text: "leave"},
			// "tomorrow" is spoken too late to join the phrase inside the gap bound.
			{StartMS: 1500, EndMS: 2100, Text: "later"},
			{StartMS: 4800, EndMS: 5400, Text: "tomorrow"},
		},
	}
	opts := DefaultOptions()
	opts.ModalityStore = &testModalityStore{universe: map[string]bool{}}
	svc := NewService(store, opts)
	_, supporting, matches, err := svc.temporalEvidence(context.Background(),
		SearchRequest{Query: "we leave tomorrow", Temporal: &TemporalQuery{
			Conditions: []TemporalCondition{{ID: "said", Modality: ModalityTranscript, Query: "leave tomorrow"}},
		}},
		SearchQuery{Raw: "we leave tomorrow"}, 5, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(supporting) != 1 || supporting[0].ShotID != "shot-a" {
		t.Fatalf("supporting shots=%+v, want only shot-a", supporting)
	}
	if len(matches) != 1 || matches[0].StartMS != 600 || matches[0].EndMS != 2100 {
		t.Fatalf("transcript match=%+v, want the located span range 600-2100", matches)
	}
	if matches[0].EvidenceStatus != string(EvidenceStatusConfirmed) {
		t.Fatalf("located transcript spans are timed evidence: %+v", matches[0])
	}
}

// TestTemporalEvidenceSoundEventsWithinOneAsset: an event that spans two
// shots satisfies the temporal query once per asset, and its window keeps the
// original interval while the supporting candidates carry canonical bounds.
func TestTemporalEvidenceSoundEventsWithinOneAsset(t *testing.T) {
	store := &testModalityStore{
		universe: map[string]bool{"shot-1": true, "shot-2": true},
		soundByLabel: map[string][]ModalityWindow{
			"/m/06mb1": {
				{AssetID: "asset", ShotID: "shot-1", RecordID: "rain-1", Modality: ModalitySoundEvent, StartMS: 5000, EndMS: 15000, ShotStartMS: 0, ShotEndMS: 10000, Score: 0.9},
				{AssetID: "asset", ShotID: "shot-2", RecordID: "rain-1", Modality: ModalitySoundEvent, StartMS: 5000, EndMS: 15000, ShotStartMS: 10000, ShotEndMS: 20000, Score: 0.9},
			},
		},
	}
	opts := DefaultOptions()
	opts.ModalityStore = store
	svc := NewService(&fakeStore{}, opts)
	assets, supporting, matches, err := svc.temporalEvidence(context.Background(),
		SearchRequest{Query: "rain", Temporal: &TemporalQuery{
			Conditions: []TemporalCondition{{ID: "rain", Modality: ModalitySoundEvent, Query: "rain"}},
		}},
		SearchQuery{Raw: "rain"}, 5, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || !assets["asset"] {
		t.Fatalf("allowed assets=%v", assets)
	}
	if len(supporting) != 2 {
		t.Fatalf("supporting shots=%d, want both overlapped shots", len(supporting))
	}
	if supporting[0].StartMS != 0 || supporting[1].StartMS != 10000 || supporting[1].EndMS != 20000 {
		t.Fatalf("supporting bounds=%+v, want canonical shot bounds", supporting)
	}
	// The repeated window copy (one record, two shots) dedupes to one match
	// that keeps the event's own interval.
	if len(matches) != 1 || matches[0].RecordID != "rain-1" || matches[0].StartMS != 5000 || matches[0].EndMS != 15000 {
		t.Fatalf("matches=%+v, want one deduplicated event with its own interval", matches)
	}
}

// TestTemporalEvidenceUniverseFiltersConditions: a ready universe must filter
// lexical and transcript conditions too, so a facet can remove evidence.
func TestTemporalEvidenceUniverseFiltersConditions(t *testing.T) {
	store := &fakeStore{lexical: []domain.ShotSearchResult{
		shot("shot-a", "asset", 0, 1000, nil, "a red car"), shot("shot-b", "asset", 1000, 2000, nil, "a red car"),
	}}
	store.lexical[0].LexicalScore = 0.9
	store.lexical[1].LexicalScore = 0.8
	opts := DefaultOptions()
	opts.ModalityStore = &testModalityStore{universe: map[string]bool{}}
	svc := NewService(store, opts)
	assets, supporting, matches, err := svc.temporalEvidence(context.Background(),
		SearchRequest{Query: "red car while red car", Temporal: &TemporalQuery{
			Conditions: []TemporalCondition{{ID: "left", Modality: ModalityLexical, Query: "red car"}},
		}},
		SearchQuery{Raw: "red car while red car"}, 5, map[string]bool{"shot-b": true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || !assets["asset"] || len(supporting) != 1 || supporting[0].ShotID != "shot-b" || len(matches) != 1 {
		t.Fatalf("assets=%v supporting=%+v matches=%d, want only the facet-eligible shot", assets, supporting, len(matches))
	}
}

func TestSpeechMatchIntervals(t *testing.T) {
	spans := []domain.AlignmentWord{
		{StartMS: 0, EndMS: 400, Text: "we"},
		{StartMS: 500, EndMS: 1100, Text: "leave"},
		{StartMS: 1400, EndMS: 2000, Text: "tomorrow"},
		{StartMS: 4000, EndMS: 4400, Text: "leave"},
		{StartMS: 4500, EndMS: 5100, Text: "tomorrow"},
	}
	got := speechMatchIntervals("leave tomorrow", spans)
	want := []Interval{{StartMS: 500, EndMS: 2000}, {StartMS: 4000, EndMS: 5100}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("speechMatchIntervals = %v, want %v", got, want)
	}
	if got := speechMatchIntervals("leave", spans); len(got) != 2 {
		t.Fatalf("single word = %v, want both occurrences", got)
	}
	// ASCII whole-word discipline: a longer word never matches a shorter one.
	if got := speechMatchIntervals("lea", spans); got != nil {
		t.Fatalf("partial token matched: %v", got)
	}
	cjk := []domain.AlignmentWord{
		{StartMS: 0, EndMS: 300, Text: "我们"},
		{StartMS: 400, EndMS: 800, Text: "明天"},
		{StartMS: 900, EndMS: 1300, Text: "出发"},
	}
	if got := speechMatchIntervals("我们明天出发", cjk); len(got) != 1 || got[0] != (Interval{StartMS: 0, EndMS: 1300}) {
		t.Fatalf("CJK components must join across spans: %v", got)
	}
	if got := speechMatchIntervals("我们明天出发", cjk[:2]); got != nil {
		t.Fatalf("incomplete CJK phrase matched: %v", got)
	}
	// A gap beyond the phrase bound breaks the component chain.
	gapped := []domain.AlignmentWord{
		{StartMS: 0, EndMS: 300, Text: "我们"},
		{StartMS: 4000, EndMS: 4400, Text: "明天"},
	}
	if got := speechMatchIntervals("我们明天", gapped); got != nil {
		t.Fatalf("gap-violating components matched: %v", got)
	}
}
