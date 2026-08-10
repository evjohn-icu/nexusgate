package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// seedShotFacetFixtures builds two assets with distinct asset_analysis
// vocabulary and one searchable shot each, so a query that matches both
// shots lexically can then be narrowed by a facet that is true of only one
// asset. All six vocabulary fields live on asset_analysis (one row per
// asset), not on asset_shots, so this is also the fixture shape that proves
// the shot-search facet join resolves through the shot's asset.
func seedShotFacetFixtures(t *testing.T, repo *Repository) {
	t.Helper()
	ctx := context.Background()
	now := formatTime(time.Now().UTC())
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
		runID, _, err := repo.CreateModelRun(ctx, fx.assetID, "vision", "fixture", "fixture-model", "hash-"+fx.assetID, "facet-prompt-v1", "asset-analysis/v1", "{}", "", "")
		if err != nil {
			t.Fatal(err)
		}
		analysis := domain.StructuredAnalysis{
			AssetType: fx.assetType, ShotSize: fx.shotSize, CameraMotion: fx.cameraMotion,
			AudioType: fx.audioType, Quality: fx.quality, Summary: "shot facet fixture " + fx.assetID,
		}
		shots := []domain.AssetShot{{AssetID: fx.assetID, Ordinal: 0, StartMS: fx.startMS, EndMS: fx.endMS, Description: fx.description}}
		if err := repo.StageModelRun(ctx, runID, "{}", "{}", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := repo.CommitAnalysisWithShots(ctx, fx.assetID, runID, "asset-analysis/v1", analysis, shots, "", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func openShotFacetTestRepo(t *testing.T, name string) *Repository {
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

func shotAssetIDs(hits []domain.ShotSearchResult) []string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.AssetID
	}
	return ids
}

func TestSearchShotsFilteredTextQueryIntersectsWithFacetInsteadOfReplacingIt(t *testing.T) {
	repo := openShotFacetTestRepo(t, "shot-facets-intersect.db")
	seedShotFacetFixtures(t, repo)
	ctx := context.Background()

	// Unfiltered, the query matches both shots.
	unfiltered, err := repo.SearchShots(ctx, "雨夜街道", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered) != 2 {
		t.Fatalf("unfiltered hits=%v, want both shots", shotAssetIDs(unfiltered))
	}

	// The same text query plus a facet true of only one asset must narrow to
	// that one shot, not replace the text search with the facet or ignore
	// the facet and return both.
	filtered, err := repo.SearchShotsFiltered(ctx, "雨夜街道", 10, domain.FacetFilter{CameraMotions: []string{"static"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].AssetID != "asset-static-wide" {
		t.Fatalf("filtered hits=%v, want only asset-static-wide", shotAssetIDs(filtered))
	}

	// A facet matching neither asset must intersect down to nothing, not
	// fall back to the unfiltered text results.
	none, err := repo.SearchShotsFiltered(ctx, "雨夜街道", 10, domain.FacetFilter{CameraMotions: []string{"orbit"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("hits=%v, want none", shotAssetIDs(none))
	}
}

func TestSearchShotsFilteredShotDurationBoundsAreInclusiveAndPerShot(t *testing.T) {
	repo := openShotFacetTestRepo(t, "shot-facets-duration.db")
	seedShotFacetFixtures(t, repo)
	ctx := context.Background()

	// asset-static-wide's shot spans exactly 3000ms; MaxDurationMS must
	// include that boundary rather than requiring strictly-shorter.
	max := int64(3000)
	hits, err := repo.SearchShotsFiltered(ctx, "雨夜街道", 10, domain.FacetFilter{MaxDurationMS: &max})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].AssetID != "asset-static-wide" {
		t.Fatalf("hits=%v, want only the 3000ms shot", shotAssetIDs(hits))
	}

	// asset-handheld-close's shot spans exactly 9000ms; MinDurationMS must
	// include that boundary too. This also confirms duration is evaluated
	// per shot span (end_ms-start_ms), not the asset's overall duration —
	// no media_metadata row exists for either asset in this fixture, so a
	// query against asset-level duration would find nothing here.
	min := int64(9000)
	hits, err = repo.SearchShotsFiltered(ctx, "雨夜街道", 10, domain.FacetFilter{MinDurationMS: &min})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].AssetID != "asset-handheld-close" {
		t.Fatalf("hits=%v, want only the 9000ms shot", shotAssetIDs(hits))
	}
}

func TestHybridSearchShotsFilteredIntersectsFacetWithQuery(t *testing.T) {
	repo := openShotFacetTestRepo(t, "shot-facets-hybrid.db")
	seedShotFacetFixtures(t, repo)
	ctx := context.Background()

	unfiltered, err := repo.HybridSearchShots(ctx, "雨夜街道", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered) != 2 {
		t.Fatalf("unfiltered hits=%v, want both shots", shotAssetIDs(unfiltered))
	}

	filtered, err := repo.HybridSearchShotsFiltered(ctx, "雨夜街道", 10, domain.FacetFilter{ShotSizes: []string{"close_up"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].AssetID != "asset-handheld-close" {
		t.Fatalf("filtered hits=%v, want only asset-handheld-close", shotAssetIDs(filtered))
	}
}

func TestSimilarShotsFilteredNarrowsCandidatesByFacet(t *testing.T) {
	repo := openShotFacetTestRepo(t, "shot-facets-similar.db")
	seedShotFacetFixtures(t, repo)
	ctx := context.Background()

	// Find the seeded shot ids to pick a source shot.
	shots, err := repo.ListAssetShots(ctx, "asset-static-wide")
	if err != nil || len(shots) != 1 {
		t.Fatalf("shots=%+v err=%v", shots, err)
	}
	sourceShotID := shots[0].ID

	unfiltered, err := repo.SimilarShots(ctx, sourceShotID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered) != 1 || unfiltered[0].AssetID != "asset-handheld-close" {
		t.Fatalf("unfiltered similar=%v, want the other asset's shot", shotAssetIDs(unfiltered))
	}

	// A facet that excludes the only other shot must leave nothing, proving
	// SimilarShotsFiltered actually applies the facet to candidates rather
	// than ignoring it.
	filtered, err := repo.SimilarShotsFiltered(ctx, sourceShotID, 10, domain.FacetFilter{AudioTypes: []string{"ambient"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 {
		t.Fatalf("filtered similar=%v, want none (only candidate is speech, not ambient)", shotAssetIDs(filtered))
	}
}
