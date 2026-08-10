package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMigration0030NormalizesDuplicatePrimariesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version='0030_asset_location_primary.sql'; DROP INDEX uq_asset_locations_one_primary; DROP INDEX idx_asset_locations_asset_exists_seen; DROP INDEX idx_asset_locations_root_exists_asset_seen`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at,health_state) VALUES('mig-root-a','/a',?,?, 'healthy'),('mig-root-b','/b',?,?, 'unknown')`, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('mig-asset','fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('mig-loc-a','mig-asset','mig-root-a','a.mp4','/a/a.mp4',1,0,1,?),('mig-loc-b','mig-asset','mig-root-b','b.mp4','/b/b.mp4',1,1,1,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var primaries int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_locations WHERE asset_id='mig-asset' AND is_primary=1`).Scan(&primaries); err != nil {
		t.Fatal(err)
	}
	if primaries != 1 {
		t.Fatalf("primaries=%d, want 1", primaries)
	}
	var primaryID string
	if err := repo.db.QueryRowContext(ctx, `SELECT id FROM asset_locations WHERE asset_id='mig-asset' AND is_primary=1`).Scan(&primaryID); err != nil {
		t.Fatal(err)
	}
	if primaryID != "mig-loc-b" {
		t.Fatalf("primary=%q, want live unknown location", primaryID)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE asset_locations SET is_primary=1 WHERE id='mig-loc-a'`); err == nil {
		t.Fatal("duplicate primary update succeeded")
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertScannedFileAlternatePromotionDoesNotChangeAsset(t *testing.T) {
	ctx := context.Background()
	repo, rootA, dirA := newScanRepo(t)
	dirB := filepath.Join(t.TempDir(), "b")
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	rootB, err := repo.CreateLibraryRoot(ctx, dirB)
	if err != nil {
		t.Fatal(err)
	}
	pathA := filepath.Join(dirA, "clip.mp4")
	pathB := filepath.Join(dirB, "clip.mp4")
	scanWriteVideo(t, pathA)
	scanWriteVideo(t, pathB)
	infoA, _ := os.Stat(pathA)
	infoB, _ := os.Stat(pathB)
	first, err := repo.UpsertScannedFile(ctx, rootA, "clip.mp4", pathA, infoA, "same-fp")
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.UpsertScannedFile(ctx, rootB, "clip.mp4", pathB, infoB, "same-fp")
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Fatal("alternate promotion reported Changed")
	}
	var primaries int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_locations WHERE asset_id=? AND is_primary=1`, first.AssetID).Scan(&primaries); err != nil {
		t.Fatal(err)
	}
	if primaries != 1 {
		t.Fatalf("primaries=%d, want 1", primaries)
	}
	if second.AssetID != first.AssetID {
		t.Fatalf("asset switched from %q to %q", first.AssetID, second.AssetID)
	}
}

func TestMarkUnseenLocationsMissingPromotesLiveAlternate(t *testing.T) {
	ctx := context.Background()
	repo, rootA, dirA := newScanRepo(t)
	dirB := filepath.Join(t.TempDir(), "b")
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	rootB, err := repo.CreateLibraryRoot(ctx, dirB)
	if err != nil {
		t.Fatal(err)
	}
	pathA, pathB := filepath.Join(dirA, "clip.mp4"), filepath.Join(dirB, "clip.mp4")
	scanWriteVideo(t, pathA)
	scanWriteVideo(t, pathB)
	infoA, _ := os.Stat(pathA)
	infoB, _ := os.Stat(pathB)
	first, err := repo.UpsertScannedFile(ctx, rootA, "clip.mp4", pathA, infoA, "same-fp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertScannedFile(ctx, rootB, "clip.mp4", pathB, infoB, "same-fp"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkUnseenLocationsMissing(ctx, rootA.ID, nil); err != nil {
		t.Fatal(err)
	}
	loc, err := repo.GetPrimaryLocation(ctx, first.AssetID)
	if err != nil {
		t.Fatal(err)
	}
	if loc.RootID != rootB.ID {
		t.Fatalf("primary root=%q, want %q", loc.RootID, rootB.ID)
	}
}

func TestUpsertScannedFileReappearanceIsChanged(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)
	path := filepath.Join(rootDir, "reappear.mp4")
	scanWriteVideo(t, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.UpsertScannedFile(ctx, root, "reappear.mp4", path, info, "reappear-fp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkUnseenLocationsMissing(ctx, root.ID, nil); err != nil {
		t.Fatal(err)
	}
	reappeared, err := repo.UpsertScannedFile(ctx, root, "reappear.mp4", path, info, "reappear-fp")
	if err != nil {
		t.Fatal(err)
	}
	if reappeared.AssetID != first.AssetID || !reappeared.Changed {
		t.Fatalf("reappearance asset=%q changed=%v, want asset %q and Changed=true", reappeared.AssetID, reappeared.Changed, first.AssetID)
	}
}

func TestGetPrimaryLocationDeterministic(t *testing.T) {
	ctx := context.Background()
	repo, root, _ := newScanRepo(t)
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('det-asset','det-fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('det-b','det-asset',?,'b.mp4','/b',1,1,1,?),('det-a','det-asset',?,'a.mp4','/a',1,1,0,?)`, root.ID, now, root.ID, now); err != nil {
		t.Fatal(err)
	}
	loc, err := repo.GetPrimaryLocation(ctx, "det-asset")
	if err != nil {
		t.Fatal(err)
	}
	if loc.RelativePath != "b.mp4" {
		t.Fatalf("path=%q, want primary b.mp4", loc.RelativePath)
	}
}

func TestMigration0030UniqueIndexExists(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := repo.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND name='uq_asset_locations_one_primary'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name == "" {
		t.Fatal("primary unique index missing")
	}
}
