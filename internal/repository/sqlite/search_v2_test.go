package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/search"
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
	ids = seedOneAsset(t, repo, ids, goldenAssetSpec{
		id: "asset-pagination-burst", analysis: domain.StructuredAnalysis{Summary: "pagination match"},
		shots: []goldenShotSpec{
			{startMS: 0, endMS: 1000, description: "pagination match footage"},
			{startMS: 30_000, endMS: 31_000, description: "pagination match footage"},
			{startMS: 60_000, endMS: 61_000, description: "pagination match footage"},
		}, session: "session-pagination",
	})
	for i := 0; i < 3; i++ {
		ids = seedOneAsset(t, repo, ids, goldenAssetSpec{
			id: fmt.Sprintf("asset-pagination-%d", i), analysis: domain.StructuredAnalysis{Summary: "pagination match"},
			shots: []goldenShotSpec{{startMS: int64(i) * 1000, endMS: int64(i+1) * 1000, description: "pagination match footage"}},
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
	withoutDiversity := page(4, 0, 0)
	for _, id := range withoutDiversity {
		if id == "" {
			t.Fatal("pagination result has empty shot ID")
		}
	}
	withDiversity := page(4, 0, 0.6)
	for _, tc := range []struct {
		diversity float64
		all       []string
	}{
		{0, withoutDiversity}, {0.6, withDiversity},
	} {
		diversity := tc.diversity
		all := tc.all
		first := page(2, 0, diversity)
		second := page(2, 2, diversity)
		if len(all) < 4 || len(second) != 2 {
			t.Fatalf("diversity=%v all=%v second=%v", diversity, all, second)
		}
		if all[2] != second[0] || all[3] != second[1] {
			t.Fatalf("diversity=%v page2=%v, want ranks 3-4 of %v", diversity, second, all)
		}
		seen := map[string]bool{}
		for _, id := range all {
			if seen[id] {
				t.Fatalf("diversity=%v full page contains duplicate ID: %v", diversity, all)
			}
			seen[id] = true
		}
		pageSeen := map[string]bool{}
		for _, id := range first {
			if pageSeen[id] {
				t.Fatalf("diversity=%v page1 contains duplicate ID: %v", diversity, first)
			}
			pageSeen[id] = true
		}
		for _, id := range second {
			if pageSeen[id] {
				t.Fatalf("diversity=%v pages are not disjoint/unique: all=%v second=%v", diversity, all, second)
			}
			pageSeen[id] = true
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

func TestSearchV2TranscriptRequiresExactPhrase(t *testing.T) {
	repo, _ := seedSearchV2Corpus(t)
	defer repo.Close()
	ids := seedOneAsset(t, repo, map[string]string{}, goldenAssetSpec{
		id: "asset-speech-hard-negatives", analysis: domain.StructuredAnalysis{Summary: "speech"},
		shots: []goldenShotSpec{{startMS: 0, endMS: 10_000, description: "partial"}, {startMS: 10_000, endMS: 20_000, description: "reordered"}, {startMS: 20_000, endMS: 30_000, description: "ascii"}},
		transcriptWords: []goldenTranscriptWord{
			{startMS: 100, endMS: 200, text: "我们", confidence: 1},
			{startMS: 10_100, endMS: 10_200, text: "出发", confidence: 1}, {startMS: 10_200, endMS: 10_300, text: "我们", confidence: 1},
			{startMS: 20_100, endMS: 20_200, text: "carefree", confidence: 1},
		},
	})
	hits, err := repo.TranscriptRankedShots(context.Background(), "我们出发", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("partial/reordered phrase candidates must not score, got %+v", hits)
	}
	_ = ids
}

func TestSearchV2TranscriptExactPhraseOnlyScores(t *testing.T) {
	repo, ids := seedSearchV2Corpus(t)
	defer repo.Close()
	hits, err := repo.TranscriptRankedShots(context.Background(), "我们明天出发", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != ids["asset-speech-quote:0"] {
		t.Fatalf("only exact phrase should score, got %+v", hits)
	}
}

func TestSearchV2TranscriptPhrasePrefilterSurvivesPartialSaturation(t *testing.T) {
	repo, _ := seedSearchV2Corpus(t)
	defer repo.Close()
	ids := map[string]string{}
	for i := 0; i < 101; i++ {
		id := fmt.Sprintf("asset-partial-%03d", i)
		ids = seedOneAsset(t, repo, ids, goldenAssetSpec{
			id: id, analysis: domain.StructuredAnalysis{Summary: "partial"},
			shots: []goldenShotSpec{{startMS: 0, endMS: 10_000, description: "partial"}},
			transcriptWords: []goldenTranscriptWord{
				{startMS: 100, endMS: 200, text: "我们", confidence: 1},
				{startMS: 300, endMS: 400, text: "我们", confidence: 1},
				{startMS: 500, endMS: 600, text: "我们", confidence: 1},
				{startMS: 700, endMS: 800, text: "我们", confidence: 1},
				{startMS: 900, endMS: 1000, text: "我们", confidence: 1},
			},
		})
	}
	ids = seedOneAsset(t, repo, ids, goldenAssetSpec{
		id: "asset-exact-after-partials", analysis: domain.StructuredAnalysis{Summary: "exact"},
		shots: []goldenShotSpec{{startMS: 0, endMS: 10_000, description: "exact"}},
		transcriptWords: []goldenTranscriptWord{
			{startMS: 100, endMS: 200, text: "我们", confidence: 1},
			{startMS: 300, endMS: 400, text: "明天", confidence: 1},
			{startMS: 500, endMS: 600, text: "出发", confidence: 1},
		},
	})
	hits, err := repo.TranscriptRankedShots(context.Background(), "我们明天出发", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != ids["asset-exact-after-partials:0"] {
		t.Fatalf("exact phrase must survive partial-token saturation, got %+v", hits)
	}
}

func TestSearchV2TranscriptPhraseSurvivesTenThousandPartialSaturation(t *testing.T) {
	repo, _ := seedSearchV2Corpus(t)
	defer repo.Close()
	// 10,001 partial shots, each holding every phrase character scattered and
	// five matched words (tscore saturates at 1.0 with five), but missing 出 so
	// the (明天→出发) window fails: they must not occupy the bounded pool at
	// all, let alone starve the exact shot ahead of the 10,000 cap.
	seedTranscriptSaturationAssets(t, repo, 10_001)
	ids := map[string]string{}
	ids = seedOneAsset(t, repo, ids, goldenAssetSpec{
		id: "asset-exact-after-saturation", analysis: domain.StructuredAnalysis{Summary: "exact"},
		shots: []goldenShotSpec{{startMS: 0, endMS: 10_000, description: "exact"}},
		transcriptWords: []goldenTranscriptWord{
			{startMS: 100, endMS: 200, text: "我们", confidence: 1},
			{startMS: 300, endMS: 400, text: "明天", confidence: 1},
			{startMS: 500, endMS: 600, text: "出发", confidence: 1},
		},
	})
	hits, err := repo.TranscriptRankedShots(context.Background(), "我们明天出发", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != ids["asset-exact-after-saturation:0"] {
		t.Fatalf("exact phrase must survive >10k partial saturation, got %+v", hits)
	}
}

// seedTranscriptSaturationAssets uses one transaction and prepared statements
// because this fixture is testing the query's bounded candidate pool, not the
// model-run write path. The rows are the same asset/shot/alignment shape the
// production query reads, while avoiding 10,001 full canonical commits.
func seedTranscriptSaturationAssets(t *testing.T, repo *Repository, count int) {
	t.Helper()
	ctx := context.Background()
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	now := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at,health_state) VALUES(?,?,?,?, 'healthy')`, "root-transcript-saturation", "root-transcript-saturation", now, now); err != nil {
		t.Fatal(err)
	}
	assetStmt, err := tx.PrepareContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'ready',?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer assetStmt.Close()
	locationStmt, err := tx.PrepareContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,last_seen_at) VALUES(?,?,?,?,?,0,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer locationStmt.Close()
	shotStmt, err := tx.PrepareContext(ctx, `INSERT INTO asset_shots(id,asset_id,source_run_id,ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at) VALUES(?,?,NULL,0,0,10000,'partial','[]','[]','[]','[]',1,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer shotStmt.Close()
	runStmt, err := tx.PrepareContext(ctx, `INSERT INTO alignment_runs(id,asset_id,provider,model,input_hash,state,request_json,raw_response,created_at,finished_at) VALUES(?,?,?,?,?,'succeeded','{}','{}',?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer runStmt.Close()
	wordStmt, err := tx.PrepareContext(ctx, `INSERT INTO transcript_words(alignment_run_id,asset_id,ordinal,start_ms,end_ms,text,confidence) VALUES(?,?,?,?,?,?,1)`)
	if err != nil {
		t.Fatal(err)
	}
	defer wordStmt.Close()
	for i := 0; i < count; i++ {
		assetID := fmt.Sprintf("asset-sat-%05d", i)
		if _, err := assetStmt.ExecContext(ctx, assetID, "fp-"+assetID, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := locationStmt.ExecContext(ctx, "loc-"+assetID, assetID, "root-transcript-saturation", "clips/"+assetID+".MOV", "clips/"+assetID+".MOV", now); err != nil {
			t.Fatal(err)
		}
		if _, err := shotStmt.ExecContext(ctx, "shot-"+assetID, assetID, now); err != nil {
			t.Fatal(err)
		}
		runID := "align-" + assetID
		if _, err := runStmt.ExecContext(ctx, runID, assetID, "fixture", "fixture-model", "hash-"+assetID, now, now); err != nil {
			t.Fatal(err)
		}
		for ordinal, word := range []string{"我", "们", "明", "天", "发"} {
			if _, err := wordStmt.ExecContext(ctx, runID, assetID, ordinal, int64(100+ordinal*200), int64(200+ordinal*200), word); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestSearchV2TranscriptTieOrderingIsDeterministic(t *testing.T) {
	repo, _ := seedSearchV2Corpus(t)
	defer repo.Close()
	ids := map[string]string{}
	for _, id := range []string{"asset-tie-b", "asset-tie-a"} {
		ids = seedOneAsset(t, repo, ids, goldenAssetSpec{
			id: id, analysis: domain.StructuredAnalysis{Summary: "tie"},
			shots:           []goldenShotSpec{{startMS: 0, endMS: 10_000, description: "tie"}},
			transcriptWords: []goldenTranscriptWord{{startMS: 100, endMS: 200, text: "唯一", confidence: 1}},
		})
	}
	var want []string
	for run := 0; run < 5; run++ {
		hits, err := repo.TranscriptRankedShots(context.Background(), "唯一", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) < 2 {
			t.Fatalf("run %d returned nondeterministic order: %+v", run, hits)
		}
		got := []string{hits[0].ID, hits[1].ID}
		if run == 0 {
			want = got
		} else if got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("run %d returned nondeterministic order: %+v", run, hits)
		}
	}
}

func TestSearchV2EvidenceConflictThroughService(t *testing.T) {
	repo, _ := seedSearchV2Corpus(t)
	defer repo.Close()
	ids := seedOneAsset(t, repo, map[string]string{}, goldenAssetSpec{
		id: "asset-evidence-conflict", analysis: domain.StructuredAnalysis{Summary: "person"},
		shots: []goldenShotSpec{{startMS: 0, endMS: 10_000, description: "no people", objects: []string{"person"}}},
	})
	svc := search.NewService(repo, search.DefaultOptions())
	response, err := svc.Search(context.Background(), search.SearchRequest{Query: "person", Mode: "semantic", Limit: 10, IncludeEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	var conflictResult *search.ResultItem
	for i := range response.Results {
		if response.Results[i].ShotID == ids["asset-evidence-conflict:0"] {
			conflictResult = &response.Results[i]
			break
		}
	}
	if conflictResult == nil {
		t.Fatalf("conflict shot missing from results=%+v", response.Results)
	}
	e := evidenceForSearchValue(conflictResult.Evidence, "person")
	if e == nil || e.State != search.EvidenceContradicted {
		t.Fatalf("evidence=%+v", e)
	}
	if len(e.Sources) != 2 || e.Sources[0] != search.SourceObjects || e.Sources[1] != search.SourceDescription {
		t.Fatalf("sources=%v", e.Sources)
	}
}

func evidenceForSearchValue(evidence []search.Evidence, value string) *search.Evidence {
	for i := range evidence {
		if evidence[i].Value == value {
			return &evidence[i]
		}
	}
	return nil
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

func TestSearchV2MetadataChannelLargeCorpus(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "metadata-large.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('large-root','/large',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1200; i++ {
		id := fmt.Sprintf("large-%04d", i)
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,?,?,?,?)`, id, id, 1, "discovered", now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,0,1,1,?)`, "loc-"+id, id, "large-root", id+"-car.mov", "/large/"+id+"-car.mov", now); err != nil {
			t.Fatal(err)
		}
		if err := repo.ReplaceAssetShots(ctx, id, "", []domain.AssetShot{{ID: "shot-" + id, AssetID: id, Ordinal: 0, EndMS: 1, Description: "test"}}, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	first, err := repo.MetadataRankedShots(ctx, "car", 1200)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1200 {
		t.Fatalf("large metadata result length = %d, want 1200", len(first))
	}
	repeat, err := repo.MetadataRankedShots(ctx, "car", 1200)
	if err != nil {
		t.Fatal(err)
	}
	if len(repeat) != len(first) {
		t.Fatalf("repeated metadata result length = %d, want %d", len(repeat), len(first))
	}
	for i, hit := range first {
		want := fmt.Sprintf("large-%04d", i)
		if hit.AssetID != want || hit.Ordinal != 0 {
			t.Fatalf("result %d = asset %s ordinal %d, want %s/0", i, hit.AssetID, hit.Ordinal, want)
		}
		if repeat[i].ID != hit.ID || repeat[i].AssetID != hit.AssetID || repeat[i].Ordinal != hit.Ordinal {
			t.Fatalf("repeated metadata result differs at %d", i)
		}
	}
	second, err := repo.MetadataRankedShots(ctx, "car", 37)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 37 {
		t.Fatalf("limited metadata result length = %d, want 37", len(second))
	}
	for i := range second {
		if second[i].ID != first[i].ID {
			t.Fatalf("nondeterministic result at %d: %s vs %s", i, second[i].ID, first[i].ID)
		}
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

func TestSearchV2NeighborsBatchNonContiguousOrdinals(t *testing.T) {
	repo, ids := seedSearchV2Corpus(t)
	ctx := context.Background()
	requests := []search.NeighborRequest{
		{ShotID: "first", AssetID: "asset-car-duplicate", Ordinal: 0},
		{ShotID: "last", AssetID: "asset-car-duplicate", Ordinal: 1},
		{ShotID: "unknown", AssetID: "missing", Ordinal: 1},
	}
	got, err := repo.NeighborShotsBatch(ctx, requests)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := got["first"]
	if !ok || first.Previous != nil {
		t.Fatalf("first neighbors = %+v, want no previous: %+v", first, got)
	}
	if first.Next == nil || first.Next.ID != ids["asset-car-duplicate:1"] {
		t.Fatalf("first next = %+v, want ordinal 1: %+v", first.Next, got)
	}
	last, ok := got["last"]
	if !ok || last.Previous == nil || last.Previous.ID != ids["asset-car-duplicate:0"] || last.Next != nil {
		t.Fatalf("last neighbors = %+v, want previous and no next", last)
	}
	if _, ok := got["unknown"]; ok {
		t.Fatalf("missing asset must be omitted from batch map: %+v", got["unknown"])
	}
	empty, err := repo.NeighborShotsBatch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty batch must return an empty map, got %+v", empty)
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

func TestSearchV2ShotTranscriptSpansBatch(t *testing.T) {
	repo, ids := seedSearchV2Corpus(t)
	ctx := context.Background()
	requests := []search.TranscriptSpanRequest{
		{ShotID: ids["asset-speech-quote:0"], AssetID: "asset-speech-quote", StartMS: 95_000, EndMS: 105_000},
		{ShotID: "outside", AssetID: "asset-speech-quote", StartMS: 200_000, EndMS: 300_000},
		{ShotID: ids["asset-speech-quote:0"] + "-second", AssetID: "asset-speech-quote", StartMS: 100_000, EndMS: 101_000},
	}
	got, err := repo.ShotTranscriptSpansBatch(ctx, requests)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[requests[0].ShotID]) != 3 {
		t.Fatalf("batch returned %d overlapping words, want 3: %+v", len(got[requests[0].ShotID]), got)
	}
	if _, ok := got[requests[1].ShotID]; ok {
		t.Fatalf("batch must omit windows without words, got %+v", got[requests[1].ShotID])
	}
	if len(got[requests[2].ShotID]) != 1 {
		t.Fatalf("narrow batch window returned %d words, want 1: %+v", len(got[requests[2].ShotID]), got[requests[2].ShotID])
	}
	for i, word := range got[requests[0].ShotID] {
		if i > 0 && word.StartMS < got[requests[0].ShotID][i-1].StartMS {
			t.Fatalf("batch words are not ordered by ordinal: %+v", got[requests[0].ShotID])
		}
	}
	empty, err := repo.ShotTranscriptSpansBatch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty batch must return an empty map, got %+v", empty)
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
	runID, _, err := repo.CreateModelRun(ctx, asset.id, "vision", "fixture", "fixture-model", "golden-"+asset.id, "footage-analysis-v4", "asset-analysis/v2", "{}", "", "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := jsonMarshal(asset.analysis)
	if err := repo.StageModelRun(ctx, runID, `{"golden":true}`, string(parsed), "", ""); err != nil {
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
		if err := repo.SaveTranscript(ctx, asset.id, "fixture", "fixture-model", "golden-transcript-"+asset.id, domain.Transcript{Language: "zh", Text: joined}, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := repo.SaveAlignment(ctx, asset.id, "fixture", "fixture-model", "golden-align-"+asset.id, "{}", domain.AlignmentResult{Words: words}, "", ""); err != nil {
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

// TestSearchV2AssetContextFilters pins the asset-context candidate universe:
// a semantically matching shot whose owning asset falls outside the selected
// captured-date/ready window is excluded by ScoreCandidatesV2, while the zero
// filter delegates to the legacy scorer unchanged.
func TestSearchV2AssetContextFilters(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "v2-assetcontext.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	ids = seedOneAsset(t, repo, ids, goldenAssetSpec{
		id:       "asset-rain-in",
		analysis: domain.StructuredAnalysis{Summary: "rainy night street"},
		shots:    []goldenShotSpec{{startMS: 0, endMS: 10_000, description: "雨夜城市街道", tags: []string{"rain", "urban_night"}}},
	})
	ids = seedOneAsset(t, repo, ids, goldenAssetSpec{
		id:       "asset-rain-out",
		analysis: domain.StructuredAnalysis{Summary: "rainy night street"},
		shots:    []goldenShotSpec{{startMS: 0, endMS: 10_000, description: "雨夜城市街道", tags: []string{"rain", "urban_night"}}},
	})
	inShot, outShot := ids["asset-rain-in:0"], ids["asset-rain-out:0"]
	if inShot == "" || outShot == "" {
		t.Fatalf("seed shot ids missing: in=%q out=%q", inShot, outShot)
	}

	now := time.Now().UTC()
	inCaptured := now.AddDate(0, 0, -1)
	if err := repo.SaveMediaMetadata(ctx, "asset-rain-in", domain.MediaMetadata{CapturedAt: &inCaptured}, "test", "", ""); err != nil {
		t.Fatal(err)
	}
	outCaptured := now.AddDate(0, 0, -10)
	if err := repo.SaveMediaMetadata(ctx, "asset-rain-out", domain.MediaMetadata{CapturedAt: &outCaptured}, "test", "", ""); err != nil {
		t.Fatal(err)
	}
	// in is "ready" (a proxy artifact exists); out stays "discovered".
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) VALUES(?,?,?,?,?,?,?)`, "proxy-in", "asset-rain-in", "proxy", "sw", "/cache/proxy-in.mp4", 20, now); err != nil {
		t.Fatal(err)
	}

	// Zero asset filter delegates to the legacy scorer: both shots match.
	all, err := repo.ScoreCandidatesV2(ctx, "雨夜 城市 街道", domain.FacetFilter{}, domain.AssetContextFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("zero filter must return both shots, got %d", len(all))
	}

	// Date + status window keeps only the in-window, ready shot.
	from := inCaptured
	to := now.AddDate(0, 0, 1)
	filtered, err := repo.ScoreCandidatesV2(ctx, "雨夜 城市 街道", domain.FacetFilter{}, domain.AssetContextFilter{
		CapturedFrom: &from,
		CapturedTo:   &to,
		Status:       domain.ProcessingStatusReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ID != inShot {
		t.Fatalf("filtered universe = %d shots, want only %q; got %+v", len(filtered), inShot, filtered)
	}

	// The date window alone (no status) also excludes the ten-day-old shot.
	dateOnly, err := repo.ScoreCandidatesV2(ctx, "雨夜 城市 街道", domain.FacetFilter{}, domain.AssetContextFilter{
		CapturedFrom: &from,
		CapturedTo:   &to,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dateOnly) != 1 || dateOnly[0].ID != inShot {
		t.Fatalf("date-only universe = %d shots, want only %q", len(dateOnly), inShot)
	}
}
