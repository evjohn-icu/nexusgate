package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/search"
)

// The Search v2 persistence tests run against real SQLite (seedGoldenCorpus
// commits through the real write path). The compat equality test is the
// safety net for the legacy-endpoint takeover: search.Service.LegacySearch
// must reproduce repository.HybridSearchShots exactly, or the old GET
// endpoints would change behaviour under the new engine.

func TestSearchV2LexicalFlatEqualsLegacy(t *testing.T) {
	repo, _ := seedGoldenCorpus(t)
	ctx := context.Background()
	for _, q := range []string{"car", "雨夜街道", "beach", "hello", "narrator"} {
		flat, err := repo.LexicalRankedShots(ctx, q, [5]float64{1, 1, 1, 1, 1}, 50)
		if err != nil {
			t.Fatal(err)
		}
		// Legacy lexical scoring, same universe: the bm25 scores must agree.
		legacy, err := repo.scoreShotCandidates(ctx, q, domain.FacetFilter{})
		if err != nil {
			t.Fatal(err)
		}
		legacyByID := map[string]float64{}
		for _, s := range legacy {
			legacyByID[s.ID] = s.LexicalScore
		}
		for _, hit := range flat {
			legacyScore, ok := legacyByID[hit.ID]
			if !ok {
				t.Fatalf("%q: flat lexical surfaced %s that legacy did not score", q, hit.ID)
			}
			if diffAbs(hit.LexicalScore, legacyScore) > 1e-12 {
				t.Fatalf("%q: flat lexical score for %s = %v, legacy = %v", q, hit.ID, hit.LexicalScore, legacyScore)
			}
		}
	}
}

func TestSearchV2OffsetPaginationAgainstSQLite(t *testing.T) {
	repo, ids := seedGoldenCorpus(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		ids = seedOneAsset(t, repo, ids, goldenAssetSpec{
			id:       fmt.Sprintf("asset-pagination-%d", i),
			analysis: domain.StructuredAnalysis{Summary: "pagination match"},
			shots:    []goldenShotSpec{{startMS: int64(i) * 1000, endMS: int64(i+1) * 1000, description: "pagination match footage"}},
		})
	}
	svc := search.NewService(repo, search.DefaultOptions())
	page := func(limit, offset int, diversity float64) []string {
		t.Helper()
		response, err := svc.Search(ctx, search.SearchRequest{Query: "pagination match", Limit: limit, Offset: offset, Diversity: diversity})
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(response.Results))
		for _, result := range response.Results {
			out = append(out, result.ShotID)
		}
		return out
	}
	for _, diversity := range []float64{0, 0.6} {
		all := page(4, 0, diversity)
		second := page(2, 2, diversity)
		if len(all) < 4 || len(second) != 2 {
			t.Fatalf("diversity=%v all=%v second=%v", diversity, all, second)
		}
		if all[2] != second[0] || all[3] != second[1] {
			t.Fatalf("diversity=%v page2=%v, want ranks 3-4 of %v", diversity, second, all)
		}
		seen := map[string]bool{}
		for _, id := range all {
			seen[id] = true
		}
		for _, id := range second {
			if seen[id] && (id == all[0] || id == all[1]) {
				t.Fatalf("diversity=%v pages overlap before page2: all=%v second=%v", diversity, all, second)
			}
		}
	}
}

func TestSearchV2LexicalFieldWeightSelectivity(t *testing.T) {
	repo, _ := seedGoldenCorpus(t)
	ctx := context.Background()
	// A query whose only match lives in the description ("城市夜景霓虹灯")
	// must find nothing when every field weight is zero, and the same shots
	// when the weights are flat.
	objectsOnly, err := repo.LexicalRankedShots(ctx, "城市夜景", [5]float64{0, 0, 0, 0, 0}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(objectsOnly) != 0 {
		t.Fatalf("zeroing all fields must return nothing, got %d", len(objectsOnly))
	}
	flat, err := repo.LexicalRankedShots(ctx, "城市夜景", [5]float64{1, 1, 1, 1, 1}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(flat) == 0 {
		t.Fatal("flat weights must match the description query")
	}
}

func TestSearchV2TranscriptConfinement(t *testing.T) {
	repo, ids := seedSearchV2Corpus(t)
	ctx := context.Background()
	hits, err := repo.TranscriptRankedShots(ctx, "我们", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := ids["asset-speech-quote:0"]
	if len(hits) != 1 || hits[0].ID != want {
		t.Fatalf("transcript '我们' must hit only the overlapping shot, got %+v", hits)
	}
	if hits[0].TranscriptScore <= 0 {
		t.Fatalf("transcript score must be positive, got %v", hits[0].TranscriptScore)
	}
}

func TestSearchV2TranscriptWholeWordDiscipline(t *testing.T) {
	repo, ids := seedSearchV2Corpus(t)
	ctx := context.Background()
	// "care" must NOT match the aligned word "carefree" anywhere in the
	// corpus — the same whole-word rule as the vocabulary.
	hits, err := repo.TranscriptRankedShots(ctx, "care", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.ID == ids["asset-speech-quote:0"] {
			t.Fatalf("'care' must not match transcript words via substring")
		}
	}
}

func TestSearchV2MetadataChannel(t *testing.T) {
	repo, ids := seedSearchV2Corpus(t)
	ctx := context.Background()
	hits, err := repo.MetadataRankedShots(ctx, "car", 10)
	if err != nil {
		t.Fatal(err)
	}
	// The car-duplicate asset's filename contains the whole word "car"; the
	// golden car assets match only their asset-level summary/subjects, which
	// the channel deliberately ignores (asset-global text must not pollute
	// shots — golden case 2).
	if len(hits) != 2 {
		t.Fatalf("filename-only metadata must surface only the car-named asset's shots, got %d", len(hits))
	}
	if hits[0].ID != ids["asset-car-duplicate:0"] || hits[1].ID != ids["asset-car-duplicate:1"] {
		t.Fatalf("metadata must return the asset's shots in ordinal order, got %+v", hits)
	}
	if hits[0].MetadataScore != 1.0 {
		t.Fatalf("filename match must score 1.0, got %v", hits[0].MetadataScore)
	}
}

func TestSearchV2NeighborsNonContiguousOrdinals(t *testing.T) {
	repo, ids := seedSearchV2Corpus(t)
	ctx := context.Background()
	prev, next, err := repo.NeighborShots(ctx, "asset-car-duplicate", 1)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || prev.ID != ids["asset-car-duplicate:0"] {
		t.Fatalf("prev must be ordinal 0, got %+v", prev)
	}
	if next != nil {
		t.Fatalf("no next expected after the last shot, got %+v", next)
	}
}

func TestSearchV2ShotSession(t *testing.T) {
	repo, _ := seedSearchV2Corpus(t)
	ctx := context.Background()
	session, err := repo.ShotSession(ctx, "asset-no-person-beach")
	if err != nil {
		t.Fatal(err)
	}
	if session == "" {
		t.Fatal("asset-no-person-beach must belong to a session")
	}
	none, err := repo.ShotSession(ctx, "asset-car-duplicate")
	if err != nil {
		t.Fatal(err)
	}
	if none != "" {
		t.Fatalf("asset-car-duplicate has no session, got %q", none)
	}
}

func TestSearchV2ShotTranscriptSpans(t *testing.T) {
	repo, _ := seedSearchV2Corpus(t)
	ctx := context.Background()
	spans, err := repo.ShotTranscriptSpans(ctx, "asset-speech-quote", 95_000, 105_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 3 {
		t.Fatalf("want 3 overlapping words, got %+v", spans)
	}
	outside, err := repo.ShotTranscriptSpans(ctx, "asset-speech-quote", 200_000, 300_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(outside) != 0 {
		t.Fatalf("spans must not leak outside the interval, got %+v", outside)
	}
}

// TestSearchV2CompatMatchesLegacy pins the takeover: for every golden query
// (and a few facet variants), search.Service.LegacySearch must return the
// identical top-10 (same IDs, same order, same scores) as
// repository.HybridSearchShots/Filtered.
func TestSearchV2CompatMatchesLegacy(t *testing.T) {
	repo, _ := seedGoldenCorpus(t)
	ctx := context.Background()
	svc := search.NewService(repo, search.DefaultOptions())
	queries := goldenQueries()
	for _, gq := range queries {
		legacy, err := repo.HybridSearchShots(ctx, gq.q, 10)
		if err != nil {
			t.Fatal(err)
		}
		v2, err := svc.LegacySearch(ctx, gq.q, 10, domain.FacetFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(legacy) != len(v2) {
			t.Fatalf("%q: legacy=%d results, v2=%d", gq.q, len(legacy), len(v2))
		}
		for i := range legacy {
			if legacy[i].ID != v2[i].ID {
				t.Fatalf("%q: rank %d legacy=%s v2=%s", gq.q, i, legacy[i].ID, v2[i].ID)
			}
			if diffAbs(legacy[i].Score, v2[i].Score) > 1e-12 {
				t.Fatalf("%q: rank %d score legacy=%v v2=%v", gq.q, i, legacy[i].Score, v2[i].Score)
			}
		}
	}
	facetCases := []struct {
		q      string
		facets domain.FacetFilter
	}{
		{"street", domain.FacetFilter{CameraMotions: []string{"static"}}},
		{"car", domain.FacetFilter{ShotSizes: []string{"wide"}}},
		{"night", domain.FacetFilter{MaxDurationMS: ptrInt64(500_000)}},
	}
	for _, fc := range facetCases {
		legacy, err := repo.HybridSearchShotsFiltered(ctx, fc.q, 10, fc.facets)
		if err != nil {
			t.Fatal(err)
		}
		v2, err := svc.LegacySearch(ctx, fc.q, 10, fc.facets)
		if err != nil {
			t.Fatal(err)
		}
		if len(legacy) != len(v2) {
			t.Fatalf("%q with facets: legacy=%d v2=%d", fc.q, len(legacy), len(v2))
		}
		for i := range legacy {
			if legacy[i].ID != v2[i].ID || diffAbs(legacy[i].Score, v2[i].Score) > 1e-12 {
				t.Fatalf("%q with facets: rank %d legacy=%s(%.12f) v2=%s(%.12f)", fc.q, i, legacy[i].ID, legacy[i].Score, v2[i].ID, v2[i].Score)
			}
		}
	}
}

func ptrInt64(v int64) *int64 { return &v }

func diffAbs(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// seedSearchV2Corpus seeds the golden corpus PLUS the v2 corpus fixtures
// (transcript/session-bearing assets) into one database. The v2 fixtures are
// deliberately separate from goldenCorpus() so the legacy TestRetrievalGolden
// gates are untouched.
func seedSearchV2Corpus(t *testing.T) (*Repository, map[string]string) {
	t.Helper()
	repo, ids := seedGoldenCorpus(t)
	for _, asset := range searchV2Corpus() {
		ids = seedOneAsset(t, repo, ids, asset)
	}
	return repo, ids
}

func seedOneAsset(t *testing.T, repo *Repository, ids map[string]string, asset goldenAssetSpec) map[string]string {
	t.Helper()
	ctx := context.Background()
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`, asset.id, "fp-"+asset.id, now, now); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, "root-fixture-"+asset.id)
	if err != nil {
		t.Fatal(err)
	}
	// The metadata channel reads asset_locations + asset_analysis.
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,last_seen_at) VALUES(?,?,?,?,?,0,?)`, "loc-"+asset.id, asset.id, root.ID, "clips/"+asset.id+".MOV", "clips/"+asset.id+".MOV", now); err != nil {
		t.Fatal(err)
	}
	runID, _, err := repo.CreateModelRun(ctx, asset.id, "vision", "fixture", "fixture-model", "golden-"+asset.id, "footage-analysis-v4", "asset-analysis/v2", "{}")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := jsonMarshal(asset.analysis)
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
	if len(asset.transcriptWords) > 0 {
		words := make([]domain.AlignmentWord, 0, len(asset.transcriptWords))
		joined := ""
		for _, w := range asset.transcriptWords {
			confidence := w.confidence
			words = append(words, domain.AlignmentWord{StartMS: w.startMS, EndMS: w.endMS, Text: w.text, Confidence: &confidence})
			joined += w.text + " "
		}
		if err := repo.SaveTranscript(ctx, asset.id, "fixture", "fixture-model", "golden-transcript-"+asset.id, domain.Transcript{Language: "zh", Text: joined}); err != nil {
			t.Fatal(err)
		}
		if err := repo.SaveAlignment(ctx, asset.id, "fixture", "fixture-model", "golden-align-"+asset.id, "{}", domain.AlignmentResult{Words: words}); err != nil {
			t.Fatal(err)
		}
	}
	if asset.session != "" {
		startsAt := time.Now().UTC()
		if err := repo.SaveShootSession(ctx, domain.ShootSession{
			ID: asset.session, Title: asset.session, State: "manual",
			AssetIDs: []string{asset.id}, StartsAt: &startsAt,
		}); err != nil {
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
	return ids
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// TestSearchV2ShotSessionsBatch covers the batch session lookup the
// diversity pass consumes. The corpus seeds two session-bearing assets
// (session-beach, session-lonely) through SaveShootSession; a third asset
// is seeded without one. ShotSessions must return the two mappings, never
// mention the sessionless or unknown assets, and agree with the single-shot
// ShotSession for the same asset.
func TestSearchV2ShotSessionsBatch(t *testing.T) {
	repo, _ := seedSearchV2Corpus(t)
	ctx := context.Background()
	seedOneAsset(t, repo, map[string]string{}, goldenAssetSpec{
		id:       "asset-no-session",
		analysis: domain.StructuredAnalysis{Summary: "plain clip"},
		shots: []goldenShotSpec{
			{startMS: 0, endMS: 10_000, description: "plain clip"},
		},
	})

	got, err := repo.ShotSessions(ctx, []string{
		"asset-no-person-beach", "asset-lonely-night", "asset-no-session",
		"asset-does-not-exist",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"asset-no-person-beach": "session-beach",
		"asset-lonely-night":    "session-lonely",
	}
	if len(got) != len(want) {
		t.Fatalf("ShotSessions returned %d mappings, want %d: %+v", len(got), len(want), got)
	}
	for assetID, sessionID := range want {
		if got[assetID] != sessionID {
			t.Fatalf("ShotSessions[%s]=%q, want %q (full: %+v)", assetID, got[assetID], sessionID, got)
		}
	}
	for _, assetID := range []string{"asset-no-session", "asset-does-not-exist"} {
		if _, present := got[assetID]; present {
			t.Fatalf("ShotSessions must not map %s, got %+v", assetID, got)
		}
	}
	single, err := repo.ShotSession(ctx, "asset-no-person-beach")
	if err != nil {
		t.Fatal(err)
	}
	if single != "session-beach" {
		t.Fatalf("ShotSession=%q, want session-beach (batch must agree with single-shot)", single)
	}
	singleNone, err := repo.ShotSession(ctx, "asset-no-session")
	if err != nil {
		t.Fatal(err)
	}
	if singleNone != "" {
		t.Fatalf("ShotSession for sessionless asset=%q, want \"\"", singleNone)
	}
}

func TestSearchV2ShotSessionsEmptyInput(t *testing.T) {
	repo, _ := seedSearchV2Corpus(t)
	ctx := context.Background()
	for _, ids := range [][]string{nil, {}} {
		got, err := repo.ShotSessions(ctx, ids)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("ShotSessions(%v) must return an empty map, got %+v", ids, got)
		}
	}
}
