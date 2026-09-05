package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// TestLegacyCollectionFilterJSONBackwardCompatibility pins the hard backward-
// compatibility requirement of embedding FacetFilter into
// AssetCollectionFilter: rows written before the change have no facet keys, so
// they must unmarshal to the zero FacetFilter (which matches everything) and an
// existing saved collection must keep returning exactly what it returned
// yesterday — the same cards, the same summary, and a re-marshal that does not
// silently grow new keys into the persisted shape.
func TestLegacyCollectionFilterJSONBackwardCompatibility(t *testing.T) {
	ctx := context.Background()
	repo, rootID := openFacetTestRepo(t, "legacy-collection-filter.db")
	seedFacetFixtures(t, repo, rootID, threeWayFixtures())

	// The fixtures carry no capture date or camera model, but a real legacy
	// filter references both, so give the fixtures the values the old JSON
	// below expects — this proves the non-facet fields still round-trip and
	// filter exactly as they did before FacetFilter existed.
	captured := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	for _, fx := range threeWayFixtures() {
		if _, err := repo.db.ExecContext(ctx, `UPDATE capture_metadata SET captured_at=?, model=? WHERE asset_id=?`, formatTime(captured), "Sony FX3", fx.id); err != nil {
			t.Fatal(err)
		}
	}

	// The pre-change filter shape, written out by hand. Because every old
	// field carried omitempty, only non-empty ones were ever persisted, so
	// this is a faithful copy of a row saved before FacetFilter was embedded.
	legacyJSON := `{"captured_from":"2026-07-01T00:00:00Z","captured_to":"2026-08-01T00:00:00Z","camera_model":"Sony FX3"}`
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_collections(id,name,description,filter_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		"legacy-col", "旧收藏", "saved before facet support", legacyJSON, formatTime(captured), formatTime(captured)); err != nil {
		t.Fatal(err)
	}

	collection, err := repo.GetAssetCollection(ctx, "legacy-col")
	if err != nil {
		t.Fatal(err)
	}
	if collection == nil {
		t.Fatal("legacy collection not found")
	}
	// Missing facet keys must unmarshal to the zero FacetFilter, which matches
	// everything — the backward-compatibility invariant.
	if !reflect.DeepEqual(collection.Filter.FacetFilter, domain.FacetFilter{}) {
		t.Fatalf("legacy filter gained facets on read: %+v", collection.Filter.FacetFilter)
	}

	cards, err := repo.ListAssetCardsInCollection(ctx, "legacy-col", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCardIDs(t, cards, "asset-wide-broll", "asset-medium-broll", "asset-closeup-talking")

	summary, err := repo.GetAssetProcessingSummary(ctx, collection.Filter)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 3 {
		t.Fatalf("summary total=%d, want 3", summary.Total)
	}

	// Re-marshalling must not introduce facet keys, so a re-save of an
	// untouched legacy collection stays byte-shape compatible.
	encoded, err := json.Marshal(collection.Filter)
	if err != nil {
		t.Fatal(err)
	}
	for _, facetKey := range []string{"asset_types", "shot_sizes", "camera_motions", "audio_types", "qualities", "usable_as", "min_duration_ms", "max_duration_ms"} {
		if bytes.Contains(encoded, []byte(facetKey)) {
			t.Fatalf("re-marshalled legacy filter contains facet key %q: %s", facetKey, encoded)
		}
	}
}

// TestSavedCollectionFacetsPersistAndNarrow proves facet support flows through
// the whole saved-collection path: a facet saved into a collection narrows both
// the collection's card list and its processing summary the same way the live
// browse query narrows, not just at parse time.
func TestSavedCollectionFacetsPersistAndNarrow(t *testing.T) {
	ctx := context.Background()
	repo, rootID := openFacetTestRepo(t, "collection-facets.db")
	seedFacetFixtures(t, repo, rootID, threeWayFixtures())

	saved, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{
		Name: "宽镜素材",
		Filter: domain.AssetCollectionFilter{
			FacetFilter: domain.FacetFilter{ShotSizes: []string{"wide"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetAssetCollection(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !reflect.DeepEqual(got.Filter.FacetFilter, domain.FacetFilter{ShotSizes: []string{"wide"}}) {
		t.Fatalf("persisted facets=%+v, want ShotSizes=[wide]", got.Filter.FacetFilter)
	}
	cards, err := repo.ListAssetCardsInCollection(ctx, saved.ID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCardIDs(t, cards, "asset-wide-broll")
	summary, err := repo.GetAssetProcessingSummary(ctx, got.Filter)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 1 {
		t.Fatalf("summary total=%d, want 1 (only the wide asset)", summary.Total)
	}
}

// TestProcessingSummaryCountsUnanalysedAssets guards the LEFT JOIN in
// GetAssetProcessingSummary. An inner join against asset_analysis would
// silently drop every asset that has no asset_analysis row — in a freshly
// scanned library that is nearly all of them — so the count strip would read
// near-zero with no filter applied at all. This assertion is what protects
// that: the unanalysed asset must still be counted.
func TestProcessingSummaryCountsUnanalysedAssets(t *testing.T) {
	ctx := context.Background()
	repo, rootID := openFacetTestRepo(t, "summary-unanalysed.db")
	now := formatTime(time.Now().UTC())
	for _, id := range []string{"analysed-1", "unanalysed-1"} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, id, id, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,1,1,1,?)`, "loc-"+id, id, rootID, id+".mov", "/library/"+id+".mov", now); err != nil {
			t.Fatal(err)
		}
	}
	runID, _, err := repo.CreateModelRun(ctx, "analysed-1", "vision", "fixture", "fixture-model", "hash-analysed-1", "facet-prompt-v1", "asset-analysis/v1", "{}", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, runID, "{}", "{}", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitAnalysisWithShots(ctx, "analysed-1", runID, "asset-analysis/v1", domain.StructuredAnalysis{AssetType: "b_roll", Summary: "analysed only"}, nil, "", ""); err != nil {
		t.Fatal(err)
	}

	summary, err := repo.GetAssetProcessingSummary(ctx, domain.AssetCollectionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 2 {
		t.Fatalf("summary total=%d, want 2 (an asset with no asset_analysis row must still be counted)", summary.Total)
	}
}
