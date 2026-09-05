package sqlite

import (
	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// searchV2Corpus is the Search v2 hard-negative corpus. It lives separately
// from goldenCorpus() on purpose: the legacy TestRetrievalGolden gates are
// pinned to goldenCorpus() + goldenQueries(), and adding vocabulary that
// overlaps existing queries (rain, street, night, ...) could move those
// gates. The v2 fixtures exercise the transcript channel, negative queries,
// near-duplicate diversity and the compiler's example query instead.
//
// Families (the spec's hard-negative list, mapped onto this data):
//
//  1. train vs rain        — same asset, adjacent shots, zero token overlap
//  2. no-person beach      — negative query: mustNot person, must keep the
//     explicitly-empty shot, exclude the observed one
//  3. near-duplicate cars  — 10.1s/11.2s cars: diversity must suppress the
//     second shot from the top-10
//  4. abstract mood        — 孤独压抑的夜晚, semantic intent
//  5. exact quoted speech  — the phrase exists ONLY in aligned transcript
//     words, never in descriptions
//  6. umbrella night       — the spec's compiler example query, fact intent
func searchV2Corpus() []goldenAssetSpec {
	return []goldenAssetSpec{
		// 1. train vs rain: "train" must hit the train shot only, "rain" the
		// rain shot only — the substring trap in both directions.
		{
			id:       "asset-train-rain",
			analysis: domain.StructuredAnalysis{Summary: "train station and wet street"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "train arriving at the platform", objects: []string{"train"}, tags: []string{"station"}},
				{startMS: 10_000, endMS: 20_000, description: "rainy street with umbrellas", tags: []string{"rain"}, objects: []string{"umbrella"}},
			},
		},
		// 2. 没有人的海边空镜: the empty shot's own words say "no people";
		// the observed shot must be excluded by the must-not gate.
		{
			id:       "asset-no-person-beach",
			analysis: domain.StructuredAnalysis{Summary: "beach day"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "empty beach no people", tags: []string{"beach", "empty"}, objects: []string{"ocean"}},
				{startMS: 10_000, endMS: 20_000, description: "person walking on the beach", objects: []string{"person"}},
			},
			session: "session-beach",
		},
		// 3. Near-duplicate car shots: the diversity pass must collapse the
		// burst into one representative.
		{
			id:       "asset-car-duplicate",
			analysis: domain.StructuredAnalysis{Summary: "car crossing the square", Subjects: []string{"car"}},
			shots: []goldenShotSpec{
				{startMS: 10_100, endMS: 11_000, description: "red car crossing the square", objects: []string{"car"}},
				{startMS: 11_200, endMS: 12_100, description: "red car crossing the square again", objects: []string{"car"}},
			},
		},
		// 4. Abstract mood query: no objects anywhere, semantic intent.
		{
			id:       "asset-lonely-night",
			analysis: domain.StructuredAnalysis{Summary: "quiet night room"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 20_000, description: "dark quiet room at night", mood: []string{"lonely", "depressing"}, tags: []string{"night"}},
			},
			session: "session-lonely",
		},
		// 5. Exact quoted speech: the phrase lives only in aligned words; a
		// description-only search can never find it.
		{
			id:       "asset-speech-quote",
			analysis: domain.StructuredAnalysis{Summary: "documentary interview"},
			shots: []goldenShotSpec{
				{startMS: 90_000, endMS: 200_000, description: "interview room interior"},
			},
			transcriptWords: []goldenTranscriptWord{
				{startMS: 100_000, endMS: 101_000, text: "我们", confidence: 0.95},
				{startMS: 101_000, endMS: 102_000, text: "明天", confidence: 0.95},
				{startMS: 102_000, endMS: 103_000, text: "出发", confidence: 0.95},
			},
		},
		// 6. The spec's compiler example: 夜晚下雨，有人撑伞走过街道.
		{
			id:       "asset-umbrella-night",
			analysis: domain.StructuredAnalysis{Summary: "rainy night street"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 15_000, description: "rainy street at night person with umbrella", objects: []string{"person", "umbrella"}, tags: []string{"rain", "night"}},
				{startMS: 15_000, endMS: 30_000, description: "dark empty alley", tags: []string{"night"}},
			},
		},
	}
}

// searchV2Queries carries the v2 corpus questions with hand-tagged intents.
type searchV2QuerySpec struct {
	q           string
	intent      string // hand-tagged intent for per-intent metrics
	relevant    []string
	notRelevant []string
	comment     string
}

func searchV2Queries() []searchV2QuerySpec {
	return []searchV2QuerySpec{
		{q: "train", intent: "fact", relevant: []string{"asset-train-rain:0"}, notRelevant: []string{"asset-train-rain:1"}, comment: "train must not substring-match rain"},
		{q: "rain", intent: "fact", relevant: []string{"asset-train-rain:1"}, notRelevant: []string{"asset-train-rain:0"}, comment: "rain must not substring-match train"},
		{q: "没有人的海边空镜", intent: "negative", relevant: []string{"asset-no-person-beach:0"}, notRelevant: []string{"asset-no-person-beach:1"}, comment: "negative query excludes the observed shot"},
		{q: "red car", intent: "fact", relevant: []string{"asset-car-duplicate:0", "asset-car-duplicate:1"}, comment: "near-duplicates both relevant; diversity may suppress one"},
		{q: "孤独压抑的夜晚", intent: "semantic", relevant: []string{"asset-lonely-night:0"}, comment: "abstract mood query"},
		{q: "谁说过我们明天出发", intent: "speech", relevant: []string{"asset-speech-quote:0"}, comment: "phrase only in aligned transcript words"},
		{q: "夜晚下雨，有人撑伞走过街道", intent: "fact", relevant: []string{"asset-umbrella-night:0"}, notRelevant: []string{"asset-umbrella-night:1"}, comment: "compiler example query"},
	}
}
