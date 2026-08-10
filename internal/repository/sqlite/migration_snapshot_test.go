package sqlite

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// preSnapshotFiles lists the pre-migration snapshot files that exist beside a
// database named base in dir, in directory order.
func preSnapshotFiles(t *testing.T, dir, base string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), base+".pre-") {
			continue
		}
		out = append(out, entry.Name())
	}
	return out
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationSnapshotCreatedOnPendingMigrations(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "timingdex.db")
	repo, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	files := preSnapshotFiles(t, dir, "timingdex.db")
	if len(files) != 1 {
		t.Fatalf("Migrate on a fresh DB created %d snapshots, want 1: %v", len(files), files)
	}
	pattern := regexp.MustCompile(`^timingdex\.db\.pre-\d+-\d{8}$`)
	if !pattern.MatchString(files[0]) {
		t.Fatalf("snapshot name %q does not match %s", files[0], pattern)
	}
	first, err := repo.firstPendingMigration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "" {
		t.Fatalf("expected all migrations applied after Migrate, first pending = %q", first)
	}
	var firstToken string
	all, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range all {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			firstToken = migrationVersionToken(entry.Name())
			break
		}
	}
	if !strings.HasPrefix(files[0], "timingdex.db.pre-"+firstToken+"-") {
		t.Fatalf("snapshot name %q does not carry the first migration's version token %q", files[0], firstToken)
	}

	// The snapshot must be a valid SQLite database ...
	backup, err := Open(filepath.Join(dir, files[0]))
	if err != nil {
		t.Fatalf("snapshot is not a valid SQLite database: %v", err)
	}
	defer backup.Close()
	if err := backup.IntegrityCheck(context.Background()); err != nil {
		t.Fatalf("snapshot integrity_check: %v", err)
	}
	// ... and must predate the migration: schema_migrations is empty inside it.
	var applied int
	if err := backup.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatalf("snapshot captured a post-migration schema: %d migrations already recorded", applied)
	}
}

func TestMigrationSnapshotSkippedWhenItAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	repo, err := Open(filepath.Join(dir, "timingdex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := preSnapshotFiles(t, dir, "timingdex.db")
	if len(before) != 1 {
		t.Fatalf("first Migrate created %d snapshots, want 1: %v", len(before), before)
	}

	// A second Migrate on the same DB, same day: the dated snapshot path
	// already exists, so it must be skipped rather than rewritten.
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := preSnapshotFiles(t, dir, "timingdex.db")
	if len(after) != len(before) {
		t.Fatalf("second Migrate added snapshots: before %v, after %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("snapshot set changed across Migrate calls: before %v, after %v", before, after)
		}
	}
}

func TestMigrationSnapshotNotCreatedWhenUpToDate(t *testing.T) {
	dir := t.TempDir()
	repo, err := Open(filepath.Join(dir, "timingdex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Delete the first-run snapshot so a new one would be visibly created.
	files := preSnapshotFiles(t, dir, "timingdex.db")
	for _, f := range files {
		if err := os.Remove(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if files := preSnapshotFiles(t, dir, "timingdex.db"); len(files) != 0 {
		t.Fatalf("Migrate on an up-to-date DB created %d snapshots: %v", len(files), files)
	}
}

func TestMigrationSnapshotRestoresCorruptedDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "timingdex.db")
	repo, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	files := preSnapshotFiles(t, dir, "timingdex.db")
	if len(files) != 1 {
		t.Fatalf("Migrate created %d snapshots, want 1: %v", len(files), files)
	}
	snapshotPath := filepath.Join(dir, files[0])

	// Corrupt the migrated database: overwrite a page mid-file with garbage,
	// dropping WAL leftovers first so the corruption is deterministically in
	// the main file rather than reconciled by recovery.
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(dbPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	garbage := make([]byte, 4096)
	for i := range garbage {
		garbage[i] = 0xAA
	}
	if _, err := f.WriteAt(garbage, info.Size()/2); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// The corruption must be detectable, or the restore test proves nothing.
	corrupted, err := Open(dbPath)
	if err == nil {
		err = corrupted.IntegrityCheck(context.Background())
		corrupted.Close()
	}
	if err == nil {
		t.Fatal("expected integrity_check to fail on the corrupted database, got ok")
	}

	// Restore: copy the pre-migration snapshot over the damaged file and drop
	// any stale WAL sidecars, then reopen and migrate to the head again.
	copyFile(t, snapshotPath, dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	restored, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening the restored database: %v", err)
	}
	defer restored.Close()
	if err := restored.IntegrityCheck(context.Background()); err != nil {
		t.Fatalf("restored database integrity_check: %v", err)
	}
	if err := restored.Migrate(context.Background()); err != nil {
		t.Fatalf("restored database cannot migrate back to head: %v", err)
	}
	var applied int
	if err := restored.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied == 0 {
		t.Fatal("restored database has no schema_migrations rows after re-migration")
	}
}
