package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

func openShotJoinTestRepo(t *testing.T, name string) *Repository {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return repo
}

// seedShotWithoutAnalysis creates an asset + shot with NO asset_analysis row.
// Uses ReplaceAssetShots to correctly populate asset_shots, FTS, and vectors.
func seedShotWithoutAnalysis(t *testing.T, repo *Repository) (assetID, shotID string) {
	t.Helper()
	ctx := context.Background()
	assetID = "no-analysis-asset"
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('shot-join-root','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`,
		assetID, assetID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,'shot-join-root',?,?,1,1,1,?)`,
		"loc-no-analysis", assetID, "no-analysis.mov", "/footage/no-analysis.mov", now); err != nil {
		t.Fatal(err)
	}
	// Create a model run and use ReplaceAssetShots to populate everything.
	runID, _, err := repo.CreateModelRun(ctx, assetID, "vision", "fixture", "fixture-model", "hash-"+assetID, "shot-prompt-v1", "video-analysis/v1", "{}")
	if err != nil {
		t.Fatal(err)
	}
	shots := []domain.AssetShot{
		{AssetID: assetID, Ordinal: 0, StartMS: 0, EndMS: 5000, Description: "测试无分析镜头", Tags: []string{}, Objects: []string{}, Actions: []string{}, Mood: []string{}, Confidence: 0.9},
	}
	if err := repo.ReplaceAssetShots(ctx, assetID, runID, shots); err != nil {
		t.Fatal(err)
	}
	// Get the shot ID that ReplaceAssetShots assigned.
	stored, err := repo.ListAssetShots(ctx, assetID)
	if err != nil || len(stored) != 1 {
		t.Fatalf("ListAssetShots: got %d shots, err=%v", len(stored), err)
	}
	return assetID, stored[0].ID
}

// seedShotWithAnalysisFixtures builds two assets with asset_analysis rows
// and one searchable shot each, similar to seedShotFacetFixtures but using
// StageModelRun → CommitAnalysisWithShots (the post-0010 gate).
func seedShotWithAnalysisFixtures(t *testing.T, repo *Repository) {
	t.Helper()
	ctx := context.Background()
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT OR IGNORE INTO library_roots(id,path,created_at,updated_at) VALUES('shot-join-analysis-root','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	type fixture struct {
		assetID, description string
		startMS, endMS       int64
		assetType, shotSize  string
		cameraMotion         string
		audioType, quality   string
	}
	fixtures := []fixture{
		{assetID: "asset-static-wide", description: "雨夜街道，静止镜头", startMS: 0, endMS: 3000, assetType: "b_roll", shotSize: "wide", cameraMotion: "static", audioType: "ambient", quality: "usable"},
		{assetID: "asset-handheld-close", description: "雨夜街道，手持特写", startMS: 0, endMS: 9000, assetType: "talking_to_camera", shotSize: "close_up", cameraMotion: "handheld", audioType: "speech", quality: "excellent"},
	}
	for _, fx := range fixtures {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, fx.assetID, fx.assetID, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,'shot-join-analysis-root',?,?,1,1,1,?)`,
			"loc-"+fx.assetID, fx.assetID, fx.assetID+".mov", "/footage/"+fx.assetID+".mov", now); err != nil {
			t.Fatal(err)
		}
		runID, _, err := repo.CreateModelRun(ctx, fx.assetID, "vision", "fixture", "fixture-model", "hash-"+fx.assetID, "facet-prompt-v1", "asset-analysis/v1", "{}")
		if err != nil {
			t.Fatal(err)
		}
		analysis := domain.StructuredAnalysis{
			AssetType: fx.assetType, ShotSize: fx.shotSize, CameraMotion: fx.cameraMotion,
			AudioType: fx.audioType, Quality: fx.quality, Summary: "shot facet fixture " + fx.assetID,
		}
		shots := []domain.AssetShot{{AssetID: fx.assetID, Ordinal: 0, StartMS: fx.startMS, EndMS: fx.endMS, Description: fx.description}}
		if err := repo.StageModelRun(ctx, runID, "{}", "{}"); err != nil {
			t.Fatal(err)
		}
		if err := repo.CommitAnalysisWithShots(ctx, fx.assetID, runID, "asset-analysis/v1", analysis, shots); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSearchShotsZeroFacetFindsShotWithoutAnalysis(t *testing.T) {
	repo := openShotJoinTestRepo(t, "shot-join-zero.db")
	assetID, shotID := seedShotWithoutAnalysis(t, repo)
	ctx := context.Background()

	hits, err := repo.SearchShots(ctx, "测试无分析镜头", 10)
	if err != nil {
		t.Fatalf("SearchShots: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.ID == shotID && h.AssetID == assetID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("zero-facet search did not find shot %s (asset %s); hits=%d", shotID, assetID, len(hits))
	}
}

func TestSearchShotsFilteredExcludesShotWithoutAnalysis(t *testing.T) {
	repo := openShotJoinTestRepo(t, "shot-join-filtered.db")
	assetID, shotID := seedShotWithoutAnalysis(t, repo)
	ctx := context.Background()

	// Verify the shot is searchable without facets first.
	unfiltered, err := repo.SearchShots(ctx, "测试无分析镜头", 10)
	if err != nil {
		t.Fatalf("SearchShots(unfiltered): %v", err)
	}
	found := false
	for _, h := range unfiltered {
		if h.ID == shotID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("shot %s not found in unfiltered search; hits=%d", shotID, len(unfiltered))
	}

	// A facet that would match any b_roll shot must NOT match this one,
	// because the LEFT JOIN produces NULL for all an.* columns when no
	// asset_analysis row exists.
	filtered, err := repo.SearchShotsFiltered(ctx, "测试无分析镜头", 10, domain.FacetFilter{
		AssetTypes: []string{"b_roll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range filtered {
		if h.ID == shotID && h.AssetID == assetID {
			t.Fatalf("faceted search unexpectedly found shot %s (asset %s) which has no asset_analysis row", shotID, assetID)
		}
	}
}

func TestSearchShotsFilteredWithAnalysisNarrowsCorrectly(t *testing.T) {
	repo := openShotJoinTestRepo(t, "shot-join-narrow.db")
	seedShotWithAnalysisFixtures(t, repo)
	ctx := context.Background()

	// Unfiltered: both shots.
	unfiltered, err := repo.SearchShots(ctx, "雨夜街道", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered) != 2 {
		t.Fatalf("unfiltered hits=%v, want both shots", shotAssetIDs(unfiltered))
	}

	// With facet: only one shot.
	filtered, err := repo.SearchShotsFiltered(ctx, "雨夜街道", 10, domain.FacetFilter{
		CameraMotions: []string{"static"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].AssetID != "asset-static-wide" {
		t.Fatalf("filtered hits=%v, want only asset-static-wide", shotAssetIDs(filtered))
	}

	// Duration-only facet: no an.* columns needed — the LEFT JOIN is
	// skipped when there are no an.* clauses, and the WHERE is just
	// MATCH + duration bound. This confirms the conditional join does
	// not break duration filtering.
	max := int64(3000)
	durHits, err := repo.SearchShotsFiltered(ctx, "雨夜街道", 10, domain.FacetFilter{MaxDurationMS: &max})
	if err != nil {
		t.Fatal(err)
	}
	if len(durHits) != 1 || durHits[0].AssetID != "asset-static-wide" {
		t.Fatalf("duration-filtered hits=%v, want only the 3000ms shot", shotAssetIDs(durHits))
	}
}
