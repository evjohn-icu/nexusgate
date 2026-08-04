package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// TestListAssetCardsFilteredTieBreakAcrossPages ensures that pagination over
// assets with identical COALESCE(captured_at) values is deterministic: no
// duplicates across pages and no missing assets.  Without a secondary
// sort key (a.id DESC in the ORDER BY clause), SQLite may return rows in an
// arbitrary physical order when the primary sort key ties, causing repeated
// or dropped assets across pages.
func TestListAssetCardsFilteredTieBreakAcrossPages(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "tiebreak.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root','/library',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}

	// Seed 5 assets, all with the same captured_at but distinct IDs.
	const nAssets = 5
	assetIDs := []string{"tie-a", "tie-b", "tie-c", "tie-d", "tie-e"}
	captured := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	for _, id := range assetIDs {
		if _, err := repo.db.ExecContext(ctx,
			`INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`,
			id, id, now, now,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx,
			`INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,'root',?,?,1,1,1,?)`,
			"loc-"+id, id, id+".mov", "/library/"+id+".mov", now,
		); err != nil {
			t.Fatal(err)
		}
		// Assign the same captured_at via media_metadata; capture_metadata
		// captures it from images but media_metadata.captured_at is what
		// the COALESCE falls through to in this fixture.
		c := captured
		if err := repo.SaveMediaMetadata(ctx, id, domain.MediaMetadata{CapturedAt: &c}, "tiebreak"); err != nil {
			t.Fatal(err)
		}
	}

	// Page 1: first 3 items.
	page1, err := repo.ListAssetCardsFiltered(ctx, domain.AssetCardFilter{Limit: 3, Offset: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 3 {
		t.Fatalf("page1: want 3, got %d (%v)", len(page1), cardIDs(page1))
	}

	// Page 2: remaining 2 items.
	page2, err := repo.ListAssetCardsFiltered(ctx, domain.AssetCardFilter{Limit: 3, Offset: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2: want 2, got %d (%v)", len(page2), cardIDs(page2))
	}

	// Page 3 past end: zero.
	page3, err := repo.ListAssetCardsFiltered(ctx, domain.AssetCardFilter{Limit: 3, Offset: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(page3) != 0 {
		t.Fatalf("page3: want 0, got %d (%v)", len(page3), cardIDs(page3))
	}

	// Union of page1 + page2 must have 5 unique IDs — no duplicates.
	seen := make(map[string]bool, nAssets)
	for _, c := range page1 {
		if seen[c.ID] {
			t.Fatalf("duplicate %s in page1", c.ID)
		}
		seen[c.ID] = true
	}
	for _, c := range page2 {
		if seen[c.ID] {
			t.Fatalf("duplicate %s across pages", c.ID)
		}
		seen[c.ID] = true
	}
	if len(seen) != nAssets {
		t.Fatalf("union size: want %d, got %d (missing: want %v)", nAssets, len(seen), setDifference(assetIDs, seen))
	}

	// Stability: the same page queried again must return the same IDs in the
	// same order — a secondary sort key guarantees deterministic ordering.
	page1again, err := repo.ListAssetCardsFiltered(ctx, domain.AssetCardFilter{Limit: 3, Offset: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1again) != len(page1) {
		t.Fatalf("page1 stability: lengths differ (%d vs %d)", len(page1again), len(page1))
	}
	for i := range page1 {
		if page1again[i].ID != page1[i].ID {
			t.Fatalf("page1 stability: position %d: first=%q second=%q", i, page1[i].ID, page1again[i].ID)
		}
	}
}

func setDifference(all []string, have map[string]bool) []string {
	var missing []string
	for _, id := range all {
		if !have[id] {
			missing = append(missing, id)
		}
	}
	return missing
}
