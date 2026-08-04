package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/discovery"
	"github.com/evjohn-icu/timingdex/internal/domain"
)

func TestAssetShotsPersistTimeRangesAndSupportSearch(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-shots.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-shot-1','fp',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	runID, _, err := repo.CreateModelRun(ctx, "asset-shot-1", "vision", "fixture", "fixture-model", "shot-hash", "shot-prompt-v1", "video-analysis/v1", "{}")
	if err != nil {
		t.Fatal(err)
	}
	shots := []domain.AssetShot{
		{AssetID: "asset-shot-1", SourceRunID: runID, Ordinal: 0, StartMS: 12400, EndMS: 18900, Description: "雨夜街道，一个人慢慢走路", Tags: []string{"rain", "street"}, Mood: []string{"cinematic"}, Confidence: 0.93},
		{AssetID: "asset-shot-1", SourceRunID: runID, Ordinal: 1, StartMS: 18900, EndMS: 24000, Description: "霓虹灯和车流", Tags: []string{"urban_night", "traffic"}, Confidence: 0.88},
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-shot-1", runID, shots); err != nil {
		t.Fatal(err)
	}

	got, err := repo.ListAssetShots(ctx, "asset-shot-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].StartMS != 12400 || got[0].EndMS != 18900 || got[1].Ordinal != 1 {
		t.Fatalf("persisted shots=%+v", got)
	}

	hits, err := repo.SearchShots(ctx, "雨夜街道", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].AssetID != "asset-shot-1" || hits[0].StartMS != 12400 || hits[0].EndMS != 18900 {
		t.Fatalf("shot search hits=%+v", hits)
	}
	shortChineseHits, err := repo.SearchShots(ctx, "雨夜", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(shortChineseHits) != 1 || shortChineseHits[0].AssetID != "asset-shot-1" || shortChineseHits[0].StartMS != 12400 {
		t.Fatalf("short Chinese search hits=%+v", shortChineseHits)
	}
}

func TestCJKShotSearchUsesIndexedBigramsInsteadOfSubstringScan(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-cjk-fts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-cjk','fp-cjk',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-cjk", "", []domain.AssetShot{{ID: "shot-cjk", StartMS: 0, EndMS: 5000, Description: "雨夜街道，一个人慢慢走路", Tags: []string{"urban_night", "street"}}}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"雨夜", "街道", "走路", "一个人", "雨夜街道", "雨夜 street"} {
		hits, err := repo.SearchShots(ctx, query, 10)
		if err != nil || len(hits) != 1 || hits[0].ID != "shot-cjk" {
			t.Fatalf("query=%q hits=%+v err=%v", query, hits, err)
		}
	}
	for _, query := range []string{"城市", "海滩"} {
		hits, err := repo.SearchShots(ctx, query, 10)
		if err != nil || len(hits) != 0 {
			t.Fatalf("negative query=%q hits=%+v err=%v", query, hits, err)
		}
	}
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM asset_shot_search WHERE shot_id='shot-cjk'`); err != nil {
		t.Fatal(err)
	}
	hits, err := repo.SearchShots(ctx, "雨夜街道", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("CJK search must not fall back to an unindexed substring scan: hits=%+v", hits)
	}
}

func TestCJKBigramMigrationBackfillsPendingIndex(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-cjk-backfill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-backfill','fp-backfill',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-backfill", "", []domain.AssetShot{{ID: "shot-backfill", StartMS: 0, EndMS: 3000, Description: "雨夜街道"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM asset_shot_search; UPDATE fts_index_state SET value='pending' WHERE name='cjk_bigram_v1'`); err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	hits, err := repo.SearchShots(ctx, "雨夜街道", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != "shot-backfill" {
		t.Fatalf("backfilled hits=%+v err=%v", hits, err)
	}
	var state string
	if err := repo.db.QueryRowContext(ctx, `SELECT value FROM fts_index_state WHERE name='cjk_bigram_v1'`).Scan(&state); err != nil || state != "ready" {
		t.Fatalf("backfill state=%q err=%v", state, err)
	}
}

func TestCommitAnalysisWithShotsKeepsTrustedDataWhenReplacementShotsAreInvalid(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-atomic-analysis.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-atomic','fp-atomic',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	oldRun, _, err := repo.CreateModelRun(ctx, "asset-atomic", "vision", "fixture", "fixture-model", "old-hash", "prompt", "asset-analysis/v1", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, oldRun, "{}", "{}"); err != nil {
		t.Fatal(err)
	}
	oldAnalysis := domain.StructuredAnalysis{AssetType: "b_roll", ShotSize: "wide", CameraMotion: "static", AudioType: "ambient", Lighting: "day", Quality: "usable", Summary: "trusted original analysis"}
	if err := repo.CommitAnalysis(ctx, "asset-atomic", oldRun, "asset-analysis/v1", oldAnalysis); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-atomic", oldRun, []domain.AssetShot{{ID: "trusted-shot", StartMS: 0, EndMS: 5000, Description: "trusted rainy street"}}); err != nil {
		t.Fatal(err)
	}

	newRun, _, err := repo.CreateModelRun(ctx, "asset-atomic", "vision", "fixture", "fixture-model", "new-hash", "prompt", "asset-analysis/v1", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, newRun, "{}", "{}"); err != nil {
		t.Fatal(err)
	}
	err = repo.CommitAnalysisWithShots(ctx, "asset-atomic", newRun, "asset-analysis/v1", domain.StructuredAnalysis{AssetType: "b_roll", ShotSize: "wide", CameraMotion: "static", AudioType: "ambient", Lighting: "night", Quality: "usable", Summary: "untrusted replacement"}, []domain.AssetShot{{StartMS: 6000, EndMS: 5000, Description: "invalid range"}})
	if err == nil {
		t.Fatal("expected invalid shot to reject entire replacement")
	}
	// The refusal is a verdict on the payload, not on the database: these
	// shots would be refused identically forever, and what produced them was
	// a paid model call. isRetryableJobError (internal/app/pipeline.go) now
	// reads that from the error rather than from its wording, so it has to be
	// true of the error a real transaction actually hands back -- an app-level
	// fake could agree with itself about this and still be wrong.
	if !errors.Is(err, domain.ErrPermanentFailure) {
		t.Fatalf("a rejected shot payload must not be retried against a paid provider: %v", err)
	}

	var summary string
	if err := repo.db.QueryRowContext(ctx, `SELECT summary FROM asset_analysis WHERE asset_id='asset-atomic'`).Scan(&summary); err != nil || summary != "trusted original analysis" {
		t.Fatalf("trusted analysis was overwritten: summary=%q err=%v", summary, err)
	}
	shots, err := repo.ListAssetShots(ctx, "asset-atomic")
	if err != nil || len(shots) != 1 || shots[0].ID != "trusted-shot" {
		t.Fatalf("trusted shots were replaced: shots=%+v err=%v", shots, err)
	}
}

func TestHybridShotSearchUsesSemanticFeaturesBeyondLiteralFTS(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-hybrid-shots.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	for _, assetID := range []string{"asset-rain", "asset-report"} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`, assetID, assetID+"-fp", now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-rain", "", []domain.AssetShot{{ID: "shot-rain", StartMS: 0, EndMS: 6000, Description: "雨天的城市夜街和湿地反光", Tags: []string{"rain", "urban_night", "street"}, Mood: []string{"cinematic"}}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-report", "", []domain.AssetShot{{ID: "shot-report", StartMS: 0, EndMS: 6000, Description: "rain report title card", Tags: []string{"report"}}}); err != nil {
		t.Fatal(err)
	}

	hits, err := repo.HybridSearchShots(ctx, "rainy city night", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 || hits[0].ID != "shot-rain" || hits[0].SemanticScore <= hits[1].SemanticScore {
		t.Fatalf("hybrid hits=%+v", hits)
	}
}

func TestSimilarAndRareShotDiscoveryUseLibraryRelativeSemantics(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-discovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	for _, assetID := range []string{"asset-rain", "asset-rain-2", "asset-day-1", "asset-day-2", "asset-day-3"} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`, assetID, assetID+"-fp", now, now); err != nil {
			t.Fatal(err)
		}
	}
	seed := map[string]domain.AssetShot{
		"asset-rain":   {ID: "shot-rain", StartMS: 0, EndMS: 5000, Description: "雨夜城市街道", Tags: []string{"rain", "urban_night", "street"}},
		"asset-rain-2": {ID: "shot-rain-2", StartMS: 0, EndMS: 5000, Description: "雨天城市道路", Tags: []string{"rain", "city", "street"}},
		"asset-day-1":  {ID: "shot-day-1", StartMS: 0, EndMS: 5000, Description: "白天城市街道", Tags: []string{"city", "day", "street"}},
		"asset-day-2":  {ID: "shot-day-2", StartMS: 0, EndMS: 5000, Description: "晴天城市车流", Tags: []string{"city", "day", "traffic"}},
		"asset-day-3":  {ID: "shot-day-3", StartMS: 0, EndMS: 5000, Description: "日间城市建筑", Tags: []string{"city", "day"}},
	}
	for assetID, shot := range seed {
		if err := repo.ReplaceAssetShots(ctx, assetID, "", []domain.AssetShot{shot}); err != nil {
			t.Fatal(err)
		}
	}

	similar, err := repo.SimilarShots(ctx, "shot-rain", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(similar) == 0 || similar[0].ID != "shot-rain-2" {
		t.Fatalf("similar=%+v", similar)
	}
	rare, err := repo.DiscoverRareShots(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rare) == 0 || rare[0].ID != "shot-rain" || rare[0].RarityScore <= 0 || rare[0].Reason == "" {
		t.Fatalf("rare=%+v", rare)
	}
}

func TestReplacingShotsInvalidatesInMemoryFeatureVectorCache(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-vector-cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	for _, assetID := range []string{"cache-a", "cache-b"} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`, assetID, assetID+"-fp", now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.ReplaceAssetShots(ctx, "cache-a", "", []domain.AssetShot{{ID: "cache-shot-a", StartMS: 0, EndMS: 1000, Description: "城市夜景", Tags: []string{"city", "night"}}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAssetShots(ctx, "cache-b", "", []domain.AssetShot{{ID: "cache-shot-b", StartMS: 0, EndMS: 1000, Description: "城市街道", Tags: []string{"city", "street"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SimilarShots(ctx, "cache-shot-a", 10); err != nil {
		t.Fatal(err)
	}
	if repo.semanticVectorCacheLen() == 0 {
		t.Fatal("expected decoded feature vectors to be cached")
	}
	if err := repo.ReplaceAssetShots(ctx, "cache-a", "", []domain.AssetShot{{ID: "cache-shot-a-new", StartMS: 0, EndMS: 1000, Description: "白天建筑", Tags: []string{"day"}}}); err != nil {
		t.Fatal(err)
	}
	if repo.semanticVectorCacheLen() != 0 {
		t.Fatal("replacing shots must invalidate stale feature vectors")
	}
}

func TestSemanticSearchFiltersVectorsFromSupersededModel(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-vector-model.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-scheme','fp-scheme',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-scheme", "", []domain.AssetShot{
		{ID: "shot-valid", StartMS: 0, EndMS: 6000, Description: "雨天的城市夜街和湿地反光", Tags: []string{"rain", "urban_night", "street"}, Mood: []string{"cinematic"}},
		{ID: "shot-stale", StartMS: 0, EndMS: 6000, Description: "雨天的城市夜街和湿地反光", Tags: []string{"rain", "urban_night", "street"}, Mood: []string{"cinematic"}},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := repo.db.ExecContext(ctx, `UPDATE shot_semantic_vectors SET model='superseded-scheme-v0' WHERE shot_id='shot-stale'`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("expected 1 vector row to be marked stale, got %d", n)
	}

	hits, err := repo.HybridSearchShots(ctx, "rainy city night", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.ID == "shot-stale" {
			t.Fatalf("hybrid search returned vector from superseded model: hits=%+v", hits)
		}
	}

	similar, err := repo.SimilarShots(ctx, "shot-valid", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range similar {
		if hit.ID == "shot-stale" {
			t.Fatalf("similar search returned vector from superseded model: similar=%+v", similar)
		}
	}
}

func TestSemanticVectorRejectsWrongDimension(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-vector-dim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	good, err := json.Marshal(discovery.VectorForText("rainy city night"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.semanticVector("shot-good", string(good)); err != nil {
		t.Fatalf("well-formed vector rejected: %v", err)
	}

	wrong := make([]float64, discovery.VectorSize+1)
	encoded, err := json.Marshal(wrong)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.semanticVector("shot-wrong", string(encoded))
	if err == nil {
		t.Fatal("expected dimension mismatch to be rejected")
	}
	for _, want := range []string{"shot-wrong", "65", "64"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must name shot id and both lengths (missing %q)", err, want)
		}
	}
	if _, cached := repo.semanticVectorCache["shot-wrong"]; cached {
		t.Fatal("malformed vector must not be cached")
	}
}

// Tags that differ only in punctuation or spacing normalize to one row. Nothing
// upstream can prevent that: de-duplication there compares lowercased text,
// while the unique index is on a form that also collapses punctuation. A model
// producing both spellings is ordinary, and analysing an asset in several
// windows multiplies the chance of it, so the commit has to tolerate the pair
// rather than fail the whole analysis.
func TestCommitAnalysisKeepsOneRowForTagsThatNormalizeAlike(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-tag-collision.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-tags','fp-tags',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	run, _, err := repo.CreateModelRun(ctx, "asset-tags", "vision", "fixture", "fixture-model", "tag-hash", "prompt", "asset-analysis/v1", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, run, "{}", "{}"); err != nil {
		t.Fatal(err)
	}

	analysis := domain.StructuredAnalysis{
		AssetType: "b_roll", ShotSize: "wide", CameraMotion: "static",
		AudioType: "ambient", Lighting: "night", Quality: "usable",
		Summary: "night street",
		// All three collapse to everyday_urban.
		SceneTags: []string{"everyday urban", "everyday-urban", "everyday  urban"},
		MoodTags:  []string{"calm", "calm."},
	}
	if err := repo.CommitAnalysisWithShots(ctx, "asset-tags", run, "asset-analysis/v1", analysis, nil); err != nil {
		t.Fatalf("tags that normalize alike must not fail the commit: %v", err)
	}

	var scene, mood int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM asset_tag_links WHERE asset_id='asset-tags' AND tag_type='scene'`).Scan(&scene); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM asset_tag_links WHERE asset_id='asset-tags' AND tag_type='mood'`).Scan(&mood); err != nil {
		t.Fatal(err)
	}
	if scene != 1 {
		t.Errorf("three spellings of one scene tag must store one row, got %d", scene)
	}
	if mood != 1 {
		t.Errorf("two spellings of one mood tag must store one row, got %d", mood)
	}
}

// seedTopKLibrary inserts descriptions spread across assets (one ReplaceAssetShots
// call per asset, matching how real analyses land) and returns the shots by ID so
// the reference scorer can reconstruct each vector from the same fields the
// repository hashed.
func seedTopKLibrary(t *testing.T, repo *Repository, descriptions []string) map[string]domain.AssetShot {
	t.Helper()
	ctx := context.Background()
	now := formatTime(time.Now().UTC())
	const perAsset = 10
	byAsset := map[string][]domain.AssetShot{}
	shots := map[string]domain.AssetShot{}
	for i, desc := range descriptions {
		assetID := fmt.Sprintf("asset-topk-%d", i/perAsset)
		shot := domain.AssetShot{ID: fmt.Sprintf("shot-topk-%03d", i), StartMS: 0, EndMS: 5000, Description: desc}
		byAsset[assetID] = append(byAsset[assetID], shot)
		shots[shot.ID] = shot
	}
	for assetID := range byAsset {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`, assetID, assetID+"-fp", now, now); err != nil {
			t.Fatal(err)
		}
		if err := repo.ReplaceAssetShots(ctx, assetID, "", byAsset[assetID]); err != nil {
			t.Fatal(err)
		}
	}
	return shots
}

// referenceSimilarShots reproduces the pre-heap contract: score every other shot
// with the same deterministic vectors the repository stores, sort by score
// descending then shot ID ascending, truncate. The heap must return exactly this
// set and order.
func referenceSimilarShots(shots map[string]domain.AssetShot, sourceID string, limit int) []string {
	query := discovery.VectorForShot(shots[sourceID])
	scored := make([]domain.ShotSearchResult, 0, len(shots))
	for id, shot := range shots {
		if id == sourceID {
			continue
		}
		score := discovery.Cosine(query, discovery.VectorForShot(shot))
		if score > 0 {
			scored = append(scored, domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: id}, Score: score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score == scored[j].Score {
			return scored[i].ID < scored[j].ID
		}
		return scored[i].Score > scored[j].Score
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	ids := make([]string, 0, len(scored))
	for _, r := range scored {
		ids = append(ids, r.ID)
	}
	return ids
}

func assertShotIDOrder(t *testing.T, got []domain.ShotSearchResult, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("result %d = %s, want %s (order matters); got=%+v", i, got[i].ID, id, got)
		}
	}
}

// TestSimilarShotsBoundedTopKMatchesReference proves the heap changed nothing:
// against a 50-shot library whose descriptions repeat in groups (so scores tie
// exactly, stressing the ID tie-break) the bounded top-k must return the same
// set and order as scoring everything then sorting.
func TestSimilarShotsBoundedTopKMatchesReference(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-topk-reference.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pool := []string{
		"雨夜城市街道",
		"雨天城市道路",
		"白天城市街道",
		"晴天城市车流",
		"日间城市建筑",
		"rainy city night street",
		"sunny beach ocean",
		"cinematic moody night film",
		"mountain forest river",
		"bridge market station",
	}
	descriptions := make([]string, 50)
	for i := range descriptions {
		descriptions[i] = pool[i%len(pool)]
	}
	shots := seedTopKLibrary(t, repo, descriptions)

	got, err := repo.SimilarShots(ctx, "shot-topk-000", 8)
	if err != nil {
		t.Fatal(err)
	}
	assertShotIDOrder(t, got, referenceSimilarShots(shots, "shot-topk-000", 8))
}

// TestSimilarShotsBoundedTopKIsDeterministic runs one identical query five
// times. A plain heap is not stable and would reorder exact ties; the total
// order in topShotHeap.Less must make every run agree. Two shots share text so
// their scores are bit-for-bit equal and the tie-break is exercised.
func TestSimilarShotsBoundedTopKIsDeterministic(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-topk-deterministic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	descriptions := []string{
		"雨夜城市街道",
		"rainy city night street",
		"雨夜城市街道", // exact duplicate of the first: identical vector, identical score
		"sunny beach ocean",
		"mountain forest river",
		"白天城市街道",
		"cinematic moody night film",
		"晴天城市车流",
		"bridge market station",
		"harbor garden people",
		"rain rainy night city",
		"daytime urban road",
	}
	seedTopKLibrary(t, repo, descriptions)

	var baseline []string
	for run := 0; run < 5; run++ {
		got, err := repo.SimilarShots(ctx, "shot-topk-000", 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			t.Fatal("expected non-empty results")
		}
		if run == 0 {
			baseline = make([]string, len(got))
			for i, hit := range got {
				baseline[i] = hit.ID
			}
			continue
		}
		assertShotIDOrder(t, got, baseline)
	}
}

// TestSimilarShotsBoundedTopKEdgeLimits covers limit larger than the library
// (must return the full descending order) and limit=1 (must return the best
// shot, not an arbitrary member of a tie group).
func TestSimilarShotsBoundedTopKEdgeLimits(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-topk-limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pool := []string{
		"雨夜城市街道",
		"雨天城市道路",
		"白天城市街道",
		"晴天城市车流",
		"日间城市建筑",
		"rainy city night street",
		"sunny beach ocean",
		"cinematic moody night film",
		"mountain forest river",
		"bridge market station",
	}
	descriptions := make([]string, 50)
	for i := range descriptions {
		descriptions[i] = pool[i%len(pool)]
	}
	shots := seedTopKLibrary(t, repo, descriptions)

	all, err := repo.SimilarShots(ctx, "shot-topk-000", 500)
	if err != nil {
		t.Fatal(err)
	}
	assertShotIDOrder(t, all, referenceSimilarShots(shots, "shot-topk-000", 500))

	one, err := repo.SimilarShots(ctx, "shot-topk-000", 1)
	if err != nil {
		t.Fatal(err)
	}
	assertShotIDOrder(t, one, referenceSimilarShots(shots, "shot-topk-000", 1))
}
