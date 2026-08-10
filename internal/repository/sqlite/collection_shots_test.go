package sqlite

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"sync"
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

func TestCollectionShotRemoveCompactsPositionsBeforeAppend(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "basket-remove-compact.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	collection, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{Name: "移除压紧"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"shot-rm-a", "shot-rm-b", "shot-rm-c"} {
		seedShotBasketFixture(t, repo, "asset-"+id, id, 0, 1000)
		if err := repo.AddShotToCollection(ctx, collection.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.RemoveShotFromCollection(ctx, collection.ID, "shot-rm-b"); err != nil {
		t.Fatal(err)
	}
	shots, err := repo.ListCollectionShots(ctx, collection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 2 || shots[0].ShotID != "shot-rm-a" || shots[0].Position != 0 || shots[1].ShotID != "shot-rm-c" || shots[1].Position != 1 {
		t.Fatalf("after middle removal=%+v, want positions 0 and 1", shots)
	}
	seedShotBasketFixture(t, repo, "asset-rm-d", "shot-rm-d", 0, 1000)
	if err := repo.AddShotToCollection(ctx, collection.ID, "shot-rm-d"); err != nil {
		t.Fatal(err)
	}
	shots, err = repo.ListCollectionShots(ctx, collection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 3 || shots[2].ShotID != "shot-rm-d" || shots[2].Position != 2 {
		t.Fatalf("after append=%+v, want new shot at position 2", shots)
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
	// The re-analysis shape: ReplaceAssetShots deletes the old row and inserts a
	// fresh id in one transaction, so the old pin must cascade away.
	if err := repo.ReplaceAssetShots(ctx, "asset-d", "", []domain.AssetShot{{ID: "shot-d-new", StartMS: 0, EndMS: 1200, Description: "new shot"}}); err != nil {
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
	if err := repo.AddShotToCollection(ctx, collection.ID, "shot-d-new"); err != nil {
		t.Fatal(err)
	}
	shots, err := repo.ListCollectionShots(ctx, collection.ID)
	if err != nil || len(shots) != 1 || shots[0].ShotID != "shot-d-new" || shots[0].Position != 0 {
		t.Fatalf("replacement pin=%+v err=%v, want new shot at position 0", shots, err)
	}
}

func TestCollectionConcurrentAppendsHaveUniquePositions(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "concurrent-append.db")
	repo1, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo1.Close()
	if err := repo1.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo2.Close()
	collection, err := repo1.SaveAssetCollection(ctx, domain.AssetCollection{Name: "并发追加"})
	if err != nil {
		t.Fatal(err)
	}
	seedShotBasketFixture(t, repo1, "asset-ca", "shot-ca", 0, 1000)
	seedShotBasketFixture(t, repo1, "asset-cb", "shot-cb", 0, 1000)
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, item := range []struct {
		repo *Repository
		shot string
	}{{repo1, "shot-ca"}, {repo2, "shot-cb"}} {
		wg.Add(1)
		go func(repo *Repository, shot string) {
			defer wg.Done()
			<-barrier
			errs <- repo.AddShotToCollection(ctx, collection.ID, shot)
		}(item.repo, item.shot)
	}
	close(barrier)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count, distinct int
	if err := repo1.db.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(DISTINCT position) FROM collection_shots WHERE collection_id=?`, collection.ID).Scan(&count, &distinct); err != nil {
		t.Fatal(err)
	}
	if count != 2 || distinct != 2 {
		t.Fatalf("count=%d distinct=%d, want two uniquely positioned rows", count, distinct)
	}
	var positions []int
	rows, err := repo1.db.QueryContext(ctx, `SELECT position FROM collection_shots WHERE collection_id=? ORDER BY position`, collection.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var position int
		if err := rows.Scan(&position); err != nil {
			t.Fatal(err)
		}
		positions = append(positions, position)
	}
	if rows.Err() != nil || len(positions) != 2 || positions[0] != 0 || positions[1] != 1 {
		t.Fatalf("positions=%v", positions)
	}
}

func TestCollectionAppendConcurrentWithReorderPreservesInvariant(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "append-reorder.db")
	repo, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	collection, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{Name: "追加重排"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"shot-ar-a", "shot-ar-b", "shot-ar-c"} {
		seedShotBasketFixture(t, repo, "asset-"+id, id, 0, 1000)
		if err := repo.AddShotToCollection(ctx, collection.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	seedShotBasketFixture(t, repo, "asset-ar-d", "shot-ar-d", 0, 1000)
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); <-start; errs <- repo.AddShotToCollection(ctx, collection.ID, "shot-ar-d") }()
	go func() {
		defer wg.Done()
		<-start
		errs <- repo.ReorderCollectionShots(ctx, collection.ID, []string{"shot-ar-c", "shot-ar-b", "shot-ar-a"})
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, domain.ErrReorderInvalid) {
			t.Fatal(err)
		}
	}
	var count, distinct int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(DISTINCT position) FROM collection_shots WHERE collection_id=?`, collection.ID).Scan(&count, &distinct); err != nil {
		t.Fatal(err)
	}
	if count != distinct {
		t.Fatalf("count=%d distinct=%d, positions are not unique", count, distinct)
	}
}

func TestCollectionPositionsMigrationNormalizesDuplicates(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "migration-positions.db")
	repo, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if _, err := repo.db.ExecContext(ctx, `CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.Name() == "0032_collection_shot_positions.sql" {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, string(content)); err != nil {
			t.Fatalf("apply %s: %v", entry.Name(), err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?,?)`, entry.Name(), formatTime(time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_collections(id,name,description,filter_json,created_at,updated_at) VALUES('migration-coll','legacy','', '{}', ?, ?)`, formatTime(time.Now()), formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	seedShotBasketFixture(t, repo, "asset-m-a", "shot-z", 0, 1000)
	seedShotBasketFixture(t, repo, "asset-m-b", "shot-a", 0, 1000)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO collection_shots(collection_id,shot_id,position,created_at) VALUES('migration-coll','shot-z',4,?),( 'migration-coll','shot-a',4,?)`, formatTime(time.Now()), formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var first, second int
	if err := repo.db.QueryRowContext(ctx, `SELECT position FROM collection_shots WHERE shot_id='shot-a'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT position FROM collection_shots WHERE shot_id='shot-z'`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != 0 || second != 1 {
		t.Fatalf("normalized positions shot-a=%d shot-z=%d", first, second)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO collection_shots(collection_id,shot_id,position,created_at) VALUES('migration-coll','shot-z',0,?)`, formatTime(time.Now())); err == nil {
		t.Fatal("unique collection position was not enforced")
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("rerunning migration: %v", err)
	}
	var indexCount int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, "0032_collection_shot_positions.sql").Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 1 {
		t.Fatalf("migration was not recorded exactly once")
	}
}
