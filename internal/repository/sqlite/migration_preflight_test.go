package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// removeNewestMigration deletes the newest applied version row, making the
// embedded migration set appear pending again without touching migration SQL.
func removeNewestMigration(t *testing.T, repo *Repository) {
	t.Helper()
	res, err := repo.db.Exec(`DELETE FROM schema_migrations WHERE version = (SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1)`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("removed %d schema_migrations rows, want 1", n)
	}
}

func TestPreflightNoOpWhenFullyMigrated(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "ok.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	probed := false
	old := statfsFreeBytes
	statfsFreeBytes = func(string) (uint64, error) {
		probed = true
		return 0, errors.New("probe must not run on a fully migrated database")
	}
	t.Cleanup(func() { statfsFreeBytes = old })

	if err := repo.checkPreMigrationConditions(context.Background()); err != nil {
		t.Fatalf("preflight on a fully migrated database = %v, want nil", err)
	}
	if probed {
		t.Fatal("free-space probe ran although nothing is pending; preflight must be a no-op")
	}
}

func TestPreflightRefusesCorruptDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	repo, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	repo.Close()

	repo, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	removeNewestMigration(t, repo)

	// Flip random bytes over a middle page: the header and the schema b-tree
	// stay readable (so the pending check and integrity_check both run) while
	// integrity_check must report the damage.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	pageSize := int64(4096)
	offset := (fi.Size() / pageSize / 2) * pageSize
	garbage := make([]byte, pageSize)
	for i := range garbage {
		garbage[i] = byte(0xA5 + i)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(garbage, offset); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	err = repo.checkPreMigrationConditions(context.Background())
	if err == nil {
		t.Fatal("preflight accepted a corrupt database, want refusal")
	}
	if !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("preflight error %q does not mention integrity", err)
	}
}

func TestMigrationFreeSpaceRequired(t *testing.T) {
	headroom := int64(256 << 20)
	if got := migrationFreeSpaceRequired(0); got != headroom {
		t.Fatalf("migrationFreeSpaceRequired(0) = %d, want %d", got, headroom)
	}
	dbSize := int64(1 << 30)
	if got := migrationFreeSpaceRequired(dbSize); got != dbSize*2+headroom {
		t.Fatalf("migrationFreeSpaceRequired(%d) = %d, want %d", dbSize, got, dbSize*2+headroom)
	}
}

func TestPreflightFreeSpaceError(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "full.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	removeNewestMigration(t, repo)

	old := statfsFreeBytes
	statfsFreeBytes = func(string) (uint64, error) { return 1, nil }
	t.Cleanup(func() { statfsFreeBytes = old })

	err = repo.checkPreMigrationConditions(context.Background())
	if err == nil {
		t.Fatal("preflight accepted a 1-byte filesystem, want refusal")
	}
	for _, want := range []string{"free disk space", repo.dbPath, "NEXUSGATE_DATA_DIR"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("preflight error %q does not mention %q", err, want)
		}
	}
}

func TestPreflightCountsKnownMissingMigrationDespiteUnknownSchemaRow(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "future.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	removeNewestMigration(t, repo)
	if _, err := repo.db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES('9999_future_migration.sql', ?)`, formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}

	old := statfsFreeBytes
	statfsFreeBytes = func(string) (uint64, error) { return 1, nil }
	t.Cleanup(func() { statfsFreeBytes = old })
	if err := repo.checkPreMigrationConditions(context.Background()); err == nil || !strings.Contains(err.Error(), "free disk space") {
		t.Fatalf("preflight with a missing known migration and unknown row = %v, want free-space error", err)
	}
}

func TestPendingMigrationCountIgnoresUnknownRows(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "future-only.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES('9999_future_migration.sql', ?)`, formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.pendingMigrationCount(context.Background()); err != nil {
		t.Fatal(err)
	} else if got != 0 {
		t.Fatalf("pendingMigrationCount with only an unknown row = %d, want 0", got)
	}
}

func TestPreflightPassesWhenDiskHasRoom(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "roomy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	removeNewestMigration(t, repo)

	if err := repo.checkPreMigrationConditions(context.Background()); err != nil {
		t.Fatalf("preflight with plenty of real free space = %v, want nil", err)
	}
}
