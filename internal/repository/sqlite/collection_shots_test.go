package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// seedShotBasketFixture inserts a root, an asset, its primary location and a
// shot with a known duration, returning the shot id. Raw SQL keeps the ids
// and timings under test control.
func seedShotBasketFixture(t *testing.T, repo *Repository, assetID, shotID string, startMS, endMS int64) {
	t.Helper()
	ctx := context.Background()
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT OR IGNORE INTO library_roots(id,path,created_at,updated_at) VALUES('basket-root','/nas/library',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, assetID, assetID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,1,1,1,?)`,
		"loc-"+shotID, assetID, "basket-root", assetID+".mov", "/nas/library/"+assetID+".mov", now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_shots(id,asset_id,source_run_id,ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at) VALUES(?,?,NULL,0,?,?,'shot '||?, '[]', '["person"]', '[]', '[]', 0.9, ?)`,
		shotID, assetID, startMS, endMS, shotID, now); err != nil {
		t.Fatal(err)
	}
}

func TestCollectionShotBasketAppendsAndLists(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "basket.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	collection, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{Name: "精选镜头"})
	if err != nil {
		t.Fatal(err)
	}
	seedShotBasketFixture(t, repo, "asset-a", "shot-a", 0, 4000)
	seedShotBasketFixture(t, repo, "asset-b", "shot-b", 1000, 3500)

	if err := repo.AddShotToCollection(ctx, collection.ID, "shot-a"); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddShotToCollection(ctx, collection.ID, "shot-b"); err != nil {
		t.Fatal(err)
	}
	// Re-adding a pinned shot is a documented no-op: same row, same position.
	if err := repo.AddShotToCollection(ctx, collection.ID, "shot-a"); err != nil {
		t.Fatal(err)
	}

	shots, err := repo.ListCollectionShots(ctx, collection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 2 {
		t.Fatalf("basket=%+v, want two pinned shots", shots)
	}
	if shots[0].ShotID != "shot-a" || shots[0].Position != 0 || shots[0].StartMS != 0 || shots[0].EndMS != 4000 {
		t.Fatalf("first basket entry=%+v, want shot-a at position 0", shots[0])
	}
	if shots[1].ShotID != "shot-b" || shots[1].Position != 1 {
		t.Fatalf("second basket entry=%+v, want shot-b at position 1", shots[1])
	}
	if shots[0].AssetID != "asset-a" || shots[0].Filename != "asset-a.mov" || shots[0].Description != "shot shot-a" {
		t.Fatalf("basket detail missing join: %+v", shots[0])
	}
	if len(shots[0].Objects) != 1 || shots[0].Objects[0] != "person" {
		t.Fatalf("basket detail objects=%v, want decoded objects_json", shots[0].Objects)
	}

	// Remove is idempotent too: removing a non-member row deletes nothing.
	if err := repo.RemoveShotFromCollection(ctx, collection.ID, "shot-a"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveShotFromCollection(ctx, collection.ID, "shot-a"); err != nil {
		t.Fatal(err)
	}
	shots, err = repo.ListCollectionShots(ctx, collection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 1 || shots[0].ShotID != "shot-b" {
		t.Fatalf("after removal basket=%+v, want shot-b only", shots)
	}
}

func TestCollectionShotRejectsUnknownShot(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "basket-unknown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	collection, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{Name: "空篮子"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AddShotToCollection(ctx, collection.ID, "shot-ghost"); err == nil {
		t.Fatal("adding a nonexistent shot must error")
	}
	shots, err := repo.ListCollectionShots(ctx, collection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 0 {
		t.Fatalf("basket=%+v, want empty after rejected add", shots)
	}
}

func TestCollectionShotReorderAssignsPositions(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "basket-reorder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	collection, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{Name: "排列"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"shot-a", "shot-b", "shot-c"} {
		seedShotBasketFixture(t, repo, "asset-"+id, id, 0, 1000)
		if err := repo.AddShotToCollection(ctx, collection.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.ReorderCollectionShots(ctx, collection.ID, []string{"shot-c", "shot-a", "shot-b"}); err != nil {
		t.Fatal(err)
	}
	shots, err := repo.ListCollectionShots(ctx, collection.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"shot-c", "shot-a", "shot-b"}
	for i, shot := range shots {
		if shot.ShotID != want[i] || shot.Position != i {
			t.Fatalf("reordered basket[%d]=%+v, want %s at position %d", i, shot, want[i], i)
		}
	}

	// A stale list is rejected whole: fewer ids and foreign ids both fail.
	if err := repo.ReorderCollectionShots(ctx, collection.ID, []string{"shot-c", "shot-a"}); err == nil {
		t.Fatal("reorder with a shortened list must error")
	}
	if err := repo.ReorderCollectionShots(ctx, collection.ID, []string{"shot-c", "shot-a", "shot-foreign"}); err == nil {
		t.Fatal("reorder with an unowned shot must error")
	}
	if err := repo.ReorderCollectionShots(ctx, collection.ID, []string{"shot-c", "shot-c", "shot-a"}); err == nil {
		t.Fatal("reorder with a duplicate id must error")
	}
	// Nothing changed: the failed reorders must not have moved rows.
	shots, err = repo.ListCollectionShots(ctx, collection.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, shot := range shots {
		if shot.ShotID != want[i] || shot.Position != i {
			t.Fatalf("failed reorder moved the basket: %+v", shots)
		}
	}
}

func TestCollectionSummaryCountsShotsAndDuration(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "basket-summary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	collection, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{Name: "时长统计"})
	if err != nil {
		t.Fatal(err)
	}
	seedShotBasketFixture(t, repo, "asset-x", "shot-x", 0, 5000)
	seedShotBasketFixture(t, repo, "asset-y", "shot-y", 2000, 8000)

	got, err := repo.GetAssetCollection(ctx, collection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ShotCount != 0 || got.TotalDurationMS != 0 {
		t.Fatalf("empty basket summary=%+v, want zero aggregates", got)
	}
	if err := repo.AddShotToCollection(ctx, collection.ID, "shot-x"); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddShotToCollection(ctx, collection.ID, "shot-y"); err != nil {
		t.Fatal(err)
	}
	got, err = repo.GetAssetCollection(ctx, collection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ShotCount != 2 || got.TotalDurationMS != 11000 {
		t.Fatalf("summary=%+v, want 2 shots / 11000ms", got)
	}
	collections, err := repo.ListAssetCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(collections) != 1 || collections[0].ShotCount != 2 || collections[0].TotalDurationMS != 11000 || collections[0].Name != "时长统计" {
		t.Fatalf("list summary=%+v, want aggregates on the list too", collections)
	}
}

func TestDeleteCollectionCascadesPinnedShots(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "basket-cascade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	collection, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{Name: "级联删除"})
	if err != nil {
		t.Fatal(err)
	}
	seedShotBasketFixture(t, repo, "asset-c", "shot-c", 0, 1000)
	if err := repo.AddShotToCollection(ctx, collection.ID, "shot-c"); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteAssetCollection(ctx, collection.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	// foreign_keys is on (see repository.go dsn), so the FK — not a manual
	// delete — must have removed the pinned row.
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM collection_shots WHERE collection_id=?`, collection.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("deleting the collection left %d pinned rows behind", count)
	}
}

// A re-analysis replaces a shot row with a fresh id (replaceAssetShotsTx
// delete+reinsert), so a pin on the old shot must cascade away with it —
// otherwise the basket would silently drop the shot from the join while
// overcounting shot_count and rejecting reorders.
func TestShotDeletionCascadesPinnedShots(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "basket-shot-cascade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	collection, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{Name: "镜头级联"})
	if err != nil {
		t.Fatal(err)
	}
	seedShotBasketFixture(t, repo, "asset-d", "shot-d", 0, 1000)
	if err := repo.AddShotToCollection(ctx, collection.ID, "shot-d"); err != nil {
		t.Fatal(err)
	}
	// The re-analysis shape: the old shot row is deleted, a fresh id appears.
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM asset_shots WHERE id=?`, "shot-d"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM collection_shots WHERE collection_id=?`, collection.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("deleting the shot left %d pinned rows behind", count)
	}
	// And the summary no longer overcounts.
	got, err := repo.GetAssetCollection(ctx, collection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ShotCount != 0 {
		t.Fatalf("summary after shot cascade=%+v, want ShotCount 0", got)
	}
}
