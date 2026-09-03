package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// The retrieval golden set. This is the regression floor for every change to
// prompts, normalization, semantic features or ranking: run
// `go test ./internal/repository/sqlite -run RetrievalGolden` and the
// precision/recall table below moves, so a ranking change cannot silently
// hurt what users actually search for.
//
// The adversarial cases are the review round's contract, now carried by the
// full-scale corpus in retrieval_golden_corpus_test.go:
//
//	1. an object appears only in part of a video (second half, first part, tail)
//	2. whole-asset tags must not pollute shots that never saw the object
//	3. English/Chinese synonym search
//	4. a shot's own description is right while the asset-global tag contradicts it
//	5. wide/medium/close-up shots inside one asset must retrieve distinctly
//	6. a window-boundary shot's committed times are exact (pinned by the
//	   corpus input and the returned start/end assertions on that query)
//	7. the corpus holds post-dedup canonical rows — one observation, one
//	   shot — so retrieval over canonical data cannot return window copies
//	8. speech belongs to a specific interval, never to the whole asset
//	9. negative assertions: plausible queries surface nothing without evidence
//	10. distractor assets make top-10 competition real

type goldenShotSpec struct {
	startMS, endMS               int64
	description                  string
	tags, objects, actions, mood []string
}

// goldenTranscriptWord is one aligned ASR word, seeded through the real
// SaveTranscript + SaveAlignment write path.
type goldenTranscriptWord struct {
	startMS, endMS int64
	text           string
	confidence     float64
}

type goldenAssetSpec struct {
	id       string
	analysis domain.StructuredAnalysis
	shots    []goldenShotSpec
	// transcriptWords seeds a transcript + alignment run so the v2 transcript
	// channel and evidence source have real data to read.
	transcriptWords []goldenTranscriptWord
	// session puts the asset in a shoot session (diversity testing).
	session string
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

type goldenMetrics struct {
	precision5, precision10, recall10 float64
	falsePositives                    int
	hits                              int
	// FP attribution: a false positive counts as lexical when the pure
	// lexical signal alone would rank it in the top-10, semantic when the
	// pure semantic signal alone would, and blend-edge when neither would
	// (only the fused score surfaced it). This is the "semantic
	// false-positive assertions" measurement: a search claiming a shot has
	// evidence it lacks, carried by the semantic side.
	lexicalFPs, semanticFPs, blendEdgeFPs int
}

// pureWeights are the two single-signal blends used to attribute a false
// positive to the signal that would have surfaced it on its own.
var pureWeights = []domain.HybridSearchWeights{
	{Semantic: 1, Lexical: 0},
	{Semantic: 0, Lexical: 1},
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
		name   string
		search func(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error)
		gate   bool // a default-candidate blend must not lose relevant shots
	}{
		{"lexical-only", func(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
			return repo.hybridSearchShots(ctx, q, limit, domain.FacetFilter{}, domain.HybridSearchWeights{Semantic: 0, Lexical: 1})
		}, false},
		{"current-70s-30l", func(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
			return repo.hybridSearchShots(ctx, q, limit, domain.FacetFilter{}, domain.HybridSearchWeights{Semantic: 0.70, Lexical: 0.30})
		}, true},
		{"70l-30h", func(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
			return repo.hybridSearchShots(ctx, q, limit, domain.FacetFilter{}, domain.HybridSearchWeights{Semantic: 0.30, Lexical: 0.70})
		}, true},
		{"80l-20h", func(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
			return repo.hybridSearchShots(ctx, q, limit, domain.FacetFilter{}, domain.HybridSearchWeights{Semantic: 0.20, Lexical: 0.80})
		}, true},
		{"rrf-k60", func(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
			return repo.HybridSearchShotsRRF(ctx, q, limit, 60)
		}, true},
	}
	queries := goldenQueries()
	for _, ws := range weightSets {
		totals := goldenMetrics{}
		t.Logf("== weight set %s ==", ws.name)
		for _, gq := range queries {
			results, err := ws.search(ctx, gq.q, 10)
			if err != nil {
				t.Fatal(err)
			}
			relevant := toGoldenIDs(ids, gq.relevant)
			notRelevant := toGoldenIDs(ids, gq.notRelevant)
			// Pure single-signal top-10s, for false-positive attribution.
			lexicalTop := toGoldenIDs(ids, nil)
			semanticTop := toGoldenIDs(ids, nil)
			for _, pure := range pureWeights {
				pureResults, err := repo.hybridSearchShots(ctx, gq.q, 10, domain.FacetFilter{}, pure)
				if err != nil {
					t.Fatal(err)
				}
				for _, hit := range pureResults {
					if pure.Semantic == 1 {
						semanticTop[hit.ID] = true
					} else {
						lexicalTop[hit.ID] = true
					}
				}
			}
			hitCount := 0
			for _, hit := range results {
				if relevant[hit.ID] {
					hitCount++
				}
				if notRelevant[hit.ID] {
					totals.falsePositives++
					switch {
					case lexicalTop[hit.ID]:
						totals.lexicalFPs++
					case semanticTop[hit.ID]:
						totals.semanticFPs++
					default:
						totals.blendEdgeFPs++
					}
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
		t.Logf("P@5=%.3f P@10=%.3f R@10=%.3f relevant-in-top10=%d/%d false-positives=%d (lexical=%d semantic=%d blend-edge=%d)",
			totals.precision5/n, totals.precision10/n, totals.recall10/n, totals.hits, len(queries), totals.falsePositives,
			totals.lexicalFPs, totals.semanticFPs, totals.blendEdgeFPs)
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
		runID, _, err := repo.CreateModelRun(ctx, asset.id, "vision", "fixture", "fixture-model", "golden-"+asset.id, "footage-analysis-v4", "asset-analysis/v2", "{}", "", "")
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := json.Marshal(asset.analysis)
		if err := repo.StageModelRun(ctx, runID, `{"golden":true}`, string(parsed), "", ""); err != nil {
			t.Fatal(err)
		}
		shots := make([]domain.AssetShot, 0, len(asset.shots))
		for i, spec := range asset.shots {
			shots = append(shots, domain.AssetShot{
				// Mint the shot id here rather than letting
				// replaceAssetShotsTx call idgen.New(). Ranking ties are
				// broken by shot id (`worse` in internal/search/retriever.go),
				// so random ids reorder every exactly-tied pair on every run —
				// which is why this corpus reported v2-weighted AssertionFP as
				// 54 or 55 depending on the run, and why two agents auditing
				// the same commit got different numbers and each thought the
				// other had misread. Production ids are stable per shot, so
				// this instability was the fixture's, not the engine's. A
				// deterministic id keeps the benchmark a measurement rather
				// than a coin flip; it does not make the tie order meaningful.
				ID:      fmt.Sprintf("%s-shot-%02d", asset.id, i),
				AssetID: asset.id, SourceRunID: runID, Ordinal: i,
				StartMS: spec.startMS, EndMS: spec.endMS, Description: spec.description,
				Tags: spec.tags, Objects: spec.objects, Actions: spec.actions, Mood: spec.mood,
			})
		}
		if err := repo.CommitAnalysisWithShots(ctx, asset.id, runID, "asset-analysis/v2", asset.analysis, shots, "", ""); err != nil {
			t.Fatal(err)
		}
		if len(asset.transcriptWords) > 0 {
			words := make([]domain.AlignmentWord, 0, len(asset.transcriptWords))
			joined := ""
			for _, w := range asset.transcriptWords {
				confidence := w.confidence
				words = append(words, domain.AlignmentWord{StartMS: w.startMS, EndMS: w.endMS, Text: w.text, Confidence: &confidence})
				joined += w.text + " "
			}
			if err := repo.SaveTranscript(ctx, asset.id, "fixture", "fixture-model", "golden-transcript-"+asset.id, domain.Transcript{Language: "zh", Text: strings.TrimSpace(joined)}, "", ""); err != nil {
				t.Fatal(err)
			}
			if err := repo.SaveAlignment(ctx, asset.id, "fixture", "fixture-model", "golden-align-"+asset.id, "{}", domain.AlignmentResult{Words: words}, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		if asset.session != "" {
			startsAt := time.Now().UTC()
			session := domain.ShootSession{
				ID: asset.session, Title: asset.session, State: "manual",
				AssetIDs: []string{asset.id}, StartsAt: &startsAt,
			}
			if err := repo.SaveShootSession(ctx, session); err != nil {
				t.Fatal(err)
			}
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
