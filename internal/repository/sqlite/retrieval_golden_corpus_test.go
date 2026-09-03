package sqlite

import (
	"fmt"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// The full-scale retrieval golden corpus. 41 assets (26 handwritten adversarial
// assets + 15 generated distractors), 65 queries, fixture-driven: each
// asset is committed through the real commit path (see seedGoldenCorpus in
// retrieval_golden_test.go), so FTS rows, semantic vectors and
// asset_analysis are exactly what production search reads.
//
// The families below are the retrieval contract, in the same order as the
// review round's adversarial list plus the additions that public release
// demanded:
//
//	1.  an object appears only in part of a video (second half, first part, tail)
//	2.  whole-asset tags must not pollute shots that never saw the object
//	3.  English/Chinese synonym search
//	4.  a shot's own description is right while the asset-global tag contradicts it
//	5.  wide/medium/close-up shots inside one asset must retrieve distinctly
//	6.  a window-boundary shot's committed times are exact (pinned)
//	7.  post-dedup canonical rows: one observation, one shot
//	8.  speech belongs to a specific interval, never to the whole asset
//	9.  negative assertions: a plausible query must not match shots lacking evidence
//	10. distractor assets: realistic retrieval noise that a smaller corpus lacks
//
// The rules that keep the corpus honest:
//   - shot text is the only thing that lands in shot vectors/FTS; asset-level
//     fields are committed to asset_analysis only (the fixture follows the
//     production write path, so a shot never inherits asset-global text).
//   - Chinese queries are bigram-lexical (the fixture text carries the same
//     CJK tokens, mirroring how real model output is written); English queries
//     are lexical/semantic. The alias table in discovery/features.go remains
//     the only cross-language semantic bridge.
//   - a notRelevant shot must never appear in ANY weight set's top-10; a
//     relevant shot must be in the top-10 of every gated blend (see the
//     runner). Adding an asset whose text matches an existing query therefore
//     needs a re-run to confirm no gate moved.

func goldenCorpus() []goldenAssetSpec {
	corpus := []goldenAssetSpec{
		// Case 1 + 2: the car only exists at 20-30s. The asset-global subjects
		// claim "car" too; shots without their own evidence must stay clean.
		{
			id: "asset-car-second-half",
			analysis: domain.StructuredAnalysis{
				Summary: "traffic intersection", Subjects: []string{"car"}, SceneTags: []string{"traffic"},
			},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "empty street at dawn"},
				{startMS: 20_000, endMS: 30_000, description: "red car crossing", objects: []string{"car"}},
			},
		},
		// Case 4: the shot says "person walking" while the asset-global tag
		// describes an abandoned street. The shot's own evidence must win.
		{
			id: "asset-abandoned-street",
			analysis: domain.StructuredAnalysis{
				Summary: "abandoned street", SceneTags: []string{"abandoned", "empty_street"},
			},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "person walking alone", objects: []string{"person"}},
				{startMS: 10_000, endMS: 20_000, description: "abandoned shopfront", tags: []string{"empty_street"}},
			},
		},
		// Case 3: the same scene describable in either language.
		{
			id:       "asset-city-night",
			analysis: domain.StructuredAnalysis{Summary: "night city streets"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 20_000, description: "城市夜景霓虹灯", tags: []string{"城市", "夜景"}},
				{startMS: 20_000, endMS: 40_000, description: "雨中的海滩", tags: []string{"beach", "rain"}, objects: []string{"ocean"}},
			},
		},
		// Case 5: one asset holds wide AND close-up shots; the asset is
		// labelled wide. asset_shot_size=wide is an asset-level filter and
		// matches every shot of the asset — pinned, documented semantics.
		{
			id: "asset-mixed-shot-size",
			analysis: domain.StructuredAnalysis{
				Summary: "street scene", ShotSize: "wide", CameraMotion: "static",
			},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "wide establishing shot of the street"},
				{startMS: 10_000, endMS: 20_000, description: "close-up of hands on a railing"},
			},
		},
		// Case 6: a windowed analysis places a shot exactly on the 8-minute
		// window cut; the committed times must be exact.
		{
			id:       "asset-window-boundary",
			analysis: domain.StructuredAnalysis{Summary: "long interview"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 480_000, description: "interview opening"},
				{startMS: 480_000, endMS: 485_000, description: "boundary shot at the eight minute cut"},
				{startMS: 485_000, endMS: 900_000, description: "interview continues"},
			},
		},
		// Case 7: two overlapping windows both saw the same observation; the
		// canonical table holds it once, with the wider merged span.
		{
			id:       "asset-overlap-dedup",
			analysis: domain.StructuredAnalysis{Summary: "street market"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 600_000, description: "street vendors bustling", tags: []string{"market"}},
				{startMS: 600_000, endMS: 900_000, description: "empty street after hours"},
			},
		},
		// Case 8: speech at 310-315s belongs to that interval only.
		{
			id:       "asset-timed-speech",
			analysis: domain.StructuredAnalysis{Summary: "documentary"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 300_000, description: "quiet street scene no speech"},
				{startMS: 310_000, endMS: 315_000, description: "narrator says hello world 你好世界", tags: []string{"narration"}},
			},
		},

		// Case 1 variant: the car exists at the START of a 30s asset; the
		// later shot is empty. Bilingual fixture text exercises both the
		// English lexical path and the CJK bigram path.
		{
			id:       "asset-car-first-half",
			analysis: domain.StructuredAnalysis{Summary: "morning square"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "red sedan 红色轿车 crossing the square", objects: []string{"car"}},
				{startMS: 10_000, endMS: 30_000, description: "empty parking lot"},
			},
		},
		// Case 1 variant: night footage, the car arrives only in the last
		// ten seconds. The first shot deliberately carries no "night" token
		// so the notRelevant gate is about evidence, not phrasing.
		{
			id:       "asset-car-night",
			analysis: domain.StructuredAnalysis{Summary: "night drive"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 50_000, description: "empty dark road"},
				{startMS: 50_000, endMS: 60_000, description: "car headlights approaching in the rain", objects: []string{"car"}, tags: []string{"rain", "night"}},
			},
		},
		// Case 2: the asset is a beach day, one shot is indoors. The indoor
		// shot must never surface for "beach" — asset-global SceneTags are
		// not shot evidence.
		{
			id: "asset-beach-indoor",
			analysis: domain.StructuredAnalysis{
				Summary: "beach day", SceneTags: []string{"beach", "summer"},
			},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 15_000, description: "indoor kitchen cooking pasta on the stove", tags: []string{"kitchen"}},
				{startMS: 15_000, endMS: 30_000, description: "sunbathers on the beach", objects: []string{"ocean"}, tags: []string{"beach"}},
			},
		},
		// Case 9: the dog is real and confined to one shot; the swing shot
		// must not inherit it, and a query for a creature that is nowhere in
		// the corpus must return nothing at all.
		{
			id: "asset-park-dog",
			analysis: domain.StructuredAnalysis{
				Summary: "city park", SceneTags: []string{"park"},
			},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "children on the swings"},
				{startMS: 10_000, endMS: 20_000, description: "dog running on the grass", objects: []string{"dog"}},
			},
		},
		// Case 9 variant: an office with no animals at all.
		{
			id:       "asset-office",
			analysis: domain.StructuredAnalysis{Summary: "open plan office"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "people typing on laptops at a desk", objects: []string{"laptop"}},
				{startMS: 10_000, endMS: 20_000, description: "empty meeting room"},
			},
		},
		// Case 5: three shot sizes inside one asset, retrieval must tell
		// them apart through the shots' own text (shot_size stays asset-level
		// by contract, so the distinction here is the shot evidence).
		{
			id: "asset-stadium",
			analysis: domain.StructuredAnalysis{
				Summary: "marathon finish", ShotSize: "wide",
			},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "wide aerial view of the stadium"},
				{startMS: 10_000, endMS: 20_000, description: "medium shot of a runner at the finish line", tags: []string{"runner"}},
				{startMS: 20_000, endMS: 30_000, description: "close-up of the runner's face"},
			},
		},
		// Case 4 variant: the asset is a factory tour; the first shot shows
		// a schoolyard. Neither direction may leak.
		{
			id: "asset-factory-schoolyard",
			analysis: domain.StructuredAnalysis{
				Summary: "old factory tour", SceneTags: []string{"factory"},
			},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "children playing in a schoolyard", objects: []string{"children"}},
				{startMS: 10_000, endMS: 20_000, description: "rusting factory machinery", tags: []string{"factory"}},
			},
		},
		// Case 7 variant: a second market asset with one merged observation.
		{
			id:       "asset-market-flowers",
			analysis: domain.StructuredAnalysis{Summary: "flower market"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 600_000, description: "flower vendors selling tulips", tags: []string{"market"}},
				{startMS: 600_000, endMS: 900_000, description: "closed stalls"},
			},
		},
		// Case 6 variant: a second window-boundary asset, this time at the
		// ten-minute cut, with pinned times.
		{
			id:       "asset-ceremony-boundary",
			analysis: domain.StructuredAnalysis{Summary: "opening ceremony"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 600_000, description: "crowd waiting quietly"},
				{startMS: 600_000, endMS: 605_000, description: "ceremony begins at the ten minute mark"},
				{startMS: 605_000, endMS: 900_000, description: "speeches continue"},
			},
		},
		// Case 8 variant: speech confined to 100-110s of a garden video.
		{
			id:       "asset-garden-speech",
			analysis: domain.StructuredAnalysis{Summary: "rose garden"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 100_000, description: "quiet rose garden"},
				{startMS: 100_000, endMS: 110_000, description: "narrator: 天气预报 rain tomorrow", tags: []string{"narration"}},
			},
		},
		// Case 8 variant: the speech sits in the asset's final five seconds.
		{
			id:       "asset-walk-speech-tail",
			analysis: domain.StructuredAnalysis{Summary: "old town walk"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 145_000, description: "walk through the old town"},
				{startMS: 145_000, endMS: 150_000, description: "closing line: 明天见 see you tomorrow", tags: []string{"narration"}},
			},
		},
		// Distinct-domain assets so the corpus covers more than streets and
		// beaches: each pair is its own retrieval island.
		{
			id:       "asset-bridge-river",
			analysis: domain.StructuredAnalysis{Summary: "river crossing"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 20_000, description: "old stone bridge over the river"},
				{startMS: 20_000, endMS: 40_000, description: "boat passing under the bridge", objects: []string{"boat"}},
			},
		},
		{
			id:       "asset-mountain-hike",
			analysis: domain.StructuredAnalysis{Summary: "alpine hike"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 15_000, description: "misty mountain trail"},
				{startMS: 15_000, endMS: 30_000, description: "hikers resting at the summit", objects: []string{"hiker"}},
			},
		},
		{
			id:       "asset-chef-kitchen",
			analysis: domain.StructuredAnalysis{Summary: "restaurant kitchen"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "chef plating dishes in a restaurant kitchen", objects: []string{"chef"}},
				{startMS: 10_000, endMS: 20_000, description: "empty dining room"},
			},
		},
		{
			id:       "asset-library-reading",
			analysis: domain.StructuredAnalysis{Summary: "city library"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "quiet library reading room"},
				{startMS: 10_000, endMS: 20_000, description: "student reading at a desk"},
			},
		},
		{
			id:       "asset-train-station",
			analysis: domain.StructuredAnalysis{Summary: "commuter station"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "commuters on the platform"},
				{startMS: 10_000, endMS: 20_000, description: "train arriving at the platform", objects: []string{"train"}},
			},
		},
		{
			id:       "asset-cafe-coffee",
			analysis: domain.StructuredAnalysis{Summary: "corner cafe"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "people drinking coffee 咖啡 at a cafe table", objects: []string{"cup"}, tags: []string{"coffee"}},
				{startMS: 10_000, endMS: 20_000, description: "empty cafe counter"},
			},
		},
		{
			id:       "asset-snow-town",
			analysis: domain.StructuredAnalysis{Summary: "winter town"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "snow-covered rooftops", tags: []string{"snow"}},
				{startMS: 10_000, endMS: 20_000, description: "children sledding on the hill"},
			},
		},
		// Case 11: alias words must match whole words, not substrings. The
		// heuristic's traffic alias contains "car", and substring matching
		// made "carefree" (and "carpet", "careful") match a "car" query — a
		// search for a vehicle returning shots with no vehicle evidence. The
		// query below is a hard gate across every weight set.
		{
			id:       "asset-carefree-beach",
			analysis: domain.StructuredAnalysis{Summary: "summer holiday"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: "carefree people relaxing under a carpet of clouds", mood: []string{"carefree"}},
				{startMS: 10_000, endMS: 20_000, description: "people carrying picnic baskets to the shore", tags: []string{"summer"}},
			},
		},
	}
	corpus = append(corpus, goldenDistractors()...)
	return corpus
}

// goldenDistractors are realistic retrieval noise: unrelated scenes that a
// smaller corpus lacks. They make top-10 competition real, and a few queries
// deliberately target them so distractors are retrievable, not dead weight.
// Their vocabulary avoids the adversarial families' words, and none of them
// appears in any adversarial notRelevant list.
func goldenDistractors() []goldenAssetSpec {
	templates := [][2]string{
		{"rustic bakery with fresh bread loaves", "baker kneading dough at the counter"},
		{"modern art gallery with abstract paintings", "visitors admiring a large canvas"},
		{"busy fish market with crates of seafood", "fishmonger gutting a catch at the stall"},
		{"children's playground on a sunny morning", "kids on a slide and a seesaw"},
		{"old bookstore with shelves of used books", "customer browsing a shelf of novels"},
		{"furniture workshop with wooden chairs", "carpenter sanding a tabletop"},
		{"flower nursery with rows of potted plants", "gardener watering the plants"},
		{"bicycle repair shop with hanging frames", "mechanic fixing a bike wheel"},
		{"garden party with a long buffet table", "guests chatting on the lawn"},
		{"pottery studio with clay pots on shelves", "potter shaping a vase at the wheel"},
		{"empty cinema lobby with red carpets", "poster wall advertising new films"},
		{"supermarket aisle stocked with groceries", "cashier scanning items at the checkout"},
		{"city rooftop with water tanks", "pigeons resting on the edge"},
		{"greenhouse full of tomato plants", "worker picking ripe tomatoes"},
		{"docks with fishing boats at the mooring", "crane loading crates onto a ship"},
	}
	out := make([]goldenAssetSpec, 0, len(templates))
	for i, scenes := range templates {
		out = append(out, goldenAssetSpec{
			id:       fmt.Sprintf("asset-dist-%02d", i+1),
			analysis: domain.StructuredAnalysis{Summary: "miscellaneous footage"},
			shots: []goldenShotSpec{
				{startMS: 0, endMS: 10_000, description: scenes[0]},
				{startMS: 10_000, endMS: 20_000, description: scenes[1]},
			},
		})
	}
	return out
}

func goldenQueries() []goldenQuerySpec {
	return []goldenQuerySpec{
		// Existing adversarial queries.
		{q: "car", relevant: []string{"asset-car-second-half:1"}, notRelevant: []string{"asset-car-second-half:0"}, comment: "object only in the second half"},
		{q: "汽车", relevant: []string{"asset-car-second-half:1"}, notRelevant: []string{"asset-car-second-half:0"}, comment: "CN synonym must not inherit asset-global car"},
		{q: "person walking", relevant: []string{"asset-abandoned-street:0"}, notRelevant: []string{"asset-abandoned-street:1"}, comment: "shot evidence wins over contradictory asset tag"},
		{q: "行人", relevant: []string{"asset-abandoned-street:0"}, notRelevant: []string{"asset-abandoned-street:1"}, comment: "CN synonym via alias table"},
		{q: "city night", relevant: []string{"asset-city-night:0"}, comment: "EN query for a CN-described shot"},
		{q: "城市夜景", relevant: []string{"asset-city-night:0"}, comment: "CN bigram query"},
		{q: "rainy beach", relevant: []string{"asset-city-night:1"}, comment: "EN query for a CN-described beach"},
		{q: "close-up hands", relevant: []string{"asset-mixed-shot-size:1"}, comment: "close-up shot inside a wide-labelled asset"},
		{q: "eight minute", relevant: []string{"asset-window-boundary:1"}, wantTimes: &goldenTimes{startMS: 480_000, endMS: 485_000}, comment: "window-boundary shot found with exact times"},
		{q: "vendors market", relevant: []string{"asset-overlap-dedup:0"}, notRelevant: []string{"asset-overlap-dedup:1"}, comment: "merged observation returned once"},
		{q: "hello", relevant: []string{"asset-timed-speech:1"}, notRelevant: []string{"asset-timed-speech:0"}, comment: "speech confined to its interval"},
		{q: "你好世界", relevant: []string{"asset-timed-speech:1"}, notRelevant: []string{"asset-timed-speech:0"}, comment: "CN speech confined to its interval"},

		// Case 1 variants and the car family.
		{q: "red sedan", relevant: []string{"asset-car-first-half:0"}, comment: "bilingual fixture, EN lexical path"},
		{q: "轿车", relevant: []string{"asset-car-first-half:0"}, notRelevant: []string{"asset-car-first-half:1"}, comment: "bilingual fixture, CJK bigram path"},
		{q: "empty parking lot", relevant: []string{"asset-car-first-half:1"}, notRelevant: []string{"asset-car-first-half:0"}, comment: "empty shot is itself retrievable"},
		{q: "vehicle", relevant: []string{"asset-car-second-half:1", "asset-car-first-half:0"}, comment: "synonym via the traffic alias family"},
		{q: "traffic", relevant: []string{"asset-car-second-half:1"}, notRelevant: []string{"asset-car-second-half:0"}, comment: "alias-canonical query"},
		{q: "night car", relevant: []string{"asset-car-night:1"}, notRelevant: []string{"asset-car-night:0"}, comment: "car evidence is confined to the last ten seconds"},
		{q: "rain headlights", relevant: []string{"asset-car-night:1"}, comment: "tags rain+night on the car shot only"},
		{q: "night", relevant: []string{"asset-car-night:1"}, notRelevant: []string{"asset-car-night:0"}, comment: "no night token in the empty shot"},

		// Case 2: beach asset with an indoor shot.
		{q: "beach", relevant: []string{"asset-beach-indoor:1"}, notRelevant: []string{"asset-beach-indoor:0"}, comment: "asset-global beach must not leak into the indoor shot"},
		{q: "indoor kitchen", relevant: []string{"asset-beach-indoor:0"}, notRelevant: []string{"asset-beach-indoor:1"}, comment: "the indoor shot is evidence on its own"},
		{q: "pasta stove", relevant: []string{"asset-beach-indoor:0"}, comment: "kitchen vocabulary"},

		// Case 9: negative assertions.
		{q: "dog", relevant: []string{"asset-park-dog:1"}, notRelevant: []string{"asset-park-dog:0"}, comment: "dog is a per-shot object"},
		{q: "children swings", relevant: []string{"asset-park-dog:0"}, notRelevant: []string{"asset-park-dog:1"}, comment: "swing shot retrievable without the dog"},
		{q: "cat", notRelevant: []string{"asset-park-dog:0", "asset-park-dog:1", "asset-office:0", "asset-office:1"}, comment: "a creature nowhere in the corpus surfaces nothing"},
		{q: "laptop desk", relevant: []string{"asset-office:0"}, notRelevant: []string{"asset-office:1"}, comment: "office evidence"},

		// Case 5: shot-size distinction inside one asset.
		{q: "aerial", relevant: []string{"asset-stadium:0"}, comment: "wide aerial shot"},
		{q: "runner finish line", relevant: []string{"asset-stadium:1"}, comment: "medium shot of the runner"},
		{q: "close-up", relevant: []string{"asset-stadium:2"}, notRelevant: []string{"asset-stadium:0"}, comment: "close-up shot only"},
		{q: "stadium", relevant: []string{"asset-stadium:0"}, comment: "asset-level summary word surfaces the aerial shot"},

		// Case 4 variant: factory vs schoolyard.
		{q: "schoolyard children", relevant: []string{"asset-factory-schoolyard:0"}, comment: "shot evidence beats asset-global factory"},
		{q: "factory machinery", relevant: []string{"asset-factory-schoolyard:1"}, notRelevant: []string{"asset-factory-schoolyard:0"}, comment: "factory stays with the shot that saw it"},

		// Case 7 variant: flower market.
		{q: "flower market", relevant: []string{"asset-market-flowers:0"}, notRelevant: []string{"asset-market-flowers:1"}, comment: "merged observation returned once"},
		{q: "tulips", relevant: []string{"asset-market-flowers:0"}, comment: "specific object vocabulary"},

		// Case 6 variant: ten-minute boundary, pinned times.
		{q: "ceremony ten minute", relevant: []string{"asset-ceremony-boundary:1"}, wantTimes: &goldenTimes{startMS: 600_000, endMS: 605_000}, comment: "second window-boundary shot with exact times"},

		// Case 8 variants.
		{q: "天气预报", relevant: []string{"asset-garden-speech:1"}, notRelevant: []string{"asset-garden-speech:0"}, comment: "CN speech confined to its interval"},
		{q: "rose garden", relevant: []string{"asset-garden-speech:0"}, notRelevant: []string{"asset-garden-speech:1"}, comment: "garden shot has no speech"},
		{q: "明天见", relevant: []string{"asset-walk-speech-tail:1"}, notRelevant: []string{"asset-walk-speech-tail:0"}, comment: "speech in the final five seconds only"},
		{q: "narration", relevant: []string{"asset-garden-speech:1", "asset-walk-speech-tail:1"}, comment: "narration tag shared by the two speech assets"},

		// Distinct-domain assets.
		{q: "stone bridge", relevant: []string{"asset-bridge-river:0"}, comment: "bridge shot"},
		{q: "river", relevant: []string{"asset-bridge-river:0"}, notRelevant: []string{"asset-bridge-river:1"}, comment: "river vocabulary only in the first shot"},
		{q: "boat", relevant: []string{"asset-bridge-river:1"}, comment: "boat object"},
		{q: "mountain trail", relevant: []string{"asset-mountain-hike:0"}, comment: "trail shot"},
		{q: "summit", relevant: []string{"asset-mountain-hike:1"}, comment: "summit shot"},
		{q: "chef plating", relevant: []string{"asset-chef-kitchen:0"}, comment: "chef shot"},
		{q: "empty dining room", relevant: []string{"asset-chef-kitchen:1"}, notRelevant: []string{"asset-chef-kitchen:0"}, comment: "dining room shot"},
		{q: "quiet library", relevant: []string{"asset-library-reading:0"}, comment: "reading room"},
		{q: "student desk", relevant: []string{"asset-library-reading:1"}, comment: "student at a desk"},
		{q: "commuters platform", relevant: []string{"asset-train-station:0"}, comment: "platform shot"},
		{q: "train arrival", relevant: []string{"asset-train-station:1"}, notRelevant: []string{"asset-train-station:0"}, comment: "train object shot"},
		{q: "coffee cup", relevant: []string{"asset-cafe-coffee:0"}, notRelevant: []string{"asset-cafe-coffee:1"}, comment: "cup object shot"},
		{q: "咖啡", relevant: []string{"asset-cafe-coffee:0"}, notRelevant: []string{"asset-cafe-coffee:1"}, comment: "CN bigram in bilingual fixture"},
		{q: "empty counter", relevant: []string{"asset-cafe-coffee:1"}, comment: "the empty shot is evidence on its own"},
		{q: "snow rooftops", relevant: []string{"asset-snow-town:0"}, comment: "snow shot"},
		{q: "sledding hill", relevant: []string{"asset-snow-town:1"}, comment: "sledding shot"},

		// Case 11: substring alias false positives. "carefree" contains
		// "car", "carpet" contains "car" — neither may surface for a vehicle
		// query, under any weight set.
		{q: "car", relevant: []string{"asset-car-second-half:1"}, notRelevant: []string{"asset-carefree-beach:0", "asset-carefree-beach:1"}, comment: "substring 'car' must not match carefree/carpet"},
		{q: "beach picnic", relevant: []string{"asset-carefree-beach:1"}, comment: "the beach shot itself is retrievable"},
		{q: "carefree", relevant: []string{"asset-carefree-beach:0"}, notRelevant: []string{"asset-carefree-beach:1"}, comment: "mood word stays retrievable"},
		{q: "雨中", relevant: []string{"asset-city-night:1"}, comment: "CN bigram rain scene"},
		{q: "霓虹灯", relevant: []string{"asset-city-night:0"}, comment: "CN bigram neon shot"},

		// Distractor retrievability: noise assets are searchable, not just present.
		{q: "bread loaves", relevant: []string{"asset-dist-01:0"}, comment: "distractor: bakery"},
		{q: "bicycle repair", relevant: []string{"asset-dist-08:0"}, comment: "distractor: bike shop"},
		{q: "ripe tomatoes", relevant: []string{"asset-dist-14:1"}, comment: "distractor: greenhouse"},
		{q: "fishing boats", relevant: []string{"asset-dist-15:0"}, comment: "distractor: docks"},
	}
}
