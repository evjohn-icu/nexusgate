package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Pre-migration snapshots live next to the live database and are named
//
//	<dbfile>.pre-<version>-<yyyymmdd>
//
// e.g. /data/nexusgate.db.pre-0027-20260809, where <version> is the leading
// digits of the first pending migration file (the target of the upgrade).
// Doctor/support tooling finds them by globbing "<dbfile>.pre-*-*" beside the
// live database; the dated suffix means a failed upgrade can be rolled back
// even after a later, second attempt took its own snapshot on another day.
const snapshotTimeFormat = "20060102"

// preMigrationSnapshotGuard is the single entry point Migrate calls before
// applying the first pending migration. The L1a integrity/free-space gate
// (`checkPreMigrationConditions`, called at the top of Migrate) runs first;
// this guard only takes the snapshot. Both no-op when the schema is up to
// date, so a steady-state Migrate call stays read-only apart from the FTS
// rebuild.
func (r *Repository) preMigrationSnapshotGuard(ctx context.Context) error {
	first, err := r.firstPendingMigration(ctx)
	if err != nil {
		return err
	}
	if first == "" {
		return nil
	}
	if _, err := r.preMigrationSnapshot(ctx); err != nil {
		return err
	}
	return nil
}

// firstPendingMigration mirrors Migrate's loop: it returns the name (e.g.
// "0027_root_health.sql") of the first migration file not yet recorded in
// schema_migrations, or "" when every migration has been applied.
func (r *Repository) firstPendingMigration(ctx context.Context) (string, error) {
	return r.firstPendingMigrationWith(ctx, r.db)
}

func (r *Repository) firstPendingMigrationWith(ctx context.Context, q migrationQuerier) (string, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	applied, err := appliedMigrationVersions(ctx, q)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if _, exists := applied[entry.Name()]; !exists {
			return entry.Name(), nil
		}
	}
	return "", nil
}

// migrationVersionToken extracts the leading digits of a migration filename —
// the version the snapshot protects — e.g. "0027_root_health.sql" → "0027".
// Files without a numeric prefix fall back to their stem.
func migrationVersionToken(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			if i == 0 {
				return strings.TrimSuffix(name, ".sql")
			}
			return name[:i]
		}
	}
	return name
}

// preMigrationSnapshot copies the database to <dbfile>.pre-<version>-<date>
// before the first pending migration runs. modernc.org/sqlite does not expose
// SQLite's Online Backup API (the driver's lib layer only carries internal
// C-shims), so the snapshot is a crash-consistent file copy: a FULL WAL
// checkpoint first folds every committed frame into the main database file,
// then a write lock (BEGIN IMMEDIATE) is held for the duration of the copy so
// no later writer can append WAL frames — or trigger a passive autocheckpoint
// that rewrites the main file — under our reader. The same path already
// existing (same version, same date) means the snapshot was already taken:
// skip it, the guarantee is the file's presence, not a fresh one per run.
//
// A failure here ABORTS the migration: a machine that cannot write a snapshot
// cannot be trusted to upgrade. The error names the snapshot path so the
// operator can restore it if the database is already damaged.
func (r *Repository) preMigrationSnapshot(ctx context.Context) (string, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return r.preMigrationSnapshotWith(ctx, conn)
}

func (r *Repository) preMigrationSnapshotWith(ctx context.Context, conn *sql.Conn) (string, error) {
	first, err := r.firstPendingMigrationWith(ctx, conn)
	if err != nil {
		return "", err
	}
	if first == "" {
		return "", nil
	}
	dbPath, err := r.mainDatabasePathWith(ctx, conn)
	if err != nil {
		return "", err
	}
	snapshotPath := fmt.Sprintf("%s.pre-%s-%s", dbPath, migrationVersionToken(first), time.Now().UTC().Format(snapshotTimeFormat))

	if _, err := os.Stat(snapshotPath); err == nil {
		if err := validateSnapshot(snapshotPath); err == nil {
			return snapshotPath, nil
		}
		if err := os.Remove(snapshotPath); err != nil {
			return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): remove invalid snapshot: %w", snapshotPath, err)
		}
	}

	tempPath := filepath.Join(filepath.Dir(snapshotPath), fmt.Sprintf(".%s.tmp-%d", filepath.Base(snapshotPath), time.Now().UnixNano()))
	defer os.Remove(tempPath)
	if _, err := conn.ExecContext(ctx, `VACUUM INTO ?`, tempPath); err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	if err := validateSnapshot(tempPath); err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): validate temporary snapshot: %w", snapshotPath, err)
	}
	file, err := os.OpenFile(tempPath, os.O_WRONLY, 0)
	if err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	if err := os.Rename(tempPath, snapshotPath); err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	// fsync the directory so the snapshot's directory entry is durable too.
	dir, err := os.Open(filepath.Dir(snapshotPath))
	if err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): open snapshot directory: %w", snapshotPath, err)
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): sync snapshot directory: %w", snapshotPath, err)
	}
	if err := dir.Close(); err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): close snapshot directory: %w", snapshotPath, err)
	}
	return snapshotPath, nil
}

func validateSnapshot(path string) error {
	repo, err := Open(path)
	if err != nil {
		return err
	}
	defer repo.Close()
	return repo.IntegrityCheck(context.Background())
}

// mainDatabasePath resolves the on-disk path of the main database from the
// connection pool, so the snapshot lands beside the real file without the
// Repository having to carry the DSN it was opened with.
func (r *Repository) mainDatabasePath(ctx context.Context) (string, error) {
	return r.mainDatabasePathWith(ctx, r.db)
}

func (r *Repository) mainDatabasePathWith(ctx context.Context, q migrationQuerier) (string, error) {
	var seq int
	var name, file string
	if err := q.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&seq, &name, &file); err != nil {
		return "", err
	}
	if name != "main" {
		return "", fmt.Errorf("pre-migration snapshot: PRAGMA database_list returned %q, want main", name)
	}
	return file, nil
}
