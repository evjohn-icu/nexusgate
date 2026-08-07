package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// The retrieval golden set. This is the regression floor for every change to
// prompts, normalization, semantic features or ranking: run
// `go test ./internal/repository/sqlite -run RetrievalGolden` and the
// precision/recall table below moves, so a ranking change cannot silently
// hurt what users actually search for.
//
// The adversarial cases are the review round's contract:
//
//	1. an object appears only in the second half of a video
//	2. whole-asset tags must not pollute shots that never saw the object
//	3. English/Chinese synonym search
//	4. a shot's own description is right while the asset-global tag contradicts it
//	5. wide/close-up shots inside one asset (asset-level facet semantics)
//	6. a window-boundary shot's committed times are exact (pinned by the
//	   corpus input and the returned start/end assertions on that query)
//	7. the corpus holds post-dedup canonical rows — one observation, one
//	   shot — so retrieval over canonical data cannot return window copies
//	   (the overlap-dedup merge itself is pinned in video_analysis/merge_test.go)
//	8. speech belongs to a specific interval, never to the whole asset

type goldenShotSpec struct {
	startMS, endMS               int64
	description                  string
	tags, objects, actions, mood []string
}

type goldenAssetSpec struct {
	id       string
	analysis domain.StructuredAnalysis
	shots    []goldenShotSpec
}

type goldenQuerySpec struct {
	q           string
	relevant    []string // (assetID,ordinal) of shots that MUST be in top-10
	notRelevant []string // (assetID,ordinal) of shots that MUST NOT appear at all
	wantTimes   *goldenTimes
	comment     string
}

// goldenTimes pins the exact committed time range of the relevant shot, so a
// window-boundary regression (drifted start/end after slicing or rebasing)
// fails the golden set instead of silently moving the shot.
type goldenTimes struct {
	startMS, endMS int64
}

func goldenCorpus() []goldenAssetSpec {
	return []goldenAssetSpec{
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
		// canonical table holds it once, with the wider merged span. A search
		// for it must return exactly one shot, not both window copies.
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
	}
}

func goldenQueries() []goldenQuerySpec {
	return []goldenQuerySpec{
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
	}
}

type goldenMetrics struct {
	precision5, precision10, recall10 float64
	falsePositives                    int
	hits                              int
}

// TestRetrievalGolden runs the golden queries against every candidate weight
// blend and reports Precision@5/@10 and Recall@10. The adversarial
// false-positive asserts (2/4/8 family) are hard gates across ALL weight
// sets: no ranking weight may resurrect a shot whose evidence says otherwise.
// The summary table is the measurement that sets the default blend.
func TestRetrievalGolden(t *testing.T) {
	repo, ids := seedGoldenCorpus(t)
	ctx := context.Background()
	weightSets := []struct {
		name    string
		weights domain.HybridSearchWeights
		gate    bool // a default-candidate blend must not lose relevant shots
	}{
		{"lexical-only", domain.HybridSearchWeights{Semantic: 0, Lexical: 1}, false},
		{"current-70s-30l", domain.HybridSearchWeights{Semantic: 0.70, Lexical: 0.30}, true},
		{"70l-30h", domain.HybridSearchWeights{Semantic: 0.30, Lexical: 0.70}, true},
		{"80l-20h", domain.HybridSearchWeights{Semantic: 0.20, Lexical: 0.80}, true},
	}
	queries := goldenQueries()
	for _, ws := range weightSets {
		totals := goldenMetrics{}
		t.Logf("== weight set %s ==", ws.name)
		for _, gq := range queries {
			results, err := repo.hybridSearchShots(ctx, gq.q, 10, domain.FacetFilter{}, ws.weights)
			if err != nil {
				t.Fatal(err)
			}
			relevant := toGoldenIDs(ids, gq.relevant)
			notRelevant := toGoldenIDs(ids, gq.notRelevant)
			hitCount := 0
			for _, hit := range results {
				if relevant[hit.ID] {
					hitCount++
				}
				if notRelevant[hit.ID] {
					totals.falsePositives++
					t.Errorf("weight %s, query %q: false positive %s (%s)", ws.name, gq.q, hit.ID, gq.comment)
				}
			}
			p5, p10, r10 := metricsFor(results, relevant, len(relevant))
			totals.precision5 += p5
			totals.precision10 += p10
			totals.recall10 += r10
			totals.hits += hitCount
			if ws.gate && hitCount < len(relevant) {
				t.Errorf("weight %s, query %q: relevant shots missing from top-10 (%s)", ws.name, gq.q, gq.comment)
			}
			if gq.wantTimes != nil {
				for _, hit := range results {
					if relevant[hit.ID] {
						if hit.StartMS != gq.wantTimes.startMS || hit.EndMS != gq.wantTimes.endMS {
							t.Errorf("weight %s, query %q: times drifted, got %d-%d want %d-%d (%s)",
								ws.name, gq.q, hit.StartMS, hit.EndMS, gq.wantTimes.startMS, gq.wantTimes.endMS, gq.comment)
						}
						break
					}
				}
			}
		}
		n := float64(len(queries))
		t.Logf("P@5=%.3f P@10=%.3f R@10=%.3f relevant-in-top10=%d/%d false-positives=%d",
			totals.precision5/n, totals.precision10/n, totals.recall10/n, totals.hits, len(queries), totals.falsePositives)
	}
}

// metricsFor computes the per-query metrics. P@K uses a fixed K denominator
// (a corpus this small can return fewer than K rows; that is scored honestly
// as a miss on the tail).
func metricsFor(results []domain.ShotSearchResult, relevant map[string]bool, totalRelevant int) (p5, p10, r10 float64) {
	hit := func(k int) int {
		count := 0
		for i, r := range results {
			if i >= k {
				break
			}
			if relevant[r.ID] {
				count++
			}
		}
		return count
	}
	p5 = float64(hit(5)) / 5
	p10 = float64(hit(10)) / 10
	if totalRelevant > 0 {
		r10 = float64(hit(10)) / float64(totalRelevant)
	}
	return
}

// seedGoldenCorpus builds the golden corpus through the real commit path
// (StageModelRun → CommitAnalysisWithShots), which is what writes FTS rows,
// semantic vectors and asset_analysis together. It returns the repo and a map
// of (assetID:ordinal) → minted shot id.
func seedGoldenCorpus(t *testing.T) (*Repository, map[string]string) {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "golden.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	ids := map[string]string{}
	for _, asset := range goldenCorpus() {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`, asset.id, "fp-"+asset.id, now, now); err != nil {
			t.Fatal(err)
		}
		runID, _, err := repo.CreateModelRun(ctx, asset.id, "vision", "fixture", "fixture-model", "golden-"+asset.id, "footage-analysis-v4", "asset-analysis/v2", "{}")
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := json.Marshal(asset.analysis)
		if err := repo.StageModelRun(ctx, runID, `{"golden":true}`, string(parsed)); err != nil {
			t.Fatal(err)
		}
		shots := make([]domain.AssetShot, 0, len(asset.shots))
		for i, spec := range asset.shots {
			shots = append(shots, domain.AssetShot{
				AssetID: asset.id, SourceRunID: runID, Ordinal: i,
				StartMS: spec.startMS, EndMS: spec.endMS, Description: spec.description,
				Tags: spec.tags, Objects: spec.objects, Actions: spec.actions, Mood: spec.mood,
			})
		}
		if err := repo.CommitAnalysisWithShots(ctx, asset.id, runID, "asset-analysis/v2", asset.analysis, shots); err != nil {
			t.Fatal(err)
		}
		committed, err := repo.ListAssetShots(ctx, asset.id)
		if err != nil {
			t.Fatal(err)
		}
		for _, shot := range committed {
			ids[fmt.Sprintf("%s:%d", asset.id, shot.Ordinal)] = shot.ID
		}
	}
	return repo, ids
}

func toGoldenIDs(ids map[string]string, keys []string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		if id, ok := ids[key]; ok {
			out[id] = true
		}
	}
	return out
}
