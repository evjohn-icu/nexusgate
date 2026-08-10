package sqlite

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// e.g. /data/timingdex.db.pre-0027-20260809, where <version> is the leading
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
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		var exists int
		if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, entry.Name()).Scan(&exists); err != nil {
			return "", err
		}
		if exists == 0 {
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
	first, err := r.firstPendingMigration(ctx)
	if err != nil {
		return "", err
	}
	if first == "" {
		return "", nil
	}
	dbPath, err := r.mainDatabasePath(ctx)
	if err != nil {
		return "", err
	}
	snapshotPath := fmt.Sprintf("%s.pre-%s-%s", dbPath, migrationVersionToken(first), time.Now().UTC().Format(snapshotTimeFormat))

	// An existing file is trusted only when it is plausibly complete: a
	// SIGKILL during a previous copy can leave a partial snapshot at the
	// canonical path, and the next run must not dedup it as valid. The size
	// check is the cheap heuristic; a fresh DB's snapshot matches the main
	// file's size because the checkpoint ran before the copy.
	if info, err := os.Stat(snapshotPath); err == nil {
		if dbInfo, dbErr := os.Stat(dbPath); dbErr == nil && info.Size() == dbInfo.Size() {
			return snapshotPath, nil
		}
		// Mismatch: remove the stale partial so this run takes a fresh one.
		_ = os.Remove(snapshotPath)
	}

	conn, err := r.db.Conn(ctx)
	if err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	defer conn.Close()

	// wal_checkpoint refuses to run inside a transaction ("database table is
	// locked"), so the checkpoint comes first; the write lock taken afterwards
	// is what keeps the copy atomic with respect to the checkpointed state.
	var busy, log, checkpointed int
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(FULL)`).Scan(&busy, &log, &checkpointed); err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	if busy != 0 {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): wal_checkpoint(FULL) could not acquire the write lock (busy=%d)", snapshotPath, busy)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()

	src, err := os.Open(dbPath)
	if err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	defer src.Close()
	// O_EXCL as a second dedup check: a snapshot appearing between the Stat
	// above and this open is one that already succeeded — use it.
	dst, err := os.OpenFile(snapshotPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return snapshotPath, nil
	}
	if err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	copied := false
	defer func() {
		dst.Close()
		// Never leave a partial snapshot behind: a truncated file at the
		// canonical path would be deduped as a valid one on the next attempt.
		if !copied {
			_ = os.Remove(snapshotPath)
		}
	}()
	if _, err := io.Copy(dst, src); err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	if err := dst.Sync(); err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	copied = true
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return "", fmt.Errorf("pre-migration snapshot failed (restore from %s if the database is damaged): %w", snapshotPath, err)
	}
	// fsync the directory so the snapshot's directory entry is durable too.
	if dir, err := os.Open(filepath.Dir(snapshotPath)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return snapshotPath, nil
}

// mainDatabasePath resolves the on-disk path of the main database from the
// connection pool, so the snapshot lands beside the real file without the
// Repository having to carry the DSN it was opened with.
func (r *Repository) mainDatabasePath(ctx context.Context) (string, error) {
	var seq int
	var name, file string
	if err := r.db.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&seq, &name, &file); err != nil {
		return "", err
	}
	if name != "main" {
		return "", fmt.Errorf("pre-migration snapshot: PRAGMA database_list returned %q, want main", name)
	}
	return file, nil
}
