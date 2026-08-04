package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// facetFixture seeds one asset with a probed duration and a committed
// asset_analysis row, so ListAssetCardsFiltered's facet clauses have
// something real to narrow.
type facetFixture struct {
	id           string
	assetType    string
	shotSize     string
	cameraMotion string
	audioType    string
	quality      string
	usableAs     []string
	durationMS   int64
}

func seedFacetFixtures(t *testing.T, repo *Repository, rootID string, fixtures []facetFixture) {
	t.Helper()
	ctx := context.Background()
	now := formatTime(time.Now().UTC())
	for _, fx := range fixtures {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, fx.id, fx.id, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,1,1,1,?)`, "loc-"+fx.id, fx.id, rootID, fx.id+".mov", "/library/"+fx.id+".mov", now); err != nil {
			t.Fatal(err)
		}
		if err := repo.SaveMediaMetadata(ctx, fx.id, domain.MediaMetadata{DurationMS: fx.durationMS}, "facet-fixture"); err != nil {
			t.Fatal(err)
		}
		runID, _, err := repo.CreateModelRun(ctx, fx.id, "vision", "fixture", "fixture-model", "hash-"+fx.id, "facet-prompt-v1", "asset-analysis/v1", "{}")
		if err != nil {
			t.Fatal(err)
		}
		analysis := domain.StructuredAnalysis{
			AssetType:    fx.assetType,
			ShotSize:     fx.shotSize,
			CameraMotion: fx.cameraMotion,
			AudioType:    fx.audioType,
			Quality:      fx.quality,
			UsableAs:     fx.usableAs,
			Summary:      "facet fixture " + fx.id,
		}
		if err := repo.CommitAnalysisWithShots(ctx, fx.id, runID, "asset-analysis/v1", analysis, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func openFacetTestRepo(t *testing.T, name string) (*Repository, string) {
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
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('facet-root','/library',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	return repo, "facet-root"
}

// threeWayFixtures gives every test below the same three assets, chosen so
// no single field distinguishes all of them — asset-type, shot-size and
// quality each split the set differently, which is what lets the AND/OR
// tests below tell "narrows" apart from "matches everything" or "matches
// nothing".
func threeWayFixtures() []facetFixture {
	return []facetFixture{
		{id: "asset-wide-broll", assetType: "b_roll", shotSize: "wide", cameraMotion: "static", audioType: "ambient", quality: "usable", usableAs: []string{"establishing"}, durationMS: 5000},
		{id: "asset-medium-broll", assetType: "b_roll", shotSize: "medium", cameraMotion: "static", audioType: "ambient", quality: "usable", usableAs: []string{"hook"}, durationMS: 3000},
		{id: "asset-closeup-talking", assetType: "talking_to_camera", shotSize: "close_up", cameraMotion: "handheld", audioType: "speech", quality: "excellent", usableAs: []string{"opening"}, durationMS: 8000},
	}
}

func cardIDs(cards []domain.AssetCard) []string {
	ids := make([]string, len(cards))
	for i, c := range cards {
		ids[i] = c.ID
	}
	return ids
}

func assertCardIDs(t *testing.T, got []domain.AssetCard, want ...string) {
	t.Helper()
	gotIDs := cardIDs(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("got ids=%v, want %v", gotIDs, want)
	}
	wantSet := make(map[string]bool, len(want))
	for _, w := range want {
		wantSet[w] = true
	}
	for _, id := range gotIDs {
		if !wantSet[id] {
			t.Fatalf("got ids=%v, want %v", gotIDs, want)
		}
	}
}

func TestListAssetCardsFilteredFacetsEmptyFilterMatchesTodaysBehavior(t *testing.T) {
	repo, rootID := openFacetTestRepo(t, "facets-empty.db")
	seedFacetFixtures(t, repo, rootID, threeWayFixtures())

	cards, err := repo.ListAssetCardsFiltered(context.Background(), domain.AssetCardFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	assertCardIDs(t, cards, "asset-wide-broll", "asset-medium-broll", "asset-closeup-talking")
}

func TestListAssetCardsFilteredSingleFacetSingleValueNarrows(t *testing.T) {
	repo, rootID := openFacetTestRepo(t, "facets-single.db")
	seedFacetFixtures(t, repo, rootID, threeWayFixtures())

	cards, err := repo.ListAssetCardsFiltered(context.Background(), domain.AssetCardFilter{
		Limit:  20,
		Facets: domain.FacetFilter{ShotSizes: []string{"wide"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCardIDs(t, cards, "asset-wide-broll")
}

func TestListAssetCardsFilteredSingleFacetMultipleValuesIsOR(t *testing.T) {
	repo, rootID := openFacetTestRepo(t, "facets-or.db")
	seedFacetFixtures(t, repo, rootID, threeWayFixtures())

	cards, err := repo.ListAssetCardsFiltered(context.Background(), domain.AssetCardFilter{
		Limit:  20,
		Facets: domain.FacetFilter{ShotSizes: []string{"wide", "medium"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCardIDs(t, cards, "asset-wide-broll", "asset-medium-broll")
}

func TestListAssetCardsFilteredTwoFacetsAreAND(t *testing.T) {
	repo, rootID := openFacetTestRepo(t, "facets-and.db")
	seedFacetFixtures(t, repo, rootID, threeWayFixtures())

	// ShotSizes alone would match wide+medium; Qualities alone would match
	// all three (all "usable" except the close-up). Together they must
	// narrow to only the asset satisfying both, proving the two facets are
	// AND'd rather than OR'd across fields.
	cards, err := repo.ListAssetCardsFiltered(context.Background(), domain.AssetCardFilter{
		Limit: 20,
		Facets: domain.FacetFilter{
			ShotSizes: []string{"wide", "medium"},
			Qualities: []string{"usable"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCardIDs(t, cards, "asset-wide-broll", "asset-medium-broll")

	cards, err = repo.ListAssetCardsFiltered(context.Background(), domain.AssetCardFilter{
		Limit: 20,
		Facets: domain.FacetFilter{
			ShotSizes:     []string{"wide", "medium"},
			CameraMotions: []string{"handheld"}, // no asset is both wide/medium and handheld
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 0 {
		t.Fatalf("cards=%v, want none (AND of disjoint facets)", cardIDs(cards))
	}
}

func TestListAssetCardsFilteredUsableAsIsOROverTheList(t *testing.T) {
	repo, rootID := openFacetTestRepo(t, "facets-usableas.db")
	seedFacetFixtures(t, repo, rootID, threeWayFixtures())

	cards, err := repo.ListAssetCardsFiltered(context.Background(), domain.AssetCardFilter{
		Limit:  20,
		Facets: domain.FacetFilter{UsableAs: []string{"hook", "opening"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCardIDs(t, cards, "asset-medium-broll", "asset-closeup-talking")
}

func TestListAssetCardsFilteredDurationBoundsAreInclusive(t *testing.T) {
	repo, rootID := openFacetTestRepo(t, "facets-duration.db")
	seedFacetFixtures(t, repo, rootID, threeWayFixtures())

	min := int64(5000)
	cards, err := repo.ListAssetCardsFiltered(context.Background(), domain.AssetCardFilter{
		Limit:  20,
		Facets: domain.FacetFilter{MinDurationMS: &min},
	})
	if err != nil {
		t.Fatal(err)
	}
	// asset-wide-broll is exactly 5000ms: MinDurationMS must include the
	// boundary, not just strictly-greater durations.
	assertCardIDs(t, cards, "asset-wide-broll", "asset-closeup-talking")

	max := int64(3000)
	cards, err = repo.ListAssetCardsFiltered(context.Background(), domain.AssetCardFilter{
		Limit:  20,
		Facets: domain.FacetFilter{MaxDurationMS: &max},
	})
	if err != nil {
		t.Fatal(err)
	}
	// asset-medium-broll is exactly 3000ms: MaxDurationMS must include the
	// boundary too.
	assertCardIDs(t, cards, "asset-medium-broll")
}

func TestListAssetCardsFilteredFacetCombinesWithCaptureFilters(t *testing.T) {
	repo, rootID := openFacetTestRepo(t, "facets-combine.db")
	seedFacetFixtures(t, repo, rootID, threeWayFixtures())
	if _, err := repo.db.ExecContext(context.Background(), `UPDATE capture_metadata SET region_label='中国 · 深圳' WHERE asset_id='asset-wide-broll'`); err != nil {
		t.Fatal(err)
	}

	// A facet plus an existing (non-facet) filter must intersect: the region
	// filter alone matches only asset-wide-broll, and the shot_size facet
	// alone would also match it, so this doesn't by itself prove
	// intersection — the second case below (region matches, facet excludes)
	// does.
	cards, err := repo.ListAssetCardsFiltered(context.Background(), domain.AssetCardFilter{
		Limit:       20,
		RegionLabel: "中国 · 深圳",
		Facets:      domain.FacetFilter{ShotSizes: []string{"wide"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCardIDs(t, cards, "asset-wide-broll")

	cards, err = repo.ListAssetCardsFiltered(context.Background(), domain.AssetCardFilter{
		Limit:       20,
		RegionLabel: "中国 · 深圳",
		Facets:      domain.FacetFilter{ShotSizes: []string{"medium"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 0 {
		t.Fatalf("cards=%v, want none: region matches asset-wide-broll but shot_size=medium does not", cardIDs(cards))
	}
}
