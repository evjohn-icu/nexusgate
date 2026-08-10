package sqlite

import (
	"container/heap"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evjohn-icu/timingdex/internal/capture"
	"github.com/evjohn-icu/timingdex/internal/discovery"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/idgen"
	"github.com/evjohn-icu/timingdex/internal/remote"
	"github.com/evjohn-icu/timingdex/internal/textindex"
	_ "modernc.org/sqlite"
)

// CreateWorkerPairing returns the raw token exactly once. Only its SHA-256
// digest is persisted, so a database backup never becomes a worker credential.
func (r *Repository) CreateWorkerPairing(ctx context.Context, ttl time.Duration) (remote.PairingToken, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	raw, err := randomToken()
	if err != nil {
		return remote.PairingToken{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(ttl)
	_, err = r.db.ExecContext(ctx, `INSERT INTO worker_pairing_tokens(id,token_hash,expires_at,created_at) VALUES(?,?,?,?)`, idgen.New(), tokenDigest(raw), formatTime(expires), formatTime(now))
	if err != nil {
		return remote.PairingToken{}, err
	}
	return remote.PairingToken{Token: raw, ExpiresAt: expires}, nil
}

func (r *Repository) EnrollWorker(ctx context.Context, pairingToken string, registration remote.WorkerRegistration) (remote.Worker, string, error) {
	if strings.TrimSpace(registration.Name) == "" || strings.TrimSpace(registration.Platform) == "" {
		return remote.Worker{}, "", errors.New("worker name and platform are required")
	}
	now := time.Now().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return remote.Worker{}, "", err
	}
	defer tx.Rollback()
	var pairingID, expires string
	err = tx.QueryRowContext(ctx, `SELECT id,expires_at FROM worker_pairing_tokens WHERE token_hash=? AND redeemed_at IS NULL`, tokenDigest(pairingToken)).Scan(&pairingID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return remote.Worker{}, "", errors.New("pairing token is invalid or already redeemed")
	}
	if err != nil {
		return remote.Worker{}, "", err
	}
	expiresAt, _ := time.Parse(time.RFC3339Nano, expires)
	if !expiresAt.After(now) {
		return remote.Worker{}, "", errors.New("pairing token has expired")
	}
	token, err := randomToken()
	if err != nil {
		return remote.Worker{}, "", err
	}
	capabilities, err := json.Marshal(registration.Capabilities)
	if err != nil {
		return remote.Worker{}, "", err
	}
	worker := remote.Worker{ID: idgen.New(), Name: registration.Name, Platform: registration.Platform, Version: registration.Version, Status: remote.WorkerOnline, Capabilities: registration.Capabilities, LastSeenAt: now, CreatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO workers(id,name,platform,version,token_hash,status,capabilities_json,last_seen_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, worker.ID, worker.Name, worker.Platform, worker.Version, tokenDigest(token), worker.Status, string(capabilities), formatTime(now), formatTime(now))
	if err != nil {
		return remote.Worker{}, "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE worker_pairing_tokens SET redeemed_at=?,worker_id=? WHERE id=? AND redeemed_at IS NULL`, formatTime(now), worker.ID, pairingID)
	if err != nil {
		return remote.Worker{}, "", err
	}
	if err = tx.Commit(); err != nil {
		return remote.Worker{}, "", err
	}
	return worker, token, nil
}

func (r *Repository) AuthenticateWorker(ctx context.Context, token string) (remote.Worker, error) {
	var worker remote.Worker
	var rawCapabilities, lastSeen, created string
	err := r.db.QueryRowContext(ctx, `SELECT id,name,platform,version,status,capabilities_json,last_seen_at,created_at FROM workers WHERE token_hash=? AND status!='revoked'`, tokenDigest(token)).Scan(&worker.ID, &worker.Name, &worker.Platform, &worker.Version, &worker.Status, &rawCapabilities, &lastSeen, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return remote.Worker{}, errors.New("worker token is invalid or revoked")
	}
	if err != nil {
		return remote.Worker{}, err
	}
	if err := json.Unmarshal([]byte(rawCapabilities), &worker.Capabilities); err != nil {
		return remote.Worker{}, err
	}
	worker.LastSeenAt, _ = time.Parse(time.RFC3339Nano, lastSeen)
	worker.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return worker, nil
}

func (r *Repository) HeartbeatWorker(ctx context.Context, workerID, version string, capabilities remote.WorkerCapabilities) error {
	raw, err := json.Marshal(capabilities)
	if err != nil {
		return err
	}
	// Version is set only when the Worker sent one: a pre-version Hub-adjacent
	// Worker that never reports a version must not erase what an earlier
	// heartbeat (or enrollment) recorded.
	query := `UPDATE workers SET status='online',capabilities_json=?,last_seen_at=? WHERE id=? AND status!='revoked'`
	args := []any{string(raw), formatTime(time.Now().UTC()), workerID}
	if version != "" {
		query = `UPDATE workers SET status='online',capabilities_json=?,version=?,last_seen_at=? WHERE id=? AND status!='revoked'`
		args = []any{string(raw), version, formatTime(time.Now().UTC()), workerID}
	}
	_, err = r.db.ExecContext(ctx, query, args...)
	return err
}

// workerOfflineAfter bounds how stale a worker's last heartbeat may be before
// ListWorkers derives its status as offline. It is 3x the default 30s
// HeartbeatInterval so a single dropped heartbeat does not flap the fleet view.
const workerOfflineAfter = 90 * time.Second

// IntegrityCheck runs SQLite's own self-check (PRAGMA integrity_check). It
// returns nil when the database is consistent and an error naming the first
// problem otherwise. Exposed for the `timingdex doctor` command so an operator
// can confirm a library survived a NAS power event without guessing.
func (r *Repository) IntegrityCheck(ctx context.Context) error {
	var result string
	if err := r.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite integrity_check reported: %s", result)
	}
	return nil
}

func (r *Repository) ListWorkers(ctx context.Context) ([]remote.Worker, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,name,platform,version,status,capabilities_json,last_seen_at,created_at FROM workers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []remote.Worker
	for rows.Next() {
		var w remote.Worker
		var raw, last, created string
		if err := rows.Scan(&w.ID, &w.Name, &w.Platform, &w.Version, &w.Status, &raw, &last, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &w.Capabilities); err != nil {
			return nil, err
		}
		w.LastSeenAt, _ = time.Parse(time.RFC3339Nano, last)
		w.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		// A worker that has not heartbeated within the threshold is derived as
		// offline even though its stored status is still 'online' (heartbeats
		// write online and never fall back). This is the only place the derived
		// view is computed; the stored status stays untouched so a revoked row is
		// never flipped back and heartbeats keep writing 'online' truthfully.
		if w.Status == remote.WorkerOnline && time.Since(w.LastSeenAt) > workerOfflineAfter {
			w.Status = remote.WorkerOffline
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func tokenDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Repository struct {
	// dbPath is the on-disk database file path, kept so the migration preflight
	// can stat the file and probe the filesystem that holds it.
	dbPath              string
	db                  *sql.DB
	semanticVectorMu    sync.RWMutex
	semanticVectorCache map[string][]float64
}

// migrationPauseAfterLock is intentionally narrow so concurrency tests can
// hold the SQLite migration lock while another repository waits for it.
var migrationPauseAfterLock func()

func Open(path string) (*Repository, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &Repository{dbPath: path, db: db, semanticVectorCache: make(map[string][]float64)}, nil
}

func (r *Repository) Close() error { return r.db.Close() }

func (r *Repository) Migrate(ctx context.Context) error {
	lockConn, err := r.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer lockConn.Close()
	if _, err := lockConn.ExecContext(ctx, `PRAGMA busy_timeout=30000`); err != nil {
		return err
	}
	if _, err := lockConn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	// Preflight and VACUUM INTO run outside a transaction because VACUUM cannot
	// run inside one. The snapshot is a consistent SQLite image of this conn.
	if err := r.checkPreMigrationConditionsWith(ctx, lockConn); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if _, err := r.preMigrationSnapshotWith(ctx, lockConn); err != nil {
		return err
	}
	if _, err := lockConn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = lockConn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if migrationPauseAfterLock != nil {
		migrationPauseAfterLock()
	}
	applied, err := appliedMigrationVersions(ctx, lockConn)
	if err != nil {
		return err
	}
	// SQLite ignores PRAGMA foreign_keys changes inside a transaction.  The
	// model_runs rebuild drops a table referenced by canonical rows, so disable
	// enforcement on the dedicated migration connection before starting the
	// batch. The check after re-enabling enforcement catches damaged
	// relationships before startup continues.
	if _, err := lockConn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	foreignKeysRestored := false
	defer func() {
		if !foreignKeysRestored {
			_, _ = lockConn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`)
		}
	}()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if _, exists := applied[entry.Name()]; exists {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		if _, err := lockConn.ExecContext(ctx, string(content)); err != nil {
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		if _, err := lockConn.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, entry.Name(), formatTime(time.Now())); err != nil {
			return err
		}
		applied[entry.Name()] = struct{}{}
	}
	if _, err := lockConn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	if _, err := lockConn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return err
	}
	foreignKeysRestored = true
	var violations int
	if err := lockConn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		return err
	}
	if violations != 0 {
		return fmt.Errorf("migration left %d foreign key violations", violations)
	}
	return r.ensureCJKBigramFTS(ctx)
}

// statfsFreeBytes probes the free space on the filesystem holding a path. It
// is a variable so tests can inject a value; the build-tagged statfs_*.go
// files install the platform implementation at init time. Windows installs a
// probe that always errors, which the preflight treats as "measurement
// unavailable" rather than as a blocker.
var statfsFreeBytes func(path string) (uint64, error)

// checkPreMigrationConditions refuses to start an upgrade that cannot be
// completed safely. It runs before any migration DDL: migrating a corrupt
// database would hide the corruption under fresh schema, so the operator must
// restore from backup first (never auto-repair), and an upgrade that rebuilds
// FTS tables can roughly double the database size, so a disk that cannot hold
// that must be dealt with before the upgrade, not discovered mid-migration
// with ENOSPC. A fully migrated database skips both checks entirely so
// everyday startup pays nothing.
func (r *Repository) checkPreMigrationConditions(ctx context.Context) error {
	return r.checkPreMigrationConditionsWith(ctx, r.db)
}

func (r *Repository) checkPreMigrationConditionsWith(ctx context.Context, q migrationQuerier) error {
	pending, err := r.pendingMigrationCountWith(ctx, q)
	if err != nil {
		return fmt.Errorf("migration preflight: cannot determine pending migrations: %w", err)
	}
	if pending == 0 {
		return nil
	}
	if err := integrityCheck(ctx, q); err != nil {
		return fmt.Errorf("migration preflight: refusing to migrate a corrupt database: %w (the operator must restore the library from backup first; this upgrade will not run until the database passes integrity_check)", err)
	}
	dbSize, err := os.Stat(r.dbPath)
	if err != nil {
		return fmt.Errorf("migration preflight: cannot stat database file %s: %w", r.dbPath, err)
	}
	free, err := statfsFreeBytes(r.dbPath)
	if err != nil {
		// statfs is unavailable (Windows) or the file vanished; a disk check we
		// cannot take must never block an upgrade, so skip rather than guess.
		return nil
	}
	required := migrationFreeSpaceRequired(dbSize.Size())
	if free < uint64(required) {
		return fmt.Errorf("migration preflight: not enough free disk space to upgrade %s: need %d bytes, have %d free (free space on that volume or set TIMINGDEX_DATA_DIR to a location with room)", r.dbPath, required, free)
	}
	return nil
}

// pendingMigrationCount is how many embedded migration files have not been
// applied yet. schema_migrations is created by Migrate, so a library that has
// never been migrated reports the full embedded count; a fully migrated one
// reports zero, and the preflight then returns without touching the disk.
func (r *Repository) pendingMigrationCount(ctx context.Context) (int, error) {
	return r.pendingMigrationCountWith(ctx, r.db)
}

func (r *Repository) pendingMigrationCountWith(ctx context.Context, q migrationQuerier) (int, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return 0, err
	}
	var exists int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		pending := 0
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
				pending++
			}
		}
		return pending, nil
	}
	applied, err := appliedMigrationVersions(ctx, q)
	if err != nil {
		return 0, err
	}
	pending := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			if _, ok := applied[entry.Name()]; !ok {
				pending++
			}
		}
	}
	return pending, nil
}

type migrationQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func integrityCheck(ctx context.Context, q migrationQuerier) error {
	var result string
	if err := q.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite integrity_check reported: %s", result)
	}
	return nil
}

func appliedMigrationVersions(ctx context.Context, q migrationQuerier) (map[string]struct{}, error) {
	rows, err := q.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied := make(map[string]struct{})
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		applied[version] = struct{}{}
	}
	return applied, rows.Err()
}

// migrationFreeSpaceRequired is the disk headroom demanded before an upgrade
// begins: 2x the current database size (an FTS rebuild can roughly double it)
// plus 256 MiB for WAL growth and temporary b-tree spill.
func migrationFreeSpaceRequired(dbSize int64) int64 {
	return dbSize*2 + 256<<20
}

// ensureCJKBigramFTS makes an interrupted upgrade recoverable: migration SQL
// marks the index pending, then this Hub-owned transaction rebuilds both FTS
// tables from the authoritative asset rows before marking it ready.
func (r *Repository) ensureCJKBigramFTS(ctx context.Context) error {
	var state string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM fts_index_state WHERE name='cjk_bigram_v1'`).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "ready" {
		return nil
	}
	return r.rebuildCJKBigramFTS(ctx)
}

func (r *Repository) rebuildCJKBigramFTS(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// asset_search_rowids/asset_shot_search_rowids must be wiped and rebuilt
	// alongside the FTS shadow tables: this rebuild reinserts every row, which
	// assigns each one a new FTS5 rowid, so any previously mapped rowid would
	// otherwise point at the wrong (or no longer existing) row.
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_search; DELETE FROM asset_shot_search; DELETE FROM asset_search_rowids; DELETE FROM asset_shot_search_rowids`); err != nil {
		return err
	}

	type assetRecord struct{ id, filename, summary, transcript, sceneTags, subjects, moods, extra, reason string }
	assets := make([]assetRecord, 0)
	assetRows, err := tx.QueryContext(ctx, `SELECT a.id,
COALESCE((SELECT absolute_path FROM asset_locations l WHERE l.asset_id=a.id AND l.exists_now=1 ORDER BY l.is_primary DESC,l.last_seen_at DESC LIMIT 1),''),
COALESCE(an.summary,''),COALESCE((SELECT full_text FROM transcripts t WHERE t.asset_id=a.id AND t.status='succeeded' ORDER BY t.created_at DESC LIMIT 1),''),
COALESCE(an.scene_tags_json,''),COALESCE(an.subjects_json,''),COALESCE(an.mood_tags_json,''),COALESCE(an.extra_tags_json,''),COALESCE(an.editorial_reason,'')
FROM assets a LEFT JOIN asset_analysis an ON an.asset_id=a.id`)
	if err != nil {
		return err
	}
	for assetRows.Next() {
		var record assetRecord
		if err := assetRows.Scan(&record.id, &record.filename, &record.summary, &record.transcript, &record.sceneTags, &record.subjects, &record.moods, &record.extra, &record.reason); err != nil {
			assetRows.Close()
			return err
		}
		assets = append(assets, record)
	}
	if err := assetRows.Err(); err != nil {
		assetRows.Close()
		return err
	}
	assetRows.Close()
	for _, record := range assets {
		res, err := tx.ExecContext(ctx, `INSERT INTO asset_search(asset_id,filename,summary,transcript,scene_tags,subjects,mood_tags,extra_tags,location,editorial_reason) VALUES(?,?,?,?,?,?,?,?,?,?)`, record.id, indexText(filepath.Base(record.filename)), indexText(record.summary), indexText(record.transcript), indexText(record.sceneTags), indexText(record.subjects), indexText(record.moods), indexText(record.extra), "", indexText(record.reason))
		if err != nil {
			return err
		}
		rowid, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO asset_search_rowids(asset_id,search_rowid) VALUES(?,?)`, record.id, rowid); err != nil {
			return err
		}
	}

	type shotRecord struct{ id, assetID, description, tags, objects, actions, mood string }
	shots := make([]shotRecord, 0)
	shotRows, err := tx.QueryContext(ctx, `SELECT id,asset_id,description,tags_json,objects_json,actions_json,mood_json FROM asset_shots`)
	if err != nil {
		return err
	}
	for shotRows.Next() {
		var record shotRecord
		if err := shotRows.Scan(&record.id, &record.assetID, &record.description, &record.tags, &record.objects, &record.actions, &record.mood); err != nil {
			shotRows.Close()
			return err
		}
		shots = append(shots, record)
	}
	if err := shotRows.Err(); err != nil {
		shotRows.Close()
		return err
	}
	shotRows.Close()
	for _, record := range shots {
		res, err := tx.ExecContext(ctx, `INSERT INTO asset_shot_search(shot_id,asset_id,description,tags,objects,actions,mood) VALUES(?,?,?,?,?,?,?)`, record.id, record.assetID, indexText(record.description), indexText(record.tags), indexText(record.objects), indexText(record.actions), indexText(record.mood))
		if err != nil {
			return err
		}
		rowid, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO asset_shot_search_rowids(shot_id,asset_id,search_rowid) VALUES(?,?,?)`, record.id, record.assetID, rowid); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fts_index_state SET value='ready',updated_at=? WHERE name='cjk_bigram_v1'`, formatTime(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) CreateLibraryRoot(ctx context.Context, path string) (domain.LibraryRoot, error) {
	now := time.Now().UTC()
	root := domain.LibraryRoot{ID: idgen.New(), Path: path, CreatedAt: now, UpdatedAt: now, HealthState: domain.RootHealthUnknown}
	_, err := r.db.ExecContext(ctx, `INSERT INTO library_roots(id, path, created_at, updated_at) VALUES (?, ?, ?, ?)`, root.ID, root.Path, formatTime(now), formatTime(now))
	if err != nil {
		return domain.LibraryRoot{}, err
	}
	return root, nil
}

func (r *Repository) ListLibraryRoots(ctx context.Context) ([]domain.LibraryRoot, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, path, created_at, updated_at, health_state, last_healthy_at, last_scan_at FROM library_roots ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roots := []domain.LibraryRoot{}
	for rows.Next() {
		var root domain.LibraryRoot
		var created, updated string
		var lastHealthy, lastScan sql.NullString
		if err := rows.Scan(&root.ID, &root.Path, &created, &updated, &root.HealthState, &lastHealthy, &lastScan); err != nil {
			return nil, err
		}
		root.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		root.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		root.LastHealthyAt = parseNullableTime(lastHealthy)
		root.LastScanAt = parseNullableTime(lastScan)
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

func (r *Repository) IsLibraryRoot(ctx context.Context, path string) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM library_roots WHERE path=?)`, path).Scan(&exists)
	return exists, err
}

func (r *Repository) GetLibraryRoot(ctx context.Context, id string) (domain.LibraryRoot, error) {
	var root domain.LibraryRoot
	var created, updated string
	var lastHealthy, lastScan sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT id, path, created_at, updated_at, health_state, last_healthy_at, last_scan_at FROM library_roots WHERE id = ?`, id).Scan(&root.ID, &root.Path, &created, &updated, &root.HealthState, &lastHealthy, &lastScan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.LibraryRoot{}, fmt.Errorf("library root not found: %s", id)
	}
	if err != nil {
		return domain.LibraryRoot{}, err
	}
	root.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	root.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	root.LastHealthyAt = parseNullableTime(lastHealthy)
	root.LastScanAt = parseNullableTime(lastScan)
	return root, nil
}

func (r *Repository) UpsertScannedFile(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ScannedFile{}, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	var assetID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM assets WHERE quick_fingerprint = ? AND file_size = ? LIMIT 1`, fingerprint, info.Size()).Scan(&assetID)
	result := domain.ScannedFile{}
	if errors.Is(err, sql.ErrNoRows) {
		assetID = idgen.New()
		_, err = tx.ExecContext(ctx, `INSERT INTO assets(id, quick_fingerprint, file_size, state, first_seen_at, last_seen_at) VALUES (?, ?, ?, 'discovered', ?, ?)`, assetID, fingerprint, info.Size(), formatTime(now), formatTime(now))
		result.Created = true
	}
	if err != nil {
		return result, err
	}
	result.AssetID = assetID

	// Widen the lookup beyond the id so the scan can tell whether this revisit
	// changed anything the pipeline keys its jobs on (mtime, and a new or
	// moved location) without a second round trip. Only the id is ever written
	// back; the rest is compared.
	var locationID string
	var existingAbsolutePath string
	var existingModifiedNS int64
	locationExists := true
	err = tx.QueryRowContext(ctx, `SELECT id, modified_ns, absolute_path FROM asset_locations WHERE root_id = ? AND relative_path = ?`, root.ID, relativePath).Scan(&locationID, &existingModifiedNS, &existingAbsolutePath)
	if errors.Is(err, sql.ErrNoRows) {
		locationExists = false
		locationID = idgen.New()
		_, err = tx.ExecContext(ctx, `INSERT INTO asset_locations(id, asset_id, root_id, relative_path, absolute_path, modified_ns, exists_now, is_primary, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, 1, 1, ?)`, locationID, assetID, root.ID, relativePath, absolutePath, info.ModTime().UnixNano(), formatTime(now))
	} else if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE asset_locations SET asset_id = ?, absolute_path = ?, modified_ns = ?, exists_now = 1, last_seen_at = ? WHERE id = ?`, assetID, absolutePath, info.ModTime().UnixNano(), formatTime(now), locationID)
	}
	if err != nil {
		return result, err
	}
	// A location that did not exist was inserted — a known asset appearing at
	// a new path, or a brand-new asset — so it counts as changed. An existing
	// one counts only when the mtime the probe job's input hash is derived
	// from moved, or the path changed (a move or remount: the asset is worth
	// re-enqueuing, and the probe hash dedup makes that re-enqueue a no-op
	// when content and mtime are unchanged).
	result.Changed = result.Created || !locationExists || existingModifiedNS != info.ModTime().UnixNano() || existingAbsolutePath != absolutePath

	_, err = tx.ExecContext(ctx, `UPDATE assets SET state = 'discovered', last_seen_at = ?, missing_since = NULL WHERE id = ?`, formatTime(now), assetID)
	if err != nil {
		return result, err
	}

	return result, tx.Commit()
}

func (r *Repository) MarkUnseenLocationsMissing(ctx context.Context, rootID string, seenRelativePaths []string) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS scan_seen(relative_path TEXT PRIMARY KEY)`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM scan_seen`); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO scan_seen(relative_path) VALUES (?)`)
	if err != nil {
		return 0, err
	}
	for _, path := range seenRelativePaths {
		if _, err := stmt.ExecContext(ctx, path); err != nil {
			stmt.Close()
			return 0, err
		}
	}
	stmt.Close()

	result, err := tx.ExecContext(ctx, `UPDATE asset_locations SET exists_now = 0 WHERE root_id = ? AND relative_path NOT IN (SELECT relative_path FROM scan_seen) AND exists_now = 1`, rootID)
	if err != nil {
		return 0, err
	}
	count, _ := result.RowsAffected()

	now := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `UPDATE assets SET state = 'missing', missing_since = COALESCE(missing_since, ?) WHERE id IN (SELECT a.id FROM assets a WHERE NOT EXISTS (SELECT 1 FROM asset_locations l WHERE l.asset_id = a.id AND l.exists_now = 1))`, now); err != nil {
		return 0, err
	}
	return int(count), tx.Commit()
}

// AssetsWithoutProbeJob returns ids of live assets in a root that have never had
// a probe job enqueued. The scan's changed set only covers assets whose keying
// data moved; an asset that never got a probe job at all would otherwise starve
// the pipeline forever, so the scan catches up on those too. The limit bounds
// the catch-up so one pathological root cannot turn a scan into an unbounded
// insert storm.
func (r *Repository) AssetsWithoutProbeJob(ctx context.Context, rootID string, limit int) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT l.asset_id FROM asset_locations l JOIN assets a ON a.id = l.asset_id WHERE l.root_id = ? AND l.exists_now = 1 AND a.state != 'missing' AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.asset_id = l.asset_id AND j.job_type = 'probe') LIMIT ?`, rootID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *Repository) ListAssets(ctx context.Context, limit, offset int) ([]domain.Asset, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, quick_fingerprint, full_hash, file_size, state, first_seen_at, last_seen_at, missing_since FROM assets ORDER BY last_seen_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets := []domain.Asset{}
	for rows.Next() {
		var asset domain.Asset
		var fullHash sql.NullString
		var firstSeen, lastSeen string
		var missing sql.NullString
		if err := rows.Scan(&asset.ID, &asset.QuickFingerprint, &fullHash, &asset.FileSize, &asset.State, &firstSeen, &lastSeen, &missing); err != nil {
			return nil, err
		}
		if fullHash.Valid {
			asset.FullHash = &fullHash.String
		}
		asset.FirstSeenAt, _ = time.Parse(time.RFC3339Nano, firstSeen)
		asset.LastSeenAt, _ = time.Parse(time.RFC3339Nano, lastSeen)
		// Parse missing_since with the same strategy as GetAssetDetail:
		// treat unparseable values as absent (nil) rather than zero time,
		// so the two APIs are semantically consistent even on dirty data.
		if missing.Valid {
			if t, err := time.Parse(time.RFC3339Nano, missing.String); err != nil {
				slog.Debug("failed to parse missing_since in ListAssets", "asset_id", asset.ID, "value", missing.String, "error", err)
			} else {
				asset.MissingSince = &t
			}
		}
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

// A fixed-width fraction, not RFC3339Nano's .999999999. Every timestamp in this
// database is compared as a string by SQL, and RFC3339Nano strips trailing
// zeros, so its output is variable-length and lexicographic order stops matching
// chronological order -- see migration 0021 for the rewrite of rows written
// before this.
const sortableTimeLayout = "2006-01-02T15:04:05.000000000Z"

func formatTime(value time.Time) string { return value.UTC().Format(sortableTimeLayout) }

func (r *Repository) GetPrimaryLocation(ctx context.Context, assetID string) (domain.AssetLocation, error) {
	var v domain.AssetLocation
	var existsNow, primary int
	var last string
	// The asset's quick_fingerprint and file_size ride along on the location
	// row: the pipeline derives the probe job's input hash from them (not from
	// the path), and asking a second time for data one join provides would be
	// a needless round trip on the hottest pipeline path.
	err := r.db.QueryRowContext(ctx, `SELECT l.id,l.asset_id,l.root_id,l.relative_path,l.absolute_path,COALESCE(l.file_id,''),l.modified_ns,l.exists_now,l.is_primary,l.last_seen_at,a.quick_fingerprint,a.file_size FROM asset_locations l JOIN assets a ON a.id=l.asset_id WHERE l.asset_id=? AND l.exists_now=1 ORDER BY l.is_primary DESC,l.last_seen_at DESC LIMIT 1`, assetID).Scan(&v.ID, &v.AssetID, &v.RootID, &v.RelativePath, &v.AbsolutePath, &v.FileID, &v.ModifiedNS, &existsNow, &primary, &last, &v.QuickFingerprint, &v.FileSize)
	if err != nil {
		return domain.AssetLocation{}, err
	}
	v.Exists = existsNow == 1
	v.IsPrimary = primary == 1
	v.LastSeenAt, _ = time.Parse(time.RFC3339Nano, last)
	return v, nil
}

func (r *Repository) UpsertProviderChannel(ctx context.Context, channel domain.ProviderChannel) (domain.ProviderChannel, error) {
	if strings.TrimSpace(channel.Capability) == "" || strings.TrimSpace(channel.Label) == "" || strings.TrimSpace(channel.ProviderName) == "" {
		return domain.ProviderChannel{}, fmt.Errorf("provider channel capability, label, and provider are required")
	}
	if channel.ID == "" {
		channel.ID = idgen.New()
	}
	now := time.Now().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ProviderChannel{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO provider_channels(id,capability,label,provider_name,protocol,endpoint,model,enabled,route_order,cost_per_request,cost_per_video_minute,cost_per_audio_minute,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET capability=excluded.capability,label=excluded.label,provider_name=excluded.provider_name,protocol=excluded.protocol,endpoint=excluded.endpoint,model=excluded.model,enabled=excluded.enabled,route_order=excluded.route_order,cost_per_request=excluded.cost_per_request,cost_per_video_minute=excluded.cost_per_video_minute,cost_per_audio_minute=excluded.cost_per_audio_minute,deleted_at=NULL,updated_at=excluded.updated_at`, channel.ID, channel.Capability, channel.Label, channel.ProviderName, channel.Protocol, channel.Endpoint, channel.Model, boolInt(channel.Enabled), channel.RouteOrder, channel.CostPerRequest, channel.CostPerVideoMinute, channel.CostPerAudioMinute, formatTime(now), formatTime(now)); err != nil {
		return domain.ProviderChannel{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM provider_channel_members WHERE channel_id=?`, channel.ID); err != nil {
		return domain.ProviderChannel{}, err
	}
	for i := range channel.Members {
		member := &channel.Members[i]
		if strings.TrimSpace(member.Label) == "" || strings.TrimSpace(member.SecretRef) == "" {
			return domain.ProviderChannel{}, fmt.Errorf("provider channel member label and secret reference are required")
		}
		if member.ID == "" {
			member.ID = idgen.New()
		}
		member.ChannelID = channel.ID
		if member.Weight <= 0 {
			member.Weight = 1
		}
		if member.MaxInflight <= 0 {
			member.MaxInflight = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO provider_channel_members(id,channel_id,label,secret_ref,enabled,weight,max_inflight,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, member.ID, member.ChannelID, member.Label, member.SecretRef, boolInt(member.Enabled), member.Weight, member.MaxInflight, formatTime(now), formatTime(now)); err != nil {
			return domain.ProviderChannel{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.ProviderChannel{}, err
	}
	channel.CreatedAt = now
	channel.UpdatedAt = now
	return channel, nil
}

// SoftDeleteProviderChannel sets deleted_at on the row so ListProviderChannels
// excludes it immediately, and keeps excluding it across Hub restarts — the
// tombstone that used to live in an in-memory map. It also rewrites label to a
// tombstone value keyed by the channel id ("<label> [deleted:<id>]"): the
// UNIQUE(capability,label) constraint on provider_channels is on the live
// column, not scoped by deleted_at, so a deleted row would otherwise keep
// occupying that (capability,label) pair forever and block recreating a
// channel with the same name. Rewriting the label frees the pair for reuse
// while ListProviderChannels' `deleted_at IS NULL` filter keeps the mangled
// value from ever reaching the UI or API. A future hard-delete migration
// would let us drop the row instead, but rebuilding provider_channels today
// would mean DROP TABLE, and provider_channel_members/provider_channel_events
// reference it with ON DELETE CASCADE — inside the transaction migrations run
// in, PRAGMA foreign_keys has no effect, so that DROP would silently cascade
// and wipe those child tables. Rewriting the label in place avoids that risk.
func (r *Repository) SoftDeleteProviderChannel(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("provider channel id is required")
	}
	_, err := r.db.ExecContext(ctx, `UPDATE provider_channels SET deleted_at=?, label=label || ' [deleted:' || id || ']' WHERE id=?`, formatTime(time.Now().UTC()), id)
	return err
}

func (r *Repository) ListProviderChannels(ctx context.Context, capability string) ([]domain.ProviderChannel, error) {
	query := `SELECT id,capability,label,provider_name,protocol,endpoint,model,enabled,route_order,cost_per_request,cost_per_video_minute,cost_per_audio_minute,created_at,updated_at FROM provider_channels WHERE deleted_at IS NULL`
	args := []any{}
	if capability = strings.TrimSpace(capability); capability != "" {
		query += ` AND capability=?`
		args = append(args, capability)
	}
	query += ` ORDER BY capability,route_order,label`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var channels []domain.ProviderChannel
	for rows.Next() {
		var channel domain.ProviderChannel
		var enabled int
		var created, updated string
		if err := rows.Scan(&channel.ID, &channel.Capability, &channel.Label, &channel.ProviderName, &channel.Protocol, &channel.Endpoint, &channel.Model, &enabled, &channel.RouteOrder, &channel.CostPerRequest, &channel.CostPerVideoMinute, &channel.CostPerAudioMinute, &created, &updated); err != nil {
			return nil, err
		}
		channel.Enabled = enabled != 0
		channel.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		channel.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(channels) == 0 {
		return channels, nil
	}

	// Batch-fetch all members in chunks to avoid N+1 while staying under
	// SQLite's default max variable limit (999). 500 is a safe per-chunk size
	// that keeps the query plan stable and the argument count well below the
	// driver threshold.
	const chunkSize = 500
	byID := make(map[string]*domain.ProviderChannel, len(channels))
	for i := range channels {
		byID[channels[i].ID] = &channels[i]
	}
	channelIDs := make([]string, 0, len(channels))
	for i := range channels {
		channelIDs = append(channelIDs, channels[i].ID)
	}
	for start := 0; start < len(channelIDs); start += chunkSize {
		end := start + chunkSize
		if end > len(channelIDs) {
			end = len(channelIDs)
		}
		chunk := channelIDs[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		chunkArgs := make([]any, len(chunk))
		for i, id := range chunk {
			chunkArgs[i] = id
		}
		memberRows, err := r.db.QueryContext(ctx, `SELECT id,channel_id,label,secret_ref,enabled,weight,max_inflight FROM provider_channel_members WHERE channel_id IN (`+placeholders+`) ORDER BY channel_id,label`, chunkArgs...)
		if err != nil {
			return nil, err
		}
		for memberRows.Next() {
			var member domain.ProviderChannelMember
			var memberEnabled int
			if err := memberRows.Scan(&member.ID, &member.ChannelID, &member.Label, &member.SecretRef, &memberEnabled, &member.Weight, &member.MaxInflight); err != nil {
				memberRows.Close()
				return nil, err
			}
			member.Enabled = memberEnabled != 0
			if ch, ok := byID[member.ChannelID]; ok {
				ch.Members = append(ch.Members, member)
			}
		}
		if err := memberRows.Err(); err != nil {
			memberRows.Close()
			return nil, err
		}
		memberRows.Close()
	}
	return channels, nil
}

// RebuildAutomaticShootSessions materializes deterministic, conservative
// capture groups for one library root. Manual sessions are intentionally left
// untouched; source files and metadata are never modified.
func (r *Repository) RebuildAutomaticShootSessions(ctx context.Context, rootID string) error {
	rows, err := r.db.QueryContext(ctx, `SELECT a.id,COALESCE(al.relative_path,''),COALESCE(cm.vendor,''),COALESCE(cm.model,''),COALESCE(cm.device_serial,''),cm.captured_at,COALESCE(m.duration_ms,0),COALESCE(cm.session_marker,''),COALESCE(cm.reel,'') FROM assets a JOIN asset_locations al ON al.asset_id=a.id AND al.root_id=? AND al.exists_now=1 LEFT JOIN capture_metadata cm ON cm.asset_id=a.id LEFT JOIN media_metadata m ON m.asset_id=a.id WHERE cm.captured_at IS NOT NULL GROUP BY a.id`, rootID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var captures []capture.Capture
	for rows.Next() {
		var item capture.Capture
		var vendor, model, serial, captured, sessionMarker, reel string
		var durationMS int64
		if err := rows.Scan(&item.ID, &item.Context.Filename, &vendor, &model, &serial, &captured, &durationMS, &sessionMarker, &reel); err != nil {
			return err
		}
		item.Source = capture.SourceOriginal
		item.Identity = capture.CaptureIdentity{DeviceID: strings.TrimSpace(vendor + ":" + model), DeviceSerial: serial, Manufacturer: vendor, Model: model}
		item.Context.CapturedAt, _ = time.Parse(time.RFC3339Nano, captured)
		item.Context.Duration = time.Duration(durationMS) * time.Millisecond
		item.Context.SessionMarker = sessionMarker
		item.Context.ReelMarker = reel
		captures = append(captures, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sessions := capture.AggregateSessions(captures)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM shoot_sessions WHERE root_id=? AND state='automatic'`, rootID); err != nil {
		return err
	}
	now := formatTime(time.Now())
	for _, session := range sessions {
		id := idgen.New()
		cameraLabel := strings.TrimSpace(session.Identity.Manufacturer + " " + session.Identity.Model)
		title := cameraLabel
		if !session.Start.IsZero() {
			title = session.Start.Local().Format("2006-01-02 15:04") + " · " + cameraLabel
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO shoot_sessions(id,root_id,title,state,starts_at,ends_at,camera_label,confidence,created_at,updated_at) VALUES(?,?,?,'automatic',?,?,?,?,?,?)`, id, rootID, title, nullableTime(&session.Start), nullableTime(&session.End), cameraLabel, 0.8, now, now); err != nil {
			return err
		}
		for _, item := range session.Captures {
			if _, err := tx.ExecContext(ctx, `INSERT INTO asset_shoot_sessions(asset_id,session_id,is_primary,created_at) VALUES(?,?,1,?)`, item.ID, id, now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (r *Repository) SaveMediaMetadata(ctx context.Context, assetID string, m domain.MediaMetadata, version string) error {
	norm, _ := json.Marshal(m)
	_, err := r.db.ExecContext(ctx, `INSERT INTO media_metadata(asset_id,ffprobe_json,exiftool_json,normalized_json,probe_version,updated_at,duration_ms,width,height,fps,video_codec,audio_codec,has_audio,orientation,captured_at,camera_model,latitude,longitude)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(asset_id) DO UPDATE SET ffprobe_json=excluded.ffprobe_json,exiftool_json=excluded.exiftool_json,normalized_json=excluded.normalized_json,probe_version=excluded.probe_version,updated_at=excluded.updated_at,duration_ms=excluded.duration_ms,width=excluded.width,height=excluded.height,fps=excluded.fps,video_codec=excluded.video_codec,audio_codec=excluded.audio_codec,has_audio=excluded.has_audio,orientation=excluded.orientation,captured_at=excluded.captured_at,camera_model=excluded.camera_model,latitude=excluded.latitude,longitude=excluded.longitude`, assetID, m.FFProbeRaw, m.ExifToolRaw, string(norm), version, formatTime(time.Now()), m.DurationMS, m.Width, m.Height, m.FPS, m.VideoCodec, m.AudioCodec, boolInt(m.HasAudio), m.Orientation, nullableTime(m.CapturedAt), m.CameraModel, m.Latitude, m.Longitude)
	if err != nil {
		return err
	}
	previewStatus := m.PreviewStatus
	if previewStatus == "" {
		previewStatus = "unknown"
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO capture_metadata(asset_id,vendor,make,model,device_serial,captured_at,capture_time_source,capture_time_confidence,latitude,longitude,location_source,location_precision,source_color,color_profile,raw_format,preview_status,normalized_json,updated_at)
VALUES(?,?,?,?,?,?,?, ?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(asset_id) DO UPDATE SET vendor=excluded.vendor,make=excluded.make,model=excluded.model,device_serial=excluded.device_serial,captured_at=excluded.captured_at,capture_time_source=excluded.capture_time_source,capture_time_confidence=excluded.capture_time_confidence,latitude=excluded.latitude,longitude=excluded.longitude,location_source=excluded.location_source,location_precision=excluded.location_precision,source_color=excluded.source_color,color_profile=excluded.color_profile,raw_format=excluded.raw_format,preview_status=excluded.preview_status,normalized_json=excluded.normalized_json,updated_at=excluded.updated_at`,
		assetID, m.CaptureVendor, m.CameraMake, m.CameraModel, m.CameraSerial, nullableTime(m.CapturedAt), captureTimeSource(m), captureTimeConfidence(m), m.Latitude, m.Longitude, captureLocationSource(m), captureLocationPrecision(m), m.SourceColor, m.ColorProfile, m.RawFormat, previewStatus, string(norm), formatTime(time.Now()))
	return err
}

func captureTimeSource(m domain.MediaMetadata) string {
	if m.CapturedAt == nil {
		return ""
	}
	if m.ExifToolRaw != "" {
		return "embedded"
	}
	return "probe"
}

func captureTimeConfidence(m domain.MediaMetadata) float64 {
	if m.CapturedAt == nil {
		return 0
	}
	if m.ExifToolRaw != "" {
		return 0.9
	}
	return 0.6
}

func captureLocationSource(m domain.MediaMetadata) string {
	if m.Latitude == nil || m.Longitude == nil {
		return ""
	}
	return "embedded"
}

func captureLocationPrecision(m domain.MediaMetadata) string {
	if m.Latitude == nil || m.Longitude == nil {
		return ""
	}
	return "precise"
}

func (r *Repository) GetMediaMetadata(ctx context.Context, assetID string) (*domain.MediaMetadata, error) {
	var raw string
	err := r.db.QueryRowContext(ctx, `SELECT normalized_json FROM media_metadata WHERE asset_id=?`, assetID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m domain.MediaMetadata
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// SaveArtifact upserts a derived artifact under the job that produced it.
// jobID and owner tie the write to that job's lease for the same reason
// CompleteJob does below: JobDerive renders a thumbnail, proxy, and (when the
// source has audio) an audio extract synchronously, inside one job execution,
// and that render can run well past the Hub-local pipeline's fixed,
// never-renewed 2-minute lease on a long clip -- the same window that makes
// the completion race reachable. Without the same predicate here, a holder
// that already lost the job to a reclaim (a paired Worker on `derive`, or a
// second Hub process) would still persist its stale render over whatever the
// new holder wrote, silently, since this call is the only I/O JobDerive does
// between renders. See leaseLostErr for what a CAS miss here means to the
// caller.
//
// The predicate is expressed as a WHERE EXISTS on the SELECT side of an
// INSERT...SELECT rather than a separate check-then-write: this repository's
// convention (see LeaseNextJob's UPDATE) is to push a compare-and-swap into
// the write itself, because a preceding read leaves a gap for a concurrent
// reclaim to land in between.
func (r *Repository) SaveArtifact(ctx context.Context, a domain.DerivedArtifact, jobID, owner string) error {
	now := time.Now()
	res, err := r.db.ExecContext(ctx, `INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) SELECT ?,?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM jobs WHERE id=? AND lease_owner=? AND state='running' AND lease_expires_at>?) ON CONFLICT(asset_id,artifact_type,profile_hash) DO UPDATE SET local_path=excluded.local_path,size_bytes=excluded.size_bytes`, a.ID, a.AssetID, a.Type, a.ProfileHash, a.LocalPath, a.SizeBytes, formatTime(now), jobID, owner, formatTime(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return leaseLostErr(jobID, owner)
	}
	return nil
}
func (r *Repository) GetArtifact(ctx context.Context, assetID, typ string) (*domain.DerivedArtifact, error) {
	var a domain.DerivedArtifact
	err := r.db.QueryRowContext(ctx, `SELECT id,asset_id,artifact_type,profile_hash,local_path,size_bytes FROM derived_artifacts WHERE asset_id=? AND artifact_type=? ORDER BY created_at DESC LIMIT 1`, assetID, typ).Scan(&a.ID, &a.AssetID, &a.Type, &a.ProfileHash, &a.LocalPath, &a.SizeBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

func (r *Repository) SaveSpeechClassification(ctx context.Context, assetID string, c domain.SpeechClassification) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO speech_classifications(asset_id,classification,speech_probability,reason,classifier_version,raw_json,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(asset_id) DO UPDATE SET classification=excluded.classification,speech_probability=excluded.speech_probability,reason=excluded.reason,classifier_version=excluded.classifier_version,raw_json=excluded.raw_json,updated_at=excluded.updated_at`, assetID, c.Classification, c.SpeechProbability, c.Reason, "ffmpeg-volumedetect-v1", c.RawJSON, formatTime(time.Now()))
	return err
}

func (r *Repository) EnqueueJob(ctx context.Context, assetID string, typ domain.JobType, inputHash string, priority int) error {
	_, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at) VALUES(?,?,?,'pending',?,0,3,?,?,?,?)`, idgen.New(), assetID, string(typ), priority, formatTime(time.Now()), inputHash, formatTime(time.Now()), formatTime(time.Now()))
	return err
}

// LeaseNextJob atomically claims the next leasable job for worker. ttlFor, when
// non-nil, picks the lease duration from the job's type once it is known (a
// long derive or windowed analysis outlasts any fixed short lease, and an
// expired lease is what lets a second holder reclaim the work and pay for it
// again); nil means a fixed 2 minutes. The duration is a heuristic ceiling,
// not a contract: a lease that expires is simply reclaimed, and the losing
// holder's completion writes all miss their CAS.
func (r *Repository) LeaseNextJob(ctx context.Context, worker string, ttlFor func(domain.JobType) time.Duration, filter domain.LeaseFilter) (*domain.Job, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now()
	// A Worker (or the Hub's own local pipeline, which leases through this
	// same path) that dies mid-job used to leave the job at state='running'
	// forever: nothing else ever transitioned it back. Reclaiming an exhausted
	// lease runs inline with every lease attempt rather than through a
	// background sweeper, so it still resolves even when nobody happens to be
	// asking for that job's type of work right now. See reclaimExhaustedLeases
	// (remote_jobs.go) for why it also applies to Worker-derive leases.
	if err := reclaimExhaustedLeases(ctx, tx, now); err != nil {
		return nil, err
	}
	var j domain.Job
	var run string
	var last sql.NullString
	// INDEXED BY is deliberate, not a hint: without it SQLite picks
	// idx_jobs_ready and sorts every leasable row into a temp b-tree on each
	// lease. idx_jobs_lease_order (migration 0018, widened by 0020 to also
	// admit 'running') already stores the rows in ORDER BY sequence, so the
	// scan stops at the first match. Its partial WHERE clause must keep
	// matching the terms below or SQLite rejects the query outright -- a loud
	// failure, which is the point.
	//
	// state also admits 'running': an expired lease -- the Worker or the Hub
	// itself died mid-job -- is exactly as leasable as a job that never
	// started. The (lease_expires_at IS NULL OR lease_expires_at<=?) term
	// below is what keeps a *live* running job out of the result, since
	// pending/failed jobs always carry a NULL lease_expires_at, so widening
	// the state list costs nothing for the ordinary case.
	//
	// assigned_worker_id IS NULL keeps this Hub-local lease from stealing a
	// job an admin pinned to one specific Worker with
	// WorkerAssignmentRequired (SetDeriveWorkerAssignment, remote_jobs.go):
	// the Hub is not that Worker, and "required" means required. Preferred
	// assignments are unaffected -- those only reorder
	// LeaseNextWorkerDerive's candidates among capable Workers, they never
	// restrict who else may take the job.
	//
	// The size ceiling is expressed as "no oversized asset exists for this job"
	// rather than as a join so the shape above survives: a join would give the
	// planner a second table to order by and could cost the ordered scan. The
	// subquery is a primary-key lookup, and it is skipped entirely when the
	// ceiling is zero.
	err = tx.QueryRowContext(ctx, `SELECT id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,last_error_message FROM jobs INDEXED BY idx_jobs_lease_order WHERE state IN ('pending','failed','running') AND terminal=0 AND assigned_worker_id IS NULL AND attempt_count<max_attempts AND run_after<=? AND (lease_expires_at IS NULL OR lease_expires_at<=?) AND (?=0 OR NOT EXISTS (SELECT 1 FROM assets a WHERE a.id=jobs.asset_id AND a.file_size>?)) ORDER BY priority DESC,created_at LIMIT 1`, formatTime(now), formatTime(now), filter.MaxAssetBytes, filter.MaxAssetBytes).Scan(&j.ID, &j.AssetID, &j.Type, &j.State, &j.Priority, &j.AttemptCount, &j.MaxAttempts, &run, &j.InputHash, &last)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing to lease, but reclaimExhaustedLeases above may still have
		// terminally failed an unrelated exhausted job in this same
		// transaction; that write has to survive, or a job that repeatedly
		// kills its Worker never actually goes terminal -- every attempt to
		// clean it up would find nothing to lease and roll itself back out.
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	j.RunAfter, _ = time.Parse(time.RFC3339Nano, run)
	if last.Valid {
		j.LastError = last.String
	}
	// The "OR (state='running' AND lease_expires_at<=?)" arm re-checks expiry
	// at UPDATE time, not just at SELECT time, so a lease that got renewed (or
	// reclaimed by a concurrent transaction) in the gap between this
	// transaction's SELECT and this UPDATE loses the race instead of stealing
	// a job that is actually still live -- RowsAffected below reports 0 either
	// way, the same CAS-miss handling every other lease path in this package
	// uses.
	lease := 2 * time.Minute
	if ttlFor != nil {
		if ttl := ttlFor(j.Type); ttl > 0 {
			lease = ttl
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state='running',attempt_count=attempt_count+1,lease_owner=?,lease_expires_at=?,updated_at=? WHERE id=? AND assigned_worker_id IS NULL AND (state IN ('pending','failed') OR (state='running' AND lease_expires_at<=?))`, worker, formatTime(now.Add(lease)), formatTime(now), j.ID, formatTime(now))
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		// Lost the CAS race for this job, but reclaimExhaustedLeases's write
		// still needs to survive -- see the comment on the ErrNoRows branch
		// above.
		return nil, tx.Commit()
	}
	j.State = domain.JobRunning
	j.AttemptCount++
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &j, nil
}

// leaseLostErr reports that id's lease is no longer held by owner: either
// something else's compare-and-swap won it first, or this owner's own lease
// had already expired when the write landed (lease_expires_at>? is checked
// at write time everywhere below, not just at whatever point the caller
// started its work, for the same reason LeaseNextJob's UPDATE re-checks
// expiry instead of trusting its SELECT). Wraps domain.ErrJobLeaseLost —
// see that sentinel's doc comment for why callers must match it with
// errors.Is rather than this message, which is free to reword. id and owner
// stay in the text because they are useful in a log line, not because
// anything parses them back out.
func leaseLostErr(id, owner string) error {
	return fmt.Errorf("job %s: lease no longer held by %s: %w", id, owner, domain.ErrJobLeaseLost)
}

// jobLeaseActive reports whether id is presently 'running' under owner's
// unexpired lease. The completion writes below never rely on this read for
// correctness -- each one's own UPDATE/INSERT WHERE clause is the actual
// compare-and-swap, exactly like LeaseNextJob's. This exists only to answer
// a narrower question after a CAS miss: RetryJob's predicate can also miss
// for a reason that has nothing to do with ownership (attempt_count already
// at max_attempts, which is an expected, silent no-op -- see
// TestDeferJobOnTheLastAttemptStillLeavesTheJobLeasable), so RetryJob uses
// this to tell that apart from an actual lost lease before deciding whether
// to report leaseLostErr.
func (r *Repository) jobLeaseActive(ctx context.Context, id, owner string, now time.Time) (bool, error) {
	var exists int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id=? AND lease_owner=? AND state='running' AND lease_expires_at>?`, id, owner, formatTime(now)).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// CompleteJob records a Hub-local job's outcome. owner is the identity
// RunUntilIdle minted for this pipeline run and passed to LeaseNextJob
// (pipeline.go); the WHERE clause is the missing half of the
// compare-and-swap LeaseNextJob performs on acquisition -- this branch made
// an expired lease reclaimable (state also admits 'running' with a stale
// lease_expires_at; see LeaseNextJob's comment), and the Hub-local pipeline
// leases for a fixed, never-renewed 2 minutes while running synchronous work
// (a software x264 proxy of a long clip, a windowed analysis) that routinely
// exceeds it. Without owner+expiry here, a holder that already lost the job
// to a second Hub process or a paired Worker on `derive` would still
// overwrite whatever the new holder wrote, and the same job could run
// twice -- on analyze, that is a duplicate paid provider call.
func (r *Repository) CompleteJob(ctx context.Context, id, owner string, state domain.JobState, errMsg string) error {
	now := time.Now()
	res, err := r.db.ExecContext(ctx, `UPDATE jobs SET state=?,last_error_message=?,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND lease_owner=? AND state='running' AND lease_expires_at>?`, string(state), nullString(errMsg), formatTime(now), id, owner, formatTime(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return leaseLostErr(id, owner)
	}
	return nil
}

// FailJobTerminally marks a job failed and flags it as terminal so the lease
// predicate stops returning it. CompleteJob alone leaves state='failed' with
// attempts still available, and LeaseNextJob deliberately picks failed jobs back
// up — so a failure the pipeline classified as permanent would otherwise be
// retried anyway, immediately and without even the backoff a retryable error
// gets. For a provider 4xx that means paying for the same rejected call again.
//
// category rides into jobs.last_error_code alongside the message: the column
// already holds defer reasons (which are category values themselves), and the
// issues view aggregates failures by reading exactly this code, so a terminal
// failure's category must be written when the terminal verdict is.
//
// attempt_count is left alone on purpose. An earlier implementation exhausted it
// to make the predicate skip the row, which worked but reported a job that ran
// once as "3/3" on the progress page.
//
// owner and the lease_owner/lease_expires_at predicate are the same CAS
// CompleteJob adds, and for the same reason: a stale holder must not flip a
// job someone else has already reclaimed to terminal failure out from under
// them.
func (r *Repository) FailJobTerminally(ctx context.Context, id, owner string, category domain.JobFailureCategory, errMsg string) error {
	now := time.Now()
	res, err := r.db.ExecContext(ctx, `UPDATE jobs SET state=?,terminal=1,last_error_code=?,last_error_message=?,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND lease_owner=? AND state='running' AND lease_expires_at>?`, string(domain.JobFailed), nullString(string(category)), nullString(errMsg), formatTime(now), id, owner, formatTime(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return leaseLostErr(id, owner)
	}
	return nil
}

// RequeueFailedJobs puts every failed job back in the queue with a fresh
// attempt budget. It exists because the two ways a job stops being retried —
// exhausting its attempts and being classified permanent — are both one-way:
// EnqueueJob is INSERT OR IGNORE keyed on (asset_id, job_type, input_hash), so
// rescanning the library does not revive them. The common case is a provider
// that was not configured yet; once it is, the operator needs a way to say so.
//
// Only failed jobs are touched. A running job belongs to whoever holds its
// lease, and succeeded or skipped jobs are the idempotency record that keeps
// re-running the pipeline cheap.
func (r *Repository) RequeueFailedJobs(ctx context.Context) (int, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE jobs SET state='pending',terminal=0,attempt_count=0,run_after=?,last_error_message=NULL,last_error_code=NULL,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE state='failed'`, formatTime(time.Now()), formatTime(time.Now()))
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	return int(affected), err
}

// RequeueFailedJobsByCategory is RequeueFailedJobs narrowed to one failure
// category, so the operator can revive the work a single provider problem
// stranded without touching everything else. It is the write half of the
// issues view: the category expression here must be the same one
// JobIssues groups by, and it is — both use
// COALESCE(NULLIF(last_error_code,”),'unknown'), so 'unknown' matches the
// jobs whose failure carried no code (NULL or empty), and every other value
// matches last_error_code literally. An empty-string category therefore
// matches nothing: the API keeps "" meaning "everything" and lets this
// method's WHERE clause do the narrowing.
//
// The parked-deferral half of a category needs no new method: ResumeDeferredJobs
// already takes the category as its reason parameter (its WHERE clause
// compares last_error_code=?), so a category-scoped release is just
// ResumeDeferredJobs(category).
func (r *Repository) RequeueFailedJobsByCategory(ctx context.Context, category string) (int, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE jobs SET state='pending',terminal=0,attempt_count=0,run_after=?,last_error_message=NULL,last_error_code=NULL,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE state='failed' AND COALESCE(NULLIF(last_error_code,''),'unknown')=?`, formatTime(time.Now()), formatTime(time.Now()), category)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	return int(affected), err
}

// RetryJob returns a leased job to the pending queue after a bounded delay.
// Leasing already increments attempt_count, so the normal lease predicate
// enforces max_attempts without a separate mutable retry counter.
//
// The WHERE clause carries two independent conditions that can each make it
// match zero rows, and they mean different things: lease_owner/lease_expires_at
// is the same CAS CompleteJob adds (a stale holder must not reschedule a job
// someone else's lease now covers), while attempt_count<max_attempts is the
// pre-existing, expected case where RetryJob simply cannot help anymore (see
// TestDeferJobOnTheLastAttemptStillLeavesTheJobLeasable, which is the only
// path back to leasable once attempts are spent). Only the first is an error
// worth reporting -- the miss is disambiguated after the fact with
// jobLeaseActive, which does not change what the CAS above already decided.
//
// category is written into jobs.last_error_code so a retrying job still
// carries the machine-readable reason of its last failure; the issues view
// aggregates the column whether the job is currently retrying, parked, or
// terminal.
func (r *Repository) RetryJob(ctx context.Context, id, owner string, category domain.JobFailureCategory, errMsg string, delay time.Duration) error {
	if delay < 0 {
		delay = 0
	}
	now := time.Now()
	res, err := r.db.ExecContext(ctx, `UPDATE jobs SET state='pending',run_after=?,last_error_code=?,last_error_message=?,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND lease_owner=? AND state='running' AND lease_expires_at>? AND attempt_count<max_attempts`, formatTime(now.Add(delay)), nullString(string(category)), nullString(errMsg), formatTime(now), id, owner, formatTime(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	stillOwned, err := r.jobLeaseActive(ctx, id, owner, now)
	if err != nil {
		return err
	}
	if !stillOwned {
		return leaseLostErr(id, owner)
	}
	return nil
}

// DeferJob parks a running job on wall-clock time and hands back the attempt
// this lease spent. It is for failures that are not the job's fault: when every
// provider key on a capability's route is failing at once — a hard monthly
// quota answers 429 on all of them together — the job never ran, the account
// did. Charging that to the job would spend its whole budget on an outage and
// then fail it permanently for something only waiting can fix.
//
// The row is left exactly where the lease predicate can pick it up again once
// run_after passes: state='pending', terminal untouched at 0, lease_expires_at
// cleared, and attempt_count back to what it was before this lease — which is
// by definition still below max_attempts, since the lease predicate is what
// let the job run. Without the decrement the third defer would leave
// attempt_count=max_attempts and the job would never be leased again, so the
// five-hour wait would park it forever rather than resume it.
//
// reason is a Hub-assigned constant, never upstream text: ListJobs turns it
// into the queue's visible "waiting on provider quota" state, while errMsg
// stays behind the admin token in last_error_message.
// The lease_owner/lease_expires_at predicate is the same CAS CompleteJob
// adds, for the same reason: a stale holder must not park a job someone
// else's lease now covers, handing back an attempt that was never this
// caller's to hand back.
func (r *Repository) DeferJob(ctx context.Context, id, owner string, until time.Time, reason, errMsg string) error {
	now := time.Now()
	if until.Before(now) {
		until = now
	}
	res, err := r.db.ExecContext(ctx, `UPDATE jobs SET state='pending',run_after=?,attempt_count=MAX(attempt_count-1,0),last_error_code=?,last_error_message=?,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND lease_owner=? AND state='running' AND lease_expires_at>?`, formatTime(until), nullString(reason), nullString(errMsg), formatTime(now), id, owner, formatTime(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return leaseLostErr(id, owner)
	}
	return nil
}

// ResumeDeferredJobs pulls every job waiting on provider quota forward to now
// and returns how many moved.
//
// Without this an operator has no way out of the wait: a deferred job is
// 'pending', so RequeueFailedJobs — which only touches 'failed' — does not see
// it, and topping up an account or watching a brief outage clear would still
// cost the full five hours. It also bounds the cost of deferring on a
// transient failure, which is the price of treating a whole route going quiet
// as exhaustion.
//
// run_after>? keeps this to jobs actually still waiting, so it cannot disturb
// the scheduling of work that was postponed for any other reason, and the
// reason code is cleared because the wait it described is over.
func (r *Repository) ResumeDeferredJobs(ctx context.Context, reason string) (int, error) {
	now := time.Now()
	result, err := r.db.ExecContext(ctx, `UPDATE jobs SET run_after=?,last_error_code=NULL,updated_at=? WHERE state='pending' AND terminal=0 AND last_error_code=? AND run_after>?`, formatTime(now), formatTime(now), reason, formatTime(now))
	if err != nil {
		return 0, err
	}
	moved, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(moved), nil
}

// isDeferCode reports whether a last_error_code is a parked-job deferral
// (provider route exhausted, disk space low, or budget exhausted) rather than
// an ordinary failure. It is the single enumeration both ListJobs and
// JobSummary use so the two cannot disagree about which jobs are parked.
func isDeferCode(code string) bool {
	return code == domain.JobDeferProviderRouteExhausted || code == domain.JobDeferDiskSpaceLow || code == domain.JobDeferBudgetExhausted
}

func (r *Repository) ListJobs(ctx context.Context, limit int) ([]domain.Job, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT j.id,COALESCE(j.asset_id,''),j.job_type,j.state,j.priority,j.attempt_count,j.max_attempts,j.run_after,j.input_hash,COALESCE(j.last_error_message,''),j.terminal,COALESCE(j.last_error_code,''),COALESCE((SELECT loc.relative_path FROM asset_locations loc WHERE loc.asset_id=j.asset_id AND loc.is_primary=1 LIMIT 1),'') FROM jobs j ORDER BY j.created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	var out []domain.Job
	for rows.Next() {
		var j domain.Job
		var run, code, location string
		if err := rows.Scan(&j.ID, &j.AssetID, &j.Type, &j.State, &j.Priority, &j.AttemptCount, &j.MaxAttempts, &run, &j.InputHash, &j.LastError, &j.Terminal, &code, &location); err != nil {
			return nil, err
		}
		if location != "" {
			j.Filename = filepath.Base(location)
		}
		j.RunAfter, _ = time.Parse(time.RFC3339Nano, run)
		// A defer is only in force while the job is still parked. Reading the
		// error code alone would keep reporting "waiting on quota" after the
		// job resumed and even after it succeeded, because nothing clears the
		// code of the last failure; state plus run_after is the actual truth
		// about whether the job is waiting, and it is the same pair the lease
		// predicate reads.
		if isDeferCode(code) && j.State == domain.JobPending && j.RunAfter.After(now) {
			j.DeferredReason = code
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobSummary counts the queue by category in one pass. The deferred predicate
// is deliberately the same pair ListJobs uses — the error code alone would keep
// reporting "waiting on quota" long after the job resumed, because nothing
// clears the code of the last failure — so the metric row and the table below
// it cannot disagree about which jobs are parked.
func (r *Repository) JobSummary(ctx context.Context) (domain.JobSummary, error) {
	var summary domain.JobSummary
	// COALESCE, not a bare comparison: last_error_code is nullable and NULL for
	// every job that has never failed, and `NULL=?` is NULL rather than false.
	// Negating that in the pending arm below would yield NULL too, quietly
	// dropping every never-failed pending job out of the count -- which is most
	// of the queue. Deferred matches any defer code (provider route
	// exhausted, disk space low, budget exhausted) so a parked job is counted
	// exactly once under the deferred bucket.
	deferred := `state='pending' AND COALESCE(last_error_code,'') IN (?,?,?) AND run_after>?`
	query := `SELECT
        COALESCE(SUM(CASE WHEN state='pending' AND NOT (` + deferred + `) THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN state='running' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN state='succeeded' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN state='failed' AND terminal=0 THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN terminal=1 THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN ` + deferred + ` THEN 1 ELSE 0 END),0),
        COUNT(*)
    FROM jobs`
	now := formatTime(time.Now())
	err := r.db.QueryRowContext(ctx, query,
		domain.JobDeferProviderRouteExhausted, domain.JobDeferDiskSpaceLow, domain.JobDeferBudgetExhausted, now,
		domain.JobDeferProviderRouteExhausted, domain.JobDeferDiskSpaceLow, domain.JobDeferBudgetExhausted, now).
		Scan(&summary.Pending, &summary.Running, &summary.Succeeded, &summary.Failed, &summary.Terminal, &summary.Deferred, &summary.Total)
	if err != nil {
		return domain.JobSummary{}, err
	}
	return summary, nil
}

// RebuildSearch replaces assetID's row in asset_search with a fresh one
// computed from its current analysis/transcript. The delete-then-insert pair
// runs as a single transaction so a crash between them can't leave the asset
// missing from the FTS index until it happens to be re-analysed.
func (r *Repository) RebuildSearch(ctx context.Context, assetID string) error {
	loc, err := r.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		return err
	}
	var summary, transcript, tags, subjects, moods, extra, reason string
	if err := r.db.QueryRowContext(ctx, `SELECT summary,scene_tags_json,subjects_json,mood_tags_json,extra_tags_json,editorial_reason FROM asset_analysis WHERE asset_id=?`, assetID).Scan(&summary, &tags, &subjects, &moods, &extra, &reason); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT full_text FROM transcripts WHERE asset_id=? AND status='succeeded' ORDER BY created_at DESC LIMIT 1`, assetID).Scan(&transcript); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := r.rebuildSearchTx(ctx, tx, assetID, filepath.Base(loc.AbsolutePath), summary, transcript, tags, subjects, moods, extra, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// RebuildAllSearch repairs the asset-level index from canonical analysis and
// successful transcript rows. It deliberately continues after an individual
// asset failure so one stale location cannot hide the rest of the repair.
func (r *Repository) RebuildAllSearch(ctx context.Context) (int, []string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT a.id FROM assets a WHERE EXISTS (SELECT 1 FROM asset_analysis an WHERE an.asset_id=a.id) OR EXISTS (SELECT 1 FROM transcripts t WHERE t.asset_id=a.id AND t.status='succeeded') ORDER BY a.id`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	rebuilt := 0
	var failures []string
	for _, id := range ids {
		if err := r.RebuildSearch(ctx, id); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		rebuilt++
	}
	return rebuilt, failures, nil
}

// rebuildSearchTx assumes the caller already owns the transaction, so this
// delete+insert pair can be composed into a larger transaction later without
// nesting BeginTx calls. RebuildSearch is currently the only caller and it
// begins its own transaction, but a future caller inside commitAnalysisTx (or
// similar) should call this helper directly instead of RebuildSearch.
func (r *Repository) rebuildSearchTx(ctx context.Context, tx *sql.Tx, assetID, filename, summary, transcript, tags, subjects, moods, extra, reason string) error {
	if err := r.deleteFromSearchIndexTx(ctx, tx, assetID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO asset_search(asset_id,filename,summary,transcript,scene_tags,subjects,mood_tags,extra_tags,location,editorial_reason) VALUES(?,?,?,?,?,?,?,?,?,?)`, assetID, indexText(filename), indexText(summary), indexText(transcript), indexText(tags), indexText(subjects), indexText(moods), indexText(extra), "", indexText(reason))
	if err != nil {
		return err
	}
	rowid, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO asset_search_rowids(asset_id,search_rowid) VALUES(?,?) ON CONFLICT(asset_id) DO UPDATE SET search_rowid=excluded.search_rowid`, assetID, rowid)
	return err
}

// deleteFromSearchIndexTx removes assetID's row (if any) from asset_search.
// asset_search declares asset_id UNINDEXED (migrations/0002_pipeline.sql), so
// FTS5 builds no secondary index for it and `DELETE ... WHERE asset_id=?`
// would scan the whole shadow table. asset_search_rowids resolves the FTS5
// rowid first so the delete can use `WHERE rowid=?` instead, which FTS5 can
// serve directly. Keep this mapping in sync on every insert/delete path.
func (r *Repository) deleteFromSearchIndexTx(ctx context.Context, tx *sql.Tx, assetID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_search WHERE rowid IN (SELECT search_rowid FROM asset_search_rowids WHERE asset_id=?)`, assetID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM asset_search_rowids WHERE asset_id=?`, assetID)
	return err
}

func (r *Repository) ReplaceAssetShots(ctx context.Context, assetID, sourceRunID string, shots []domain.AssetShot, lease ...string) error {
	if err := validateAssetShots(shots); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := r.replaceAssetShotsTx(ctx, tx, assetID, sourceRunID, shots); err != nil {
		return err
	}
	jobID, owner := leaseParams(lease)
	if err := leaseGuard(ctx, tx, jobID, assetID, owner); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.clearSemanticVectorCache()
	return nil
}

// CommitShotRefinement replaces the canonical shot set and commits the
// refinement run as one transaction. The lease assertion follows all writes,
// so a stale refinement cannot leave either shots or model-run state behind.
func (r *Repository) CommitShotRefinement(ctx context.Context, assetID, runID string, shots []domain.AssetShot, lease ...string) error {
	if err := validateAssetShots(shots); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := r.replaceAssetShotsTx(ctx, tx, assetID, runID, shots); err != nil {
		return err
	}
	jobID, owner := leaseParams(lease)
	if err := leaseGuard(ctx, tx, jobID, assetID, owner); err != nil {
		return err
	}
	now := formatTime(time.Now().UTC())
	res, err := tx.ExecContext(ctx, `UPDATE model_runs SET state='committed',committed_at=? WHERE id=? AND asset_id=? AND state='validated' AND (?='' OR EXISTS (SELECT 1 FROM jobs WHERE id=? AND asset_id=? AND state='running' AND lease_owner=? AND lease_expires_at>?))`, now, runID, assetID, jobID, jobID, assetID, owner, now)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil {
		return e
	} else if n != 1 {
		if jobID != "" {
			return leaseLostErr(jobID, owner)
		}
		return domain.Permanent(fmt.Errorf("%w: run %s state is not validated", domain.ErrCommitRunNotValidated, runID))
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.clearSemanticVectorCache()
	return nil
}

// replaceAssetShotsTx assumes its caller has already validated every shot.
// Keeping validation outside the delete/insert sequence protects existing,
// trusted rows even when a later model response is malformed.
func (r *Repository) replaceAssetShotsTx(ctx context.Context, tx *sql.Tx, assetID, sourceRunID string, shots []domain.AssetShot) error {
	if err := r.deleteFromShotSearchIndexTx(ctx, tx, assetID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_shots WHERE asset_id=?`, assetID); err != nil {
		return err
	}
	now := time.Now().UTC()
	for i, shot := range shots {
		id := shot.ID
		if id == "" {
			id = idgen.New()
		}
		ordinal := shot.Ordinal
		if ordinal < 0 {
			ordinal = i
		}
		shotJSON := func(value []string) string {
			if value == nil {
				value = []string{}
			}
			encoded, _ := json.Marshal(value)
			return string(encoded)
		}
		created := now
		if !shot.CreatedAt.IsZero() {
			created = shot.CreatedAt
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO asset_shots(id,asset_id,source_run_id,ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, assetID, nullString(sourceRunID), ordinal, shot.StartMS, shot.EndMS, strings.TrimSpace(shot.Description), shotJSON(shot.Tags), shotJSON(shot.Objects), shotJSON(shot.Actions), shotJSON(shot.Mood), shot.Confidence, formatTime(created)); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO asset_shot_search(shot_id,asset_id,description,tags,objects,actions,mood) VALUES(?,?,?,?,?,?,?)`, id, assetID, indexText(shot.Description), indexText(strings.Join(shot.Tags, " ")), indexText(strings.Join(shot.Objects, " ")), indexText(strings.Join(shot.Actions, " ")), indexText(strings.Join(shot.Mood, " ")))
		if err != nil {
			return err
		}
		shotRowID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO asset_shot_search_rowids(shot_id,asset_id,search_rowid) VALUES(?,?,?)`, id, assetID, shotRowID); err != nil {
			return err
		}
		vector, err := json.Marshal(discovery.VectorForShot(domain.AssetShot{Description: shot.Description, Tags: shot.Tags, Objects: shot.Objects, Actions: shot.Actions, Mood: shot.Mood}))
		if err != nil {
			return err
		}
		sourceText := strings.Join(append([]string{shot.Description}, append(append(shot.Tags, shot.Objects...), append(shot.Actions, shot.Mood...)...)...), " ")
		if _, err := tx.ExecContext(ctx, `INSERT INTO shot_semantic_vectors(shot_id,model,vector_json,source_text,created_at) VALUES(?,?,?,?,?)`, id, discovery.HeuristicVectorModel, string(vector), sourceText, formatTime(created)); err != nil {
			return err
		}
	}
	return nil
}

// deleteFromShotSearchIndexTx removes every asset_shot_search row for
// assetID. asset_shot_search also declares asset_id UNINDEXED
// (migrations/0006_v080_shots.sql), so `DELETE ... WHERE asset_id=?` would
// scan the whole shadow table on every analyze commit. Unlike asset_search,
// an asset can have many shot rows, so asset_shot_search_rowids maps each
// shot_id to its FTS5 rowid and is looked up by the indexed asset_id column;
// the delete then targets those rowids directly instead of scanning FTS5.
func (r *Repository) deleteFromShotSearchIndexTx(ctx context.Context, tx *sql.Tx, assetID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_shot_search WHERE rowid IN (SELECT search_rowid FROM asset_shot_search_rowids WHERE asset_id=?)`, assetID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM asset_shot_search_rowids WHERE asset_id=?`, assetID)
	return err
}

// validateAssetShots is the last shape check before shots become canonical
// rows, and every verdict it reaches is a deterministic function of the
// payload handed to it — the same shots would be rejected identically
// forever. Marking permanent here rather than at the two call sites means a
// rule added to assetShotsProblem inherits it, and means both callers
// (CommitAnalysisWithShots on the model path, ReplaceAssetShots on the human
// one) get the same answer without either of them restating it. See
// domain.ErrPermanentFailure for why this is a property of the error rather
// than an entry in a list somewhere else.
func validateAssetShots(shots []domain.AssetShot) error {
	if err := assetShotsProblem(shots); err != nil {
		return domain.Permanent(err)
	}
	return nil
}

func assetShotsProblem(shots []domain.AssetShot) error {
	seenIDs := make(map[string]struct{}, len(shots))
	for i, shot := range shots {
		if shot.StartMS < 0 || shot.EndMS <= shot.StartMS {
			return fmt.Errorf("invalid shot time range at ordinal %d: %d-%d", i, shot.StartMS, shot.EndMS)
		}
		if strings.TrimSpace(shot.Description) == "" {
			return fmt.Errorf("shot description is required at ordinal %d", i)
		}
		if shot.ID != "" {
			if _, exists := seenIDs[shot.ID]; exists {
				return fmt.Errorf("duplicate shot id %q", shot.ID)
			}
			seenIDs[shot.ID] = struct{}{}
		}
	}
	return nil
}

func (r *Repository) ListAssetShots(ctx context.Context, assetID string) ([]domain.AssetShot, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,asset_id,COALESCE(source_run_id,''),ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at FROM asset_shots WHERE asset_id=? ORDER BY ordinal,start_ms`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AssetShot
	for rows.Next() {
		var shot domain.AssetShot
		var tags, objects, actions, mood, created string
		if err := rows.Scan(&shot.ID, &shot.AssetID, &shot.SourceRunID, &shot.Ordinal, &shot.StartMS, &shot.EndMS, &shot.Description, &tags, &objects, &actions, &mood, &shot.Confidence, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tags), &shot.Tags)
		_ = json.Unmarshal([]byte(objects), &shot.Objects)
		_ = json.Unmarshal([]byte(actions), &shot.Actions)
		_ = json.Unmarshal([]byte(mood), &shot.Mood)
		shot.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, shot)
	}
	return out, rows.Err()
}

func (r *Repository) ShotExists(ctx context.Context, shotID string) (bool, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_shots WHERE id=?`, shotID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// SearchShots is the unfiltered entry point kept for existing callers.
// Behaviourally identical to SearchShotsFiltered with a zero-value
// domain.FacetFilter.
func (r *Repository) SearchShots(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
	return r.searchShots(ctx, q, limit, domain.FacetFilter{})
}

// SearchShotsFiltered narrows SearchShots by the controlled vocabulary and a
// duration range. See the FacetFilter doc comment (internal/domain/asset_browse.go)
// for why the vocabulary fields are resolved through the shot's asset.
func (r *Repository) SearchShotsFiltered(ctx context.Context, q string, limit int, facets domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	return r.searchShots(ctx, q, limit, facets)
}

func (r *Repository) searchShots(ctx context.Context, q string, limit int, facets domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	if limit <= 0 || strings.TrimSpace(q) == "" {
		return []domain.ShotSearchResult{}, nil
	}
	ftsQuery := buildFTSQuery(q)
	if ftsQuery == "" {
		return []domain.ShotSearchResult{}, nil
	}
	clauses, args := facetWhere(facets)
	needsAnalysisJoin := len(clauses) > 0
	clauses, args = appendShotDurationBounds(clauses, args, facets)
	query := `SELECT s.id,s.asset_id,COALESCE(s.source_run_id,''),s.ordinal,s.start_ms,s.end_ms,s.description,s.tags_json,s.objects_json,s.actions_json,s.mood_json,s.confidence,s.created_at,bm25(asset_shot_search) FROM asset_shot_search JOIN asset_shots s ON s.id=asset_shot_search.shot_id`
	if needsAnalysisJoin {
		query += ` LEFT JOIN asset_analysis an ON an.asset_id=s.asset_id`
	}
	query += ` WHERE asset_shot_search MATCH ?`
	queryArgs := append([]any{ftsQuery}, args...)
	if len(clauses) > 0 {
		query += ` AND ` + strings.Join(clauses, ` AND `)
	}
	query += ` ORDER BY bm25(asset_shot_search),s.ordinal LIMIT ?`
	queryArgs = append(queryArgs, limit)
	rows, err := r.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ShotSearchResult
	for rows.Next() {
		var result domain.ShotSearchResult
		var tags, objects, actions, mood, created string
		var rank float64
		if err := rows.Scan(&result.ID, &result.AssetID, &result.SourceRunID, &result.Ordinal, &result.StartMS, &result.EndMS, &result.Description, &tags, &objects, &actions, &mood, &result.Confidence, &created, &rank); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tags), &result.Tags)
		_ = json.Unmarshal([]byte(objects), &result.Objects)
		_ = json.Unmarshal([]byte(actions), &result.Actions)
		_ = json.Unmarshal([]byte(mood), &result.Mood)
		result.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if rank < 0 {
			result.Score = -rank
		}
		out = append(out, result)
	}
	return out, rows.Err()
}

// maxTopShotPrealloc bounds how much the heap reserves up front. limit reaches
// these queries straight from a ?limit= query parameter, and parseInt only
// rejects negatives — so sizing the initial allocation by limit alone would let
// a read-scoped caller ask for two billion results and have the Hub try to
// reserve the memory before a single row is read. Capacity is only a hint: the
// heap still grows to whatever limit genuinely requires, so this costs nothing
// for real queries and removes the amplification for absurd ones.
const maxTopShotPrealloc = 1024

// topShotHeap is a bounded min-heap of scored shots for similar/hybrid search.
// Scoring happens in Go after the rows are read, so a SQL LIMIT cannot shrink
// the work; the heap keeps at most limit members instead of sorting the whole
// library. Less is a TOTAL order — score descending, then shot ID ascending —
// because a heap is not stable: without explicit tie-breaking, equal scores
// would surface in whatever order the heap happened to hold them, and repeated
// identical queries would return visibly different orders. The heap is a
// min-heap on "worse", so Pop always evicts the weakest member.
type topShotHeap []domain.ShotSearchResult

func (h topShotHeap) Len() int { return len(h) }
func (h topShotHeap) Less(i, j int) bool {
	if h[i].Score == h[j].Score {
		return h[i].ID > h[j].ID
	}
	return h[i].Score < h[j].Score
}
func (h topShotHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *topShotHeap) Push(x any)   { *h = append(*h, x.(domain.ShotSearchResult)) }
func (h *topShotHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = domain.ShotSearchResult{}
	*h = old[:n-1]
	return item
}

// HybridSearchShots is the unfiltered entry point kept for existing callers
// (including the repurpose-plan alternatives lookup in service.go).
// Behaviourally identical to HybridSearchShotsFiltered with a zero-value
// domain.FacetFilter.
func (r *Repository) HybridSearchShots(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
	return r.hybridSearchShots(ctx, q, limit, domain.FacetFilter{}, domain.DefaultHybridSearchWeights())
}

// HybridSearchShotsFiltered narrows HybridSearchShots by the controlled
// vocabulary and a duration range.
func (r *Repository) HybridSearchShotsFiltered(ctx context.Context, q string, limit int, facets domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	return r.hybridSearchShots(ctx, q, limit, facets, domain.DefaultHybridSearchWeights())
}

// HybridSearchShotsWithWeights is HybridSearchShots with an explicit blend.
// The product default is DefaultHybridSearchWeights; the explicit form exists
// for measurement: the retrieval golden set sweeps blends, and the eval
// harness attributes false positives to one signal by re-ranking with pure
// (1,0) / (0,1) weights. Production callers should keep using the default.
func (r *Repository) HybridSearchShotsWithWeights(ctx context.Context, q string, limit int, weights domain.HybridSearchWeights) ([]domain.ShotSearchResult, error) {
	return r.hybridSearchShots(ctx, q, limit, domain.FacetFilter{}, weights)
}

// bm25HalfScore is the |bm25| that earns exactly half of the lexical term
// below. bm25 has no upper bound, so any mapping onto 0-1 has to nominate
// some magnitude as "half marks"; leaving that implicit is how the previous
// conversion ended up anchored on a bare 1 that meant nothing. This one is
// read off FTS5's own arithmetic: a single query term contributes at most
// idf*(k1+1) to the rank, with k1 = 1.2 fixed inside SQLite, so a term that
// is only just discriminating (idf = 1, i.e. present in roughly a quarter of
// the corpus) and repeated often enough to saturate bm25's term-frequency
// term tops out near 2.2. Pinning half marks there reserves the upper half
// of the scale for what should actually outrank it — rarer terms, or more
// than one query term matching the same shot.
const bm25HalfScore = 2.2

// lexicalScoreFromBM25 converts a raw bm25() rank into the 0-1 lexical term
// of the hybrid blend below. Direction is the whole point of this function:
// bm25 runs the opposite way from a score — more negative means more
// relevant, which is why searchShots above orders by bm25 ascending — while
// the blend below ADDS this term to the semantic one, so it must come back
// pointing the normal way, larger meaning better matched. |rank| carries the
// strength; the saturating |rank|/(bm25HalfScore+|rank|) puts it on 0-1
// while staying strictly increasing, so a shot that matched a query term
// four times can never be outranked by one that matched it once. Saturation
// rather than a linear clamp is deliberate: bm25 is unbounded above, and the
// gap between "barely matched" and "matched" deserves more of the scale than
// the gap between two already-strong matches.
//
// A rank of 0 or above is not a strong match but the absence of one: it is
// FTS5's IDF floor reporting a term with no discriminating power (a term
// appearing across most of the corpus never quite reaches a positive rank;
// SQLite clamps idf to a small positive constant instead, so the weakest
// real match sits an epsilon below zero). The formula already sends rank 0
// to 0; the guard is what keeps a positive rank from re-entering through
// |rank| and scoring as though it were a match.
func lexicalScoreFromBM25(rank float64) float64 {
	if rank >= 0 {
		return 0
	}
	strength := -rank
	return strength / (bm25HalfScore + strength)
}

// hybridSearchShots blends local lexical FTS with deterministic semantic
// features generated from the model's already-persisted visual observations.
// It stays SQLite-first and returns source shot time ranges. The weights
// parameter exists so the retrieval golden set can sweep the blend and the
// measured result can set the default — the semantic side is a heuristic
// feature hash, not a learned embedding, so its share is a measurement
// decision, not a design constant.
func (r *Repository) hybridSearchShots(ctx context.Context, q string, limit int, facets domain.FacetFilter, weights domain.HybridSearchWeights) ([]domain.ShotSearchResult, error) {
	if limit <= 0 || strings.TrimSpace(q) == "" {
		return []domain.ShotSearchResult{}, nil
	}
	candidates, err := r.scoreShotCandidates(ctx, q, facets)
	if err != nil {
		return nil, err
	}
	top := make(topShotHeap, 0, min(limit, maxTopShotPrealloc))
	for _, result := range candidates {
		result.Score = weights.Semantic*result.SemanticScore + weights.Lexical*result.LexicalScore
		if result.Score > 0 {
			heap.Push(&top, result)
			if top.Len() > limit {
				heap.Pop(&top)
			}
		}
	}
	results := make([]domain.ShotSearchResult, 0, top.Len())
	for top.Len() > 0 {
		results = append(results, heap.Pop(&top).(domain.ShotSearchResult))
	}
	// Draining pops the weakest member first; flip for score-descending order.
	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
	}
	return results, nil
}

// scoreShotCandidates scores every candidate shot under the current vector
// scheme against the query, on both signals, without blending or truncating.
// hybridSearchShots fuses the two scores into one rank; the RRF fusion needs
// each signal ranked separately; both must see the same candidate universe,
// so the scoring lives here once instead of drifting apart.
//
// The lexical pass scores by shot_id without a facet filter — it is only a
// lookup table for the score blend below, so an id absent from it simply
// contributes a zero lexical score. Facets are applied once, here, to the
// candidate set that actually becomes the result.
func (r *Repository) scoreShotCandidates(ctx context.Context, q string, facets domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	lexical := make(map[string]float64)
	if ftsQuery := buildFTSQuery(q); ftsQuery != "" {
		rows, err := r.db.QueryContext(ctx, `SELECT shot_id,bm25(asset_shot_search) FROM asset_shot_search WHERE asset_shot_search MATCH ?`, ftsQuery)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var rank float64
			if err := rows.Scan(&id, &rank); err != nil {
				rows.Close()
				return nil, err
			}
			lexical[id] = lexicalScoreFromBM25(rank)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	queryVector := discovery.VectorForText(q)
	// The heuristic vector hashes tokens into 64 dimensions, so two texts
	// sharing no token can still collide into a nonzero cosine. That
	// collision is not evidence: a shot whose text shares nothing with the
	// query scores zero on the semantic side, or the blend would rank noise
	// above honest lexical matches — the "semantic false-positive assertion"
	// the retrieval golden set exists to catch (an unrelated shot surfacing
	// for a query it has no evidence for). Alias-canonical tokens count as
	// shared: that is the cross-language bridge, not a collision.
	queryTokens := discovery.TokensForText(q)
	queryTokenSet := make(map[string]struct{}, len(queryTokens))
	for _, token := range queryTokens {
		queryTokenSet[token] = struct{}{}
	}
	// v.model=? is mandatory, not one more optional facet clause: a vector
	// written under a superseded embedding scheme must never be scored
	// against a query vector from the current one (see semanticVector below
	// and the discovery.HeuristicVectorModel doc comment). Facets narrow further, but
	// never replace this filter.
	clauses, args := facetWhere(facets)
	clauses, args = appendShotDurationBounds(clauses, args, facets)
	query := `SELECT s.id,s.asset_id,COALESCE(s.source_run_id,''),s.ordinal,s.start_ms,s.end_ms,s.description,s.tags_json,s.objects_json,s.actions_json,s.mood_json,s.confidence,s.created_at,v.vector_json,COALESCE((SELECT relative_path FROM asset_locations WHERE asset_id=s.asset_id AND is_primary=1 LIMIT 1),'') FROM asset_shots s JOIN shot_semantic_vectors v ON v.shot_id=s.id LEFT JOIN asset_analysis an ON an.asset_id=s.asset_id WHERE v.model=?`
	queryArgs := append([]any{discovery.HeuristicVectorModel}, args...)
	if len(clauses) > 0 {
		query += ` AND ` + strings.Join(clauses, ` AND `)
	}
	rows, err := r.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make([]domain.ShotSearchResult, 0, 64)
	for rows.Next() {
		var result domain.ShotSearchResult
		var tags, objects, actions, mood, created, encodedVector string
		if err := rows.Scan(&result.ID, &result.AssetID, &result.SourceRunID, &result.Ordinal, &result.StartMS, &result.EndMS, &result.Description, &tags, &objects, &actions, &mood, &result.Confidence, &created, &encodedVector, &result.Filename); err != nil {
			return nil, err
		}
		if result.Filename != "" {
			result.Filename = filepath.Base(result.Filename)
		}
		_ = json.Unmarshal([]byte(tags), &result.Tags)
		_ = json.Unmarshal([]byte(objects), &result.Objects)
		_ = json.Unmarshal([]byte(actions), &result.Actions)
		_ = json.Unmarshal([]byte(mood), &result.Mood)
		result.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		vector, err := r.semanticVector(result.ID, encodedVector)
		if err != nil {
			return nil, fmt.Errorf("decode shot semantic vector %s: %w", result.ID, err)
		}
		result.SemanticScore = discovery.Cosine(queryVector, vector)
		if !sharesSemanticToken(queryTokenSet, result.Description, result.Tags, result.Objects, result.Actions, result.Mood) {
			result.SemanticScore = 0
		}
		result.LexicalScore = lexical[result.ID]
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// sharesSemanticToken reports whether the shot's text carries at least one of
// the query's semantic tokens. It mirrors VectorForShot's input exactly
// (description + tags + objects + actions + mood), so "shares a token" and
// "contributes to the same vector" cannot disagree.
func sharesSemanticToken(queryTokenSet map[string]struct{}, description string, tags, objects, actions, mood []string) bool {
	parts := append([]string{description}, tags...)
	parts = append(parts, objects...)
	parts = append(parts, actions...)
	parts = append(parts, mood...)
	for _, token := range discovery.TokensForText(strings.Join(parts, " ")) {
		if _, ok := queryTokenSet[token]; ok {
			return true
		}
	}
	return false
}

// DefaultRRFK is the reciprocal-rank-fusion constant: a shot ranked at
// position p in a signal's list earns 1/(k+p). 60 is the standard choice from
// the original RRF paper (Cormack, Clarke & Buettcher, SIGIR 2009) — a rank
// difference inside the top ranks matters, while contributions beyond the
// hundredth position are too small to flip a top-10.
const DefaultRRFK = 60

// HybridSearchShotsRRF is HybridSearchShots with reciprocal-rank fusion
// instead of the weighted sum: each signal ranks the candidates separately,
// and a shot's final score is the sum of 1/(k+rank) over the signals that
// ranked it. It exists because the weighted blend lets one signal's noise
// ride on the other's strong score; RRF cannot — a shot only scores where a
// signal actually ranked it. k defaults to DefaultRRFK when k <= 0.
func (r *Repository) HybridSearchShotsRRF(ctx context.Context, q string, limit, k int) ([]domain.ShotSearchResult, error) {
	return r.hybridSearchShotsRRF(ctx, q, limit, domain.FacetFilter{}, k)
}

// hybridSearchShotsRRF is the facet-aware form of HybridSearchShotsRRF.
func (r *Repository) hybridSearchShotsRRF(ctx context.Context, q string, limit int, facets domain.FacetFilter, k int) ([]domain.ShotSearchResult, error) {
	if limit <= 0 || strings.TrimSpace(q) == "" {
		return []domain.ShotSearchResult{}, nil
	}
	if k <= 0 {
		k = DefaultRRFK
	}
	candidates, err := r.scoreShotCandidates(ctx, q, facets)
	if err != nil {
		return nil, err
	}
	// Rank each signal on its own. Only shots with a positive signal score
	// enter that signal's list — the same "no evidence, no rank" rule the
	// weighted blend applies via Score > 0.
	semanticRank := make(map[string]int, len(candidates))
	lexicalRank := make(map[string]int, len(candidates))
	byLexical := make([]domain.ShotSearchResult, 0, len(candidates))
	bySemantic := make([]domain.ShotSearchResult, 0, len(candidates))
	for _, c := range candidates {
		if c.LexicalScore > 0 {
			byLexical = append(byLexical, c)
		}
		if c.SemanticScore > 0 {
			bySemantic = append(bySemantic, c)
		}
	}
	sort.SliceStable(byLexical, func(i, j int) bool { return byLexical[i].LexicalScore > byLexical[j].LexicalScore })
	sort.SliceStable(bySemantic, func(i, j int) bool { return bySemantic[i].SemanticScore > bySemantic[j].SemanticScore })
	for i, c := range byLexical {
		lexicalRank[c.ID] = i + 1
	}
	for i, c := range bySemantic {
		semanticRank[c.ID] = i + 1
	}
	top := make(topShotHeap, 0, min(limit, maxTopShotPrealloc))
	for _, c := range candidates {
		var fused float64
		if p, ok := lexicalRank[c.ID]; ok {
			fused += 1 / (float64(k) + float64(p))
		}
		if p, ok := semanticRank[c.ID]; ok {
			fused += 1 / (float64(k) + float64(p))
		}
		if fused == 0 {
			continue
		}
		c.Score = fused
		heap.Push(&top, c)
		if top.Len() > limit {
			heap.Pop(&top)
		}
	}
	results := make([]domain.ShotSearchResult, 0, top.Len())
	for top.Len() > 0 {
		results = append(results, heap.Pop(&top).(domain.ShotSearchResult))
	}
	// Draining pops the weakest member first; flip for score-descending order.
	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
	}
	return results, nil
}

// SimilarShots is the unfiltered entry point kept for existing callers.
// Behaviourally identical to SimilarShotsFiltered with a zero-value
// domain.FacetFilter.
func (r *Repository) SimilarShots(ctx context.Context, shotID string, limit int) ([]domain.ShotSearchResult, error) {
	return r.similarShots(ctx, shotID, limit, domain.FacetFilter{})
}

// SimilarShotsFiltered narrows SimilarShots by the controlled vocabulary and
// a duration range. The source shot itself is looked up unfiltered — a facet
// only prunes the candidates it is compared against, not the reference point.
func (r *Repository) SimilarShotsFiltered(ctx context.Context, shotID string, limit int, facets domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	return r.similarShots(ctx, shotID, limit, facets)
}

func (r *Repository) similarShots(ctx context.Context, shotID string, limit int, facets domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	if strings.TrimSpace(shotID) == "" || limit <= 0 {
		return []domain.ShotSearchResult{}, nil
	}
	var encoded string
	err := r.db.QueryRowContext(ctx, `SELECT vector_json FROM shot_semantic_vectors WHERE shot_id=? AND model=?`, shotID, discovery.HeuristicVectorModel).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		// Now that the lookup is scoped to the current scheme, no-rows covers
		// two different situations, and the message must not claim the first
		// when it is the second: the shot may genuinely not exist, or it may
		// exist with a vector from a superseded scheme and simply need
		// re-analysis. Saying "not found" about a shot the operator can see in
		// the library would send them looking for the wrong problem.
		return nil, fmt.Errorf("%w: %s is either unknown or was embedded under a scheme older than %s and needs re-analysis", domain.ErrShotVectorNotFound, shotID, discovery.HeuristicVectorModel)
	}
	if err != nil {
		return nil, err
	}
	queryVector, err := r.semanticVector(shotID, encoded)
	if err != nil {
		return nil, fmt.Errorf("decode source shot vector: %w", err)
	}
	records, err := r.loadSemanticShotRecords(ctx, facets)
	if err != nil {
		return nil, err
	}
	top := make(topShotHeap, 0, min(limit, maxTopShotPrealloc))
	for _, record := range records {
		if record.result.ID == shotID {
			continue
		}
		record.result.SemanticScore = discovery.Cosine(queryVector, record.vector)
		record.result.Score = record.result.SemanticScore
		if record.result.Score > 0 {
			heap.Push(&top, record.result)
			if top.Len() > limit {
				heap.Pop(&top)
			}
		}
	}
	results := make([]domain.ShotSearchResult, 0, top.Len())
	for top.Len() > 0 {
		results = append(results, heap.Pop(&top).(domain.ShotSearchResult))
	}
	// Draining pops the weakest member first; flip for score-descending order.
	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
	}
	return results, nil
}

func (r *Repository) DiscoverRareShots(ctx context.Context, limit int) ([]domain.RareShot, error) {
	if limit <= 0 {
		return []domain.RareShot{}, nil
	}
	// This one legitimately reads the whole corpus: rarity is defined by token
	// frequency computed across every shot, so top-k cannot be applied without
	// changing the answer. Unlike SimilarShots/HybridSearchShots, leave this
	// uncapped. It's also not one of the faceted endpoints in this brief, so it
	// always loads the unfiltered set — a zero-value domain.FacetFilter adds no
	// WHERE clause beyond loadSemanticShotRecords' own mandatory v.model=? scope
	// filter.
	records, err := r.loadSemanticShotRecords(ctx, domain.FacetFilter{})
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return []domain.RareShot{}, nil
	}
	frequency := make(map[string]int)
	tokensByShot := make(map[string][]string, len(records))
	for _, record := range records {
		tokens := discovery.TokensForShot(record.result.AssetShot)
		tokensByShot[record.result.ID] = tokens
		for _, token := range tokens {
			frequency[token]++
		}
	}
	rare := make([]domain.RareShot, 0, len(records))
	for _, record := range records {
		tokens := tokensByShot[record.result.ID]
		if len(tokens) == 0 {
			continue
		}
		var score float64
		uncommon := make([]string, 0, len(tokens))
		for _, token := range tokens {
			share := float64(frequency[token]) / float64(len(records))
			score += 1 - share
			if frequency[token] <= 1 {
				uncommon = append(uncommon, token)
			}
		}
		score /= float64(len(tokens))
		if len(uncommon) == 0 {
			uncommon = append(uncommon, tokens[0])
		}
		record.result.Score = score
		rare = append(rare, domain.RareShot{ShotSearchResult: record.result, RarityScore: score, Reason: "库内少见语义：" + strings.Join(uncommon, "、")})
	}
	sort.SliceStable(rare, func(i, j int) bool {
		if rare[i].RarityScore == rare[j].RarityScore {
			return rare[i].ID < rare[j].ID
		}
		return rare[i].RarityScore > rare[j].RarityScore
	})
	if len(rare) > limit {
		rare = rare[:limit]
	}
	return rare, nil
}

type semanticShotRecord struct {
	result domain.ShotSearchResult
	vector []float64
}

// loadSemanticShotRecords always scopes to discovery.HeuristicVectorModel — see the
// mandatory v.model=? filter in hybridSearchShots above for why a vector from
// a superseded embedding scheme must never reach a caller that scores it
// against a current-scheme query vector. facets narrows the candidate set
// further; a zero-value domain.FacetFilter leaves only the model filter.
func (r *Repository) loadSemanticShotRecords(ctx context.Context, facets domain.FacetFilter) ([]semanticShotRecord, error) {
	clauses, args := facetWhere(facets)
	clauses, args = appendShotDurationBounds(clauses, args, facets)
	query := `SELECT s.id,s.asset_id,COALESCE(s.source_run_id,''),s.ordinal,s.start_ms,s.end_ms,s.description,s.tags_json,s.objects_json,s.actions_json,s.mood_json,s.confidence,s.created_at,v.vector_json,COALESCE((SELECT relative_path FROM asset_locations WHERE asset_id=s.asset_id AND is_primary=1 LIMIT 1),'') FROM asset_shots s JOIN shot_semantic_vectors v ON v.shot_id=s.id LEFT JOIN asset_analysis an ON an.asset_id=s.asset_id WHERE v.model=?`
	queryArgs := append([]any{discovery.HeuristicVectorModel}, args...)
	if len(clauses) > 0 {
		query += ` AND ` + strings.Join(clauses, ` AND `)
	}
	rows, err := r.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]semanticShotRecord, 0)
	for rows.Next() {
		var record semanticShotRecord
		var tags, objects, actions, mood, created, encodedVector string
		if err := rows.Scan(&record.result.ID, &record.result.AssetID, &record.result.SourceRunID, &record.result.Ordinal, &record.result.StartMS, &record.result.EndMS, &record.result.Description, &tags, &objects, &actions, &mood, &record.result.Confidence, &created, &encodedVector, &record.result.Filename); err != nil {
			return nil, err
		}
		if record.result.Filename != "" {
			record.result.Filename = filepath.Base(record.result.Filename)
		}
		_ = json.Unmarshal([]byte(tags), &record.result.Tags)
		_ = json.Unmarshal([]byte(objects), &record.result.Objects)
		_ = json.Unmarshal([]byte(actions), &record.result.Actions)
		_ = json.Unmarshal([]byte(mood), &record.result.Mood)
		record.result.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		vector, err := r.semanticVector(record.result.ID, encodedVector)
		if err != nil {
			return nil, fmt.Errorf("decode shot semantic vector %s: %w", record.result.ID, err)
		}
		record.vector = vector
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r *Repository) semanticVector(shotID, encoded string) ([]float64, error) {
	r.semanticVectorMu.RLock()
	vector, ok := r.semanticVectorCache[shotID]
	r.semanticVectorMu.RUnlock()
	if ok {
		return vector, nil
	}
	if err := json.Unmarshal([]byte(encoded), &vector); err != nil {
		return nil, err
	}
	if len(vector) != discovery.VectorSize {
		return nil, fmt.Errorf("shot %s vector has %d dimensions, want %d", shotID, len(vector), discovery.VectorSize)
	}
	r.semanticVectorMu.Lock()
	if existing, exists := r.semanticVectorCache[shotID]; exists {
		vector = existing
	} else {
		r.semanticVectorCache[shotID] = vector
	}
	r.semanticVectorMu.Unlock()
	return vector, nil
}

func (r *Repository) clearSemanticVectorCache() {
	r.semanticVectorMu.Lock()
	r.semanticVectorCache = make(map[string][]float64)
	r.semanticVectorMu.Unlock()
}

func (r *Repository) semanticVectorCacheLen() int {
	r.semanticVectorMu.RLock()
	defer r.semanticVectorMu.RUnlock()
	return len(r.semanticVectorCache)
}

// Search is the unfiltered entry point kept for existing callers.
// Behaviourally identical to SearchFiltered with a zero domain.FacetFilter,
// and — because facetExistsGuard builds no guard for a zero filter — emits
// byte-identical SQL to the pre-facet version of this function as well; see
// TestSearchFilteredZeroFacetProducesTodaysSQL.
func (r *Repository) Search(ctx context.Context, q string, limit int) ([]string, error) {
	return r.SearchFiltered(ctx, q, limit, domain.FacetFilter{})
}

// searchSQLTrace, when non-nil, is called with the exact SQL text
// SearchFiltered sends to SQLite, once per query it issues, in order. It is
// nil in production and costs nothing there; it exists only so a test can
// capture the literal query text and diff it against a golden pre-facet
// baseline, instead of a human eyeballing the two versions of this function
// and asserting they match.
var searchSQLTrace func(query string)

// SearchFiltered narrows Search by domain.FacetFilter — the same six-field
// controlled vocabulary and duration bounds as the shot-search *Filtered
// methods (facetWhere / assetFacetWhereClauses in asset_browse.go), applied
// at asset granularity: MinDurationMS/MaxDurationMS bound the asset's own
// duration, not a shot's span (see the FacetFilter doc comment in
// internal/domain/asset_browse.go).
//
// The facet predicate is folded into each query below as a correlated EXISTS
// guard (facetExistsGuard), not applied as a pass over already-fetched
// results: LIMIT ? must count facet-matching rows, or a caller asking for
// `limit` ids from a large library would silently get "the facet-matching
// subset of the first `limit` hits" — fewer than `limit`, and wrong in
// exactly the case (a large library) where the limit is doing real work.
// facetExistsGuard returns "" for a zero FacetFilter, so every query text
// below is unchanged from before facets existed, which keeps the query plans
// the "Resolve the literal tag..." comment further down was written to
// protect.
func (r *Repository) SearchFiltered(ctx context.Context, q string, limit int, facets domain.FacetFilter) ([]string, error) {
	if limit <= 0 || strings.TrimSpace(q) == "" {
		return []string{}, nil
	}
	ids := make([]string, 0, limit)
	seen := map[string]bool{}
	appendID := func(id string) {
		if id != "" && !seen[id] && len(ids) < limit {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	// tagGuard is folded into each of the three UNION branches below (all
	// scoped to alias "l" from asset_tag_links), so its args must be repeated
	// once per branch, same as the tag placeholder itself.
	tagGuard, tagGuardArgs := facetExistsGuard("l.asset_id", facets)

	// Resolve the literal tag, its canonical form and every sibling alias to
	// the same canonical ID. This keeps an approved "城市夜景 → urban_night"
	// mapping searchable without rewriting model summaries or raw tags.
	//
	// This is written as a UNION of three single-predicate SELECTs rather
	// than one query with `l.normalized_tag=? OR t.canonical_name=? OR
	// a.alias_normalized=?`: with a three-way OR spanning columns from
	// different LEFT-JOINed tables, SQLite cannot push any single disjunct
	// down as an index seek on the driving table, so it falls back to
	// scanning every asset_tag_links row regardless of which index exists.
	// Each UNION branch is instead a single-table (or single-join) predicate
	// that idx_asset_tag_links_normalized (and the existing canonical/alias
	// indexes) can seek directly, and UNION (not UNION ALL) preserves the
	// original query's implicit de-duplication across branches.
	for _, tag := range searchTagCandidates(q) {
		query := `SELECT asset_id FROM (
SELECT l.asset_id FROM asset_tag_links l WHERE l.normalized_tag=?` + tagGuard + `
UNION
SELECT l.asset_id FROM asset_tag_links l JOIN tag_catalog t ON t.id=l.canonical_tag_id WHERE t.canonical_name=?` + tagGuard + `
UNION
SELECT l.asset_id FROM asset_tag_links l JOIN tag_aliases_v2 a ON a.canonical_tag_id=l.canonical_tag_id WHERE a.alias_normalized=?` + tagGuard + `
)
LIMIT ?`
		args := make([]any, 0, 3*(1+len(tagGuardArgs))+1)
		for i := 0; i < 3; i++ {
			args = append(args, tag)
			args = append(args, tagGuardArgs...)
		}
		args = append(args, limit-len(ids))
		if searchSQLTrace != nil {
			searchSQLTrace(query)
		}
		rows, err := r.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			appendID(id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		if len(ids) >= limit {
			return ids, nil
		}
	}
	ftsQuery := buildFTSQuery(q)
	if ftsQuery == "" {
		return ids, nil
	}
	ftsGuard, ftsGuardArgs := facetExistsGuard("asset_search.asset_id", facets)
	query := `SELECT asset_id FROM asset_search WHERE asset_search MATCH ?` + ftsGuard + ` ORDER BY bm25(asset_search) LIMIT ?`
	args := append([]any{ftsQuery}, ftsGuardArgs...)
	args = append(args, limit-len(ids))
	if searchSQLTrace != nil {
		searchSQLTrace(query)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		appendID(id)
	}
	return ids, rows.Err()
}

// facetExistsGuard returns a SQL "AND EXISTS (...)" fragment (and its args)
// that narrows a query producing asset ids under idColumn to the ids whose
// asset matches facets, or "" (with nil args) when facets is the zero value.
// idColumn is the column expression for the id being tested in the enclosing
// query — "l.asset_id" inside SearchFiltered's tag UNION branches,
// "asset_search.asset_id" in its FTS fallback. The guard is a correlated
// subquery rather than a join so it drops into either query shape without
// restructuring it, and building it only when facets is non-empty is what
// keeps SearchFiltered's SQL byte-identical to Search's for a zero
// FacetFilter.
func facetExistsGuard(idColumn string, facets domain.FacetFilter) (string, []any) {
	clauses, args := assetFacetWhereClauses(facets)
	if len(clauses) == 0 {
		return "", nil
	}
	return ` AND EXISTS (SELECT 1 FROM assets fa LEFT JOIN asset_analysis an ON an.asset_id=fa.id LEFT JOIN media_metadata m ON m.asset_id=fa.id WHERE fa.id=` + idColumn + ` AND ` + strings.Join(clauses, ` AND `) + `)`, args
}

func searchTagCandidates(q string) []string {
	raw := append([]string{q}, strings.Fields(q)...)
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		normalized := normalizeTagValue(value)
		if normalized != "" && !seen[normalized] {
			seen[normalized] = true
			out = append(out, normalized)
		}
	}
	return out
}

func buildFTSQuery(q string) string {
	return textindex.FTSQuery(q)
}

func indexText(value string) string { return textindex.Segment(value) }

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func nullableTime(v *time.Time) any {
	if v == nil {
		return nil
	}
	return formatTime(*v)
}
func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// DB exposes the underlying handle for administrative reads and tests. It is
// not part of the domain contract: callers that need data should use the
// Repository methods, which own the SQL.
func (r *Repository) DB() *sql.DB { return r.db }

func leaseParams(args []string) (string, string) {
	if len(args) >= 2 {
		return args[len(args)-2], args[len(args)-1]
	}
	return "", ""
}

func leaseGuard(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, jobID, assetID, owner string) error {
	if jobID == "" && owner == "" {
		return nil
	}
	now := formatTime(time.Now().UTC())
	res, err := exec.ExecContext(ctx, `UPDATE jobs SET updated_at=updated_at WHERE id=? AND asset_id=? AND state='running' AND lease_owner=? AND lease_expires_at>?`, jobID, assetID, owner, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return leaseLostErr(jobID, owner)
	}
	return nil
}

func (r *Repository) CreateModelRun(ctx context.Context, assetID, capability, provider, model, inputHash, promptVersion, schemaVersion, requestJSON string, lease ...string) (string, bool, error) {
	jobID, owner := leaseParams(lease)
	if jobID != "" || owner != "" {
		if err := leaseGuard(ctx, r.db, jobID, assetID, owner); err != nil {
			return "", false, err
		}
	}
	var existingID, state string
	err := r.db.QueryRowContext(ctx, `SELECT id,state FROM model_runs WHERE capability=? AND provider=? AND model=? AND input_hash=? AND prompt_version=? AND schema_version=? AND state != 'failed'`, capability, provider, model, inputHash, promptVersion, schemaVersion).Scan(&existingID, &state)
	if err == nil {
		return existingID, state == "committed", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	id := idgen.New()
	_, err = r.db.ExecContext(ctx, `INSERT INTO model_runs(id,asset_id,capability,provider,model,input_hash,prompt_version,schema_version,state,request_json,started_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, assetID, capability, provider, model, inputHash, promptVersion, schemaVersion, "running", requestJSON, formatTime(time.Now()))
	return id, false, err
}

func (r *Repository) FailModelRun(ctx context.Context, runID, code, message, raw string, lease ...string) error {
	jobID, owner := leaseParams(lease)
	assetID := ""
	if err := r.db.QueryRowContext(ctx, `SELECT asset_id FROM model_runs WHERE id=?`, runID).Scan(&assetID); err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx, `UPDATE model_runs SET state='failed',raw_response=?,error_code=?,error_message=?,finished_at=? WHERE id=? AND (?='' OR EXISTS (SELECT 1 FROM jobs WHERE id=? AND asset_id=? AND state='running' AND lease_owner=? AND lease_expires_at>?))`, raw, code, message, formatTime(time.Now()), runID, jobID, jobID, assetID, owner, formatTime(time.Now().UTC()))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 && jobID != "" {
		return leaseLostErr(jobID, owner)
	}
	if n != 1 {
		return domain.Permanent(fmt.Errorf("%w: run %s", domain.ErrModelRunNotRunning, runID))
	}
	return nil
}

// MarkModelRunCommitted advances a validated run to committed without writing
// an asset_analysis row. CommitAnalysisWithShots does both; the multiframe
// refinement run replaces only the shot rows (ReplaceAssetShots) while the
// asset-level analysis stays attributed to the run that produced it, so its
// run record needs the same state transition on its own.
func (r *Repository) MarkModelRunCommitted(ctx context.Context, runID string, lease ...string) error {
	jobID, owner := leaseParams(lease)
	assetID := ""
	if err := r.db.QueryRowContext(ctx, `SELECT asset_id FROM model_runs WHERE id=?`, runID).Scan(&assetID); err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx, `UPDATE model_runs SET state='committed',committed_at=? WHERE id=? AND state='validated' AND (?='' OR EXISTS (SELECT 1 FROM jobs WHERE id=? AND asset_id=? AND state='running' AND lease_owner=? AND lease_expires_at>?))`, formatTime(time.Now()), runID, jobID, jobID, assetID, owner, formatTime(time.Now().UTC()))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 && jobID != "" {
		return leaseLostErr(jobID, owner)
	}
	if n != 1 {
		return domain.Permanent(fmt.Errorf("%w: run %s", domain.ErrCommitRunNotValidated, runID))
	}
	return nil
}

func (r *Repository) StageModelRun(ctx context.Context, runID, raw, parsed string, lease ...string) error {
	jobID, owner := leaseParams(lease)
	assetID := ""
	if err := r.db.QueryRowContext(ctx, `SELECT asset_id FROM model_runs WHERE id=?`, runID).Scan(&assetID); err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx, `UPDATE model_runs SET state='validated',raw_response=?,parsed_json=?,validation_errors=NULL,finished_at=? WHERE id=? AND (?='' OR EXISTS (SELECT 1 FROM jobs WHERE id=? AND asset_id=? AND state='running' AND lease_owner=? AND lease_expires_at>?))`, raw, parsed, formatTime(time.Now()), runID, jobID, jobID, assetID, owner, formatTime(time.Now().UTC()))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 && jobID != "" {
		return leaseLostErr(jobID, owner)
	}
	if n != 1 {
		return domain.Permanent(fmt.Errorf("%w: run %s", domain.ErrModelRunNotRunning, runID))
	}
	return nil
}

func (r *Repository) CommitAnalysis(ctx context.Context, assetID, runID, schemaVersion string, a domain.StructuredAnalysis, lease ...string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := r.commitAnalysisTx(ctx, tx, assetID, runID, schemaVersion, a); err != nil {
		return err
	}
	jobID, owner := leaseParams(lease)
	now := formatTime(time.Now().UTC())
	res, err := tx.ExecContext(ctx, `UPDATE model_runs SET state='committed',committed_at=? WHERE id=? AND state='validated' AND (?='' OR EXISTS (SELECT 1 FROM jobs WHERE id=? AND asset_id=? AND state='running' AND lease_owner=? AND lease_expires_at>?))`, now, runID, jobID, jobID, assetID, owner, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		if jobID != "" {
			return leaseLostErr(jobID, owner)
		}
		return domain.Permanent(fmt.Errorf("%w: run %s state is not validated", domain.ErrCommitRunNotValidated, runID))
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.clearSemanticVectorCache()
	return nil
}

// CommitAnalysisWithShots makes a validated model result visible as one unit:
// whole-asset analysis, AI tag links, time-bounded shots, FTS, and local
// discovery vectors either all change or all keep their prior trusted state.
func (r *Repository) CommitAnalysisWithShots(ctx context.Context, assetID, runID, schemaVersion string, a domain.StructuredAnalysis, shots []domain.AssetShot, lease ...string) error {
	if err := validateAssetShots(shots); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := r.commitAnalysisTx(ctx, tx, assetID, runID, schemaVersion, a); err != nil {
		return err
	}
	if err := r.syncAnalysisTagsTx(ctx, tx, assetID, runID, a); err != nil {
		return err
	}
	if err := r.replaceAssetShotsTx(ctx, tx, assetID, runID, shots); err != nil {
		return err
	}
	jobID, owner := leaseParams(lease)
	if err := leaseGuard(ctx, tx, jobID, assetID, owner); err != nil {
		return err
	}
	now := formatTime(time.Now().UTC())
	res, err := tx.ExecContext(ctx, `UPDATE model_runs SET state='committed',committed_at=? WHERE id=? AND state='validated' AND (?='' OR EXISTS (SELECT 1 FROM jobs WHERE id=? AND asset_id=? AND state='running' AND lease_owner=? AND lease_expires_at>?))`, now, runID, jobID, jobID, assetID, owner, now)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil {
		return e
	} else if n != 1 {
		if jobID != "" {
			return leaseLostErr(jobID, owner)
		}
		return domain.Permanent(fmt.Errorf("%w: run %s state is not validated", domain.ErrCommitRunNotValidated, runID))
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Matches CommitAnalysis. replaceAssetShotsTx mints fresh shot IDs, so stale
	// entries are never read back and this is not a correctness fix — without it
	// the cache simply grows for the life of the process.
	r.clearSemanticVectorCache()
	return nil
}

func (r *Repository) commitAnalysisTx(ctx context.Context, tx *sql.Tx, assetID, runID, schemaVersion string, a domain.StructuredAnalysis) error {
	// Guard: only a validated run can be committed. The UPDATE below has
	// always carried WHERE state='validated' as a passive gate, but the
	// INSERT ran before it — a failed/committed/nonexistent run would
	// still write canonical rows while the UPDATE silently affected 0.
	// This SELECT makes the gate active: it runs first inside the
	// transaction, so a wrong state means the whole tx rolls back with
	// zero effect on canonical tables.
	var guardState string
	err := tx.QueryRowContext(ctx, `SELECT state FROM model_runs WHERE id=?`, runID).Scan(&guardState)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Permanent(fmt.Errorf("%w: run %s not found", domain.ErrCommitRunNotValidated, runID))
	}
	if err != nil {
		return err
	}
	if guardState != "validated" {
		return domain.Permanent(fmt.Errorf("%w: run %s state is %q, not 'validated'", domain.ErrCommitRunNotValidated, runID, guardState))
	}

	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	_, err = tx.ExecContext(ctx, `INSERT INTO asset_analysis(asset_id,source_run_id,schema_version,asset_type,shot_size,camera_motion,audio_type,lighting,people_count,has_speech,quality,summary,scene_tags_json,subjects_json,mood_tags_json,usable_as_json,quality_flags_json,extra_tags_json,editorial_reason,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(asset_id) DO UPDATE SET source_run_id=excluded.source_run_id,schema_version=excluded.schema_version,asset_type=excluded.asset_type,shot_size=excluded.shot_size,camera_motion=excluded.camera_motion,audio_type=excluded.audio_type,lighting=excluded.lighting,people_count=excluded.people_count,has_speech=excluded.has_speech,quality=excluded.quality,summary=excluded.summary,scene_tags_json=excluded.scene_tags_json,subjects_json=excluded.subjects_json,mood_tags_json=excluded.mood_tags_json,usable_as_json=excluded.usable_as_json,quality_flags_json=excluded.quality_flags_json,extra_tags_json=excluded.extra_tags_json,editorial_reason=excluded.editorial_reason,updated_at=excluded.updated_at`, assetID, runID, schemaVersion, a.AssetType, a.ShotSize, a.CameraMotion, a.AudioType, a.Lighting, a.PeopleCount, boolInt(a.HasSpeech), a.Quality, a.Summary, j(a.SceneTags), j(a.Subjects), j(a.MoodTags), j(a.UsableAs), j(a.QualityFlags), j(a.ExtraTags), a.EditorialReason, formatTime(time.Now()))
	if err != nil {
		return err
	}
	return nil
}

func (r *Repository) GetSpeechClassification(ctx context.Context, assetID string) (*domain.SpeechClassification, error) {
	var c domain.SpeechClassification
	err := r.db.QueryRowContext(ctx, `SELECT classification,speech_probability,reason,raw_json FROM speech_classifications WHERE asset_id=?`, assetID).Scan(&c.Classification, &c.SpeechProbability, &c.Reason, &c.RawJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

func (r *Repository) SaveTranscript(ctx context.Context, assetID, provider, model, inputHash string, t domain.Transcript, lease ...string) error {
	segments, _ := json.Marshal(t.Segments)
	jobID, owner := leaseParams(lease)
	now := formatTime(time.Now().UTC())
	res, err := r.db.ExecContext(ctx, `INSERT INTO transcripts(id,asset_id,provider,model,input_hash,language,full_text,segments_json,raw_response,status,created_at)
SELECT ?,?,?,?,?,?,?,?,?, 'succeeded',? WHERE (?='' OR EXISTS (SELECT 1 FROM jobs WHERE id=? AND asset_id=? AND state='running' AND lease_owner=? AND lease_expires_at>?)) ON CONFLICT(asset_id,input_hash) DO UPDATE SET language=excluded.language,full_text=excluded.full_text,segments_json=excluded.segments_json,raw_response=excluded.raw_response,status='succeeded'`,
		idgen.New(), assetID, provider, model, inputHash, t.Language, t.Text, string(segments), t.RawResponse, now, jobID, jobID, assetID, owner, now)
	if err != nil {
		return err
	}
	if jobID != "" {
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return leaseLostErr(jobID, owner)
		}
	}
	return nil
}

func (r *Repository) GetTranscript(ctx context.Context, assetID string) (*domain.Transcript, error) {
	var t domain.Transcript
	var segments string
	err := r.db.QueryRowContext(ctx, `SELECT language,full_text,segments_json,raw_response FROM transcripts WHERE asset_id=? AND status='succeeded' ORDER BY created_at DESC LIMIT 1`, assetID).Scan(&t.Language, &t.Text, &segments, &t.RawResponse)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(segments), &t.Segments)
	return &t, nil
}

func (r *Repository) GetProviderFile(ctx context.Context, assetID, artifactType, profileHash, provider string) (*domain.ProviderFile, error) {
	var f domain.ProviderFile
	var created, used string
	var expires sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT id,asset_id,artifact_type,profile_hash,provider,COALESCE(remote_name,''),remote_uri,mime_type,state,size_bytes,created_at,last_used_at,expires_at,COALESCE(error_message,'') FROM provider_files WHERE asset_id=? AND artifact_type=? AND profile_hash=? AND provider=?`, assetID, artifactType, profileHash, provider).Scan(&f.ID, &f.AssetID, &f.ArtifactType, &f.ProfileHash, &f.Provider, &f.RemoteName, &f.RemoteURI, &f.MIMEType, &f.State, &f.SizeBytes, &created, &used, &expires, &f.ErrorMessage)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	f.LastUsedAt, _ = time.Parse(time.RFC3339Nano, used)
	if expires.Valid {
		t, _ := time.Parse(time.RFC3339Nano, expires.String)
		f.ExpiresAt = &t
	}
	return &f, nil
}

func (r *Repository) SaveProviderFile(ctx context.Context, f domain.ProviderFile) error {
	now := time.Now().UTC()
	if f.ID == "" {
		f.ID = idgen.New()
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO provider_files(id,asset_id,artifact_type,profile_hash,provider,remote_name,remote_uri,mime_type,state,size_bytes,created_at,last_used_at,expires_at,error_message) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(asset_id,artifact_type,profile_hash,provider) DO UPDATE SET remote_name=excluded.remote_name,remote_uri=excluded.remote_uri,mime_type=excluded.mime_type,state=excluded.state,size_bytes=excluded.size_bytes,last_used_at=excluded.last_used_at,expires_at=excluded.expires_at,error_message=excluded.error_message`, f.ID, f.AssetID, f.ArtifactType, f.ProfileHash, f.Provider, f.RemoteName, f.RemoteURI, f.MIMEType, f.State, f.SizeBytes, formatTime(now), formatTime(now), nullableTime(f.ExpiresAt), nullString(f.ErrorMessage))
	return err
}

func (r *Repository) SaveAlignment(ctx context.Context, assetID, provider, model, inputHash, requestJSON string, result domain.AlignmentResult) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	runID := idgen.New()
	now := formatTime(time.Now().UTC())
	_, err = tx.ExecContext(ctx, `INSERT INTO alignment_runs(id,asset_id,provider,model,input_hash,state,request_json,raw_response,created_at,finished_at) VALUES(?,?,?,?,?,'succeeded',?,?,?,?) ON CONFLICT(asset_id,provider,model,input_hash) DO UPDATE SET state='succeeded',raw_response=excluded.raw_response,finished_at=excluded.finished_at`, runID, assetID, provider, model, inputHash, requestJSON, result.RawResponse, now, now)
	if err != nil {
		return err
	}
	var effectiveRun string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM alignment_runs WHERE asset_id=? AND provider=? AND model=? AND input_hash=?`, assetID, provider, model, inputHash).Scan(&effectiveRun); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM transcript_words WHERE alignment_run_id=?`, effectiveRun); err != nil {
		return err
	}
	for i, w := range result.Words {
		if _, err := tx.ExecContext(ctx, `INSERT INTO transcript_words(alignment_run_id,asset_id,ordinal,start_ms,end_ms,text,confidence) VALUES(?,?,?,?,?,?,?)`, effectiveRun, assetID, i, w.StartMS, w.EndMS, w.Text, w.Confidence); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *Repository) ListAssetCards(ctx context.Context, limit, offset int) ([]domain.AssetCard, error) {
	return r.ListAssetCardsFiltered(ctx, domain.AssetCardFilter{Limit: limit, Offset: offset})
}

func (r *Repository) GetAssetDetail(ctx context.Context, assetID string) (*domain.AssetDetail, error) {
	var d domain.AssetDetail
	var first, last string
	var missing sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT id,quick_fingerprint,full_hash,file_size,state,first_seen_at,last_seen_at,missing_since FROM assets WHERE id=?`, assetID).Scan(&d.Asset.ID, &d.Asset.QuickFingerprint, &d.Asset.FullHash, &d.Asset.FileSize, &d.Asset.State, &first, &last, &missing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339Nano, first); err != nil {
		if first != "" {
			slog.Debug("failed to parse first_seen_at in GetAssetDetail", "asset_id", assetID, "value", first, "error", err)
		}
	} else {
		d.Asset.FirstSeenAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, last); err != nil {
		if last != "" {
			slog.Debug("failed to parse last_seen_at in GetAssetDetail", "asset_id", assetID, "value", last, "error", err)
		}
	} else {
		d.Asset.LastSeenAt = t
	}
	if missing.Valid {
		if t, err := time.Parse(time.RFC3339Nano, missing.String); err != nil {
			slog.Debug("failed to parse missing_since in GetAssetDetail", "asset_id", assetID, "value", missing.String, "error", err)
		} else {
			d.Asset.MissingSince = &t
		}
	}
	if loc, err := r.GetPrimaryLocation(ctx, assetID); err == nil {
		d.Location = &loc
	}
	d.Metadata, _ = r.GetMediaMetadata(ctx, assetID)
	d.Transcript, _ = r.GetTranscript(ctx, assetID)
	// The API and the video model must tell the same story: when a forced
	// alignment exists, its word timeline supersedes the raw ASR transcript
	// (which some providers persist as a single untimed 0-0 placeholder).
	if d.Transcript != nil {
		if words, err := r.GetAlignmentWords(ctx, assetID); err == nil {
			if aligned := domain.TranscriptFromAlignmentWords(words); aligned != nil {
				aligned.Language = d.Transcript.Language
				d.Transcript = aligned
			}
		}
	}
	if a, _ := r.GetArtifact(ctx, assetID, "thumbnail"); a != nil {
		d.ThumbnailPath = a.LocalPath
	}
	if a, _ := r.GetArtifact(ctx, assetID, "proxy"); a != nil {
		d.ProxyPath = a.LocalPath
	}
	var raw string
	if err := r.db.QueryRowContext(ctx, `SELECT json_object('asset_type',asset_type,'scene_tags',json(scene_tags_json),'subjects',json(subjects_json),'people_count',people_count,'shot_size',shot_size,'camera_motion',camera_motion,'lighting',lighting,'audio_type',audio_type,'has_speech',CASE WHEN has_speech=1 THEN json('true') ELSE json('false') END,'summary',summary,'usable_as',json(usable_as_json),'mood_tags',json(mood_tags_json),'quality',quality,'quality_flags',json(quality_flags_json),'extra_tags',json(extra_tags_json),'editorial_reason',editorial_reason) FROM asset_analysis WHERE asset_id=?`, assetID).Scan(&raw); err == nil {
		var a domain.StructuredAnalysis
		if json.Unmarshal([]byte(raw), &a) == nil {
			d.Analysis = &a
		}
	}
	return &d, nil
}

func normalizeTagValue(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	var b strings.Builder
	sep := false
	for _, ch := range v {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch > 127 {
			b.WriteRune(ch)
			sep = false
		} else if !sep {
			b.WriteByte('_')
			sep = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func (r *Repository) SyncAnalysisTags(ctx context.Context, assetID, runID string, a domain.StructuredAnalysis) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := r.syncAnalysisTagsTx(ctx, tx, assetID, runID, a); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) syncAnalysisTagsTx(ctx context.Context, tx *sql.Tx, assetID, runID string, a domain.StructuredAnalysis) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_tag_links WHERE asset_id=? AND source='ai'`, assetID); err != nil {
		return err
	}
	now := formatTime(time.Now())
	groups := map[string][]string{"scene": a.SceneTags, "subject": a.Subjects, "mood": a.MoodTags, "extra": a.ExtraTags, "usable_as": a.UsableAs}
	// Two tags can differ as strings and still be one row here. Upstream
	// de-duplication compares lowercased text, while normalizeTagValue also
	// collapses every run of punctuation and spacing to "_" — so "everyday
	// urban" and "everyday-urban" pass as distinct and then collide on the
	// unique index, failing the whole commit. Model output makes that pairing
	// ordinary rather than rare, and analysing an asset in several windows
	// multiplies the chances of it, since each window phrases the same idea
	// its own way. De-duplicate on the key the index actually uses.
	inserted := make(map[string]struct{}, len(groups)*4)
	for typ, values := range groups {
		for _, raw := range values {
			n := normalizeTagValue(raw)
			if n == "" {
				continue
			}
			key := typ + "\x00" + n
			if _, clash := inserted[key]; clash {
				continue
			}
			inserted[key] = struct{}{}
			var canonical sql.NullString
			_ = tx.QueryRowContext(ctx, `SELECT canonical_tag_id FROM tag_aliases_v2 WHERE alias_normalized=? UNION SELECT id FROM tag_catalog WHERE canonical_name=? LIMIT 1`, n, n).Scan(&canonical)
			_, err := tx.ExecContext(ctx, `INSERT INTO asset_tag_links(asset_id,raw_tag,normalized_tag,canonical_tag_id,tag_type,source,source_run_id,created_at,updated_at) VALUES(?,?,?,?,?,'ai',?,?,?)`, assetID, raw, n, nullableNullString(canonical), typ, runID, now, now)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
func nullableNullString(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}

func (r *Repository) ListCanonicalTags(ctx context.Context) ([]domain.CanonicalTag, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT t.id,t.canonical_name,COALESCE(t.display_name_zh,''),COALESCE(t.display_name_en,''),t.category,COALESCE(t.parent_id,''),t.status,t.updated_at,COUNT(DISTINCT l.asset_id),COUNT(DISTINCT a.alias_normalized) FROM tag_catalog t LEFT JOIN asset_tag_links l ON l.canonical_tag_id=t.id LEFT JOIN tag_aliases_v2 a ON a.canonical_tag_id=t.id GROUP BY t.id ORDER BY COUNT(DISTINCT l.asset_id) DESC,t.canonical_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CanonicalTag
	for rows.Next() {
		var t domain.CanonicalTag
		var updated string
		if err := rows.Scan(&t.ID, &t.CanonicalName, &t.DisplayNameZH, &t.DisplayNameEN, &t.Category, &t.ParentID, &t.Status, &updated, &t.UsageCount, &t.AliasCount); err != nil {
			return nil, err
		}
		t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, t)
	}
	return out, rows.Err()
}
func (r *Repository) ListUnresolvedTags(ctx context.Context, limit int) ([]domain.UnresolvedTag, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT normalized_tag,COUNT(*),COUNT(DISTINCT asset_id),json_group_array(DISTINCT raw_tag) FROM asset_tag_links WHERE canonical_tag_id IS NULL GROUP BY normalized_tag ORDER BY COUNT(*) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.UnresolvedTag
	for rows.Next() {
		var t domain.UnresolvedTag
		var forms string
		if err := rows.Scan(&t.NormalizedTag, &t.UsageCount, &t.AssetCount, &forms); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(forms), &t.DisplayForms)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Repository) UpsertTagEmbeddings(ctx context.Context, model string, tags []domain.UnresolvedTag, vectors [][]float64) error {
	if len(tags) != len(vectors) {
		return fmt.Errorf("embedding tag/vector count mismatch")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := formatTime(time.Now())
	for i, tag := range tags {
		if len(vectors[i]) == 0 {
			return fmt.Errorf("empty embedding for %s", tag.NormalizedTag)
		}
		encoded, err := json.Marshal(vectors[i])
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO tag_embeddings(normalized_tag,model,vector_json,usage_count,asset_count,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(normalized_tag,model) DO UPDATE SET vector_json=excluded.vector_json,usage_count=excluded.usage_count,asset_count=excluded.asset_count,updated_at=excluded.updated_at`, tag.NormalizedTag, model, string(encoded), tag.UsageCount, tag.AssetCount, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *Repository) CreateTagClusterRun(ctx context.Context, provider, model string, threshold float64, clusters []domain.TagCluster, tagsEmbedded int) (string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	runID := idgen.New()
	now := formatTime(time.Now())
	stats, _ := json.Marshal(map[string]int{"tags_embedded": tagsEmbedded, "clusters_found": len(clusters)})
	if _, err = tx.ExecContext(ctx, `INSERT INTO tag_embedding_runs(id,state,provider,model,similarity_threshold,input_revision,stats_json,created_at,finished_at) VALUES(?,'completed',?,?,?,?,?,?,?)`, runID, provider, model, threshold, fmt.Sprintf("%s:%d", model, tagsEmbedded), string(stats), now, now); err != nil {
		return "", err
	}
	for _, cluster := range clusters {
		members, _ := json.Marshal(cluster.Members)
		if _, err = tx.ExecContext(ctx, `INSERT INTO tag_clusters(id,run_id,state,member_tags_json,average_similarity,created_at) VALUES(?,?,?, ?,?,?)`, idgen.New(), runID, "candidate", string(members), cluster.Similarity, now); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return runID, nil
}

func (r *Repository) BuildLibrarySummaryInput(ctx context.Context) (domain.LibrarySummaryInput, error) {
	var input domain.LibrarySummaryInput
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM assets WHERE state != 'missing'`).Scan(&input.AssetCount); err != nil {
		return input, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_analysis`).Scan(&input.AnalyzedCount); err != nil {
		return input, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT t.canonical_name,t.category,COUNT(DISTINCT l.asset_id) FROM tag_catalog t JOIN asset_tag_links l ON l.canonical_tag_id=t.id GROUP BY t.id ORDER BY COUNT(DISTINCT l.asset_id) DESC,t.canonical_name LIMIT 20`)
	if err != nil {
		return input, err
	}
	defer rows.Close()
	for rows.Next() {
		var stat domain.LibraryTagStat
		if err := rows.Scan(&stat.CanonicalName, &stat.Category, &stat.AssetCount); err != nil {
			return input, err
		}
		input.TopTags = append(input.TopTags, stat)
	}
	return input, rows.Err()
}

func (r *Repository) SaveLibrarySummary(ctx context.Context, summary domain.LibrarySummary) (domain.LibrarySummary, error) {
	if summary.ID == "" {
		summary.ID = idgen.New()
	}
	if summary.Scope == "" {
		summary.Scope = "all"
	}
	now := time.Now().UTC()
	summary.GeneratedAt = now
	input, err := json.Marshal(summary.Input)
	if err != nil {
		return domain.LibrarySummary{}, err
	}
	themes, _ := json.Marshal(summary.Themes)
	suitable, _ := json.Marshal(summary.SuitableFor)
	revision := fmt.Sprintf("assets:%d/analyzed:%d/tags:%d", summary.Input.AssetCount, summary.Input.AnalyzedCount, len(summary.Input.TopTags))
	_, err = r.db.ExecContext(ctx, `INSERT INTO library_summaries(id,scope,summary,themes_json,suitable_for_json,input_json,input_revision,provider,model,generated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, summary.ID, summary.Scope, summary.Summary, string(themes), string(suitable), string(input), revision, summary.Provider, summary.Model, formatTime(now))
	if err != nil {
		return domain.LibrarySummary{}, err
	}
	return summary, nil
}

func (r *Repository) LatestLibrarySummary(ctx context.Context) (*domain.LibrarySummary, error) {
	var item domain.LibrarySummary
	var themes, suitable, input, generated string
	err := r.db.QueryRowContext(ctx, `SELECT id,scope,summary,themes_json,suitable_for_json,input_json,provider,model,generated_at FROM library_summaries WHERE scope='all' ORDER BY generated_at DESC LIMIT 1`).Scan(&item.ID, &item.Scope, &item.Summary, &themes, &suitable, &input, &item.Provider, &item.Model, &generated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(themes), &item.Themes)
	_ = json.Unmarshal([]byte(suitable), &item.SuitableFor)
	_ = json.Unmarshal([]byte(input), &item.Input)
	item.GeneratedAt, _ = time.Parse(time.RFC3339Nano, generated)
	return &item, nil
}

// SaveRepurposePlan refuses to overwrite a plan a human already approved.
// This check and the ones in SaveRepurposePlanRevision and
// ApproveRepurposePlanRevision below are where the human-approval boundary is
// actually enforced -- they are the only ones that run against state no
// concurrent caller can change underneath them, so they are also the only
// ones that can be trusted. They wrap domain sentinels rather than returning
// bare prose so the layers above can react to what was refused with errors.Is
// instead of re-deriving it from a second, unlocked read; see
// internal/domain/errors.go.
func (r *Repository) SaveRepurposePlan(ctx context.Context, plan domain.RepurposePlan) (domain.RepurposePlan, error) {
	var currentState string
	err := r.db.QueryRowContext(ctx, `SELECT status FROM repurpose_plans WHERE id=?`, plan.ID).Scan(&currentState)
	if err == nil && currentState == "approved" {
		return domain.RepurposePlan{}, fmt.Errorf("%w: plan %s is approved and accepts no further writes", domain.ErrPlanImmutable, plan.ID)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.RepurposePlan{}, err
	}
	now := time.Now().UTC()
	if plan.ID == "" {
		plan.ID = idgen.New()
	}
	if plan.Status == "" {
		plan.Status = "draft"
	}
	if plan.CreatedAt.IsZero() {
		plan.CreatedAt = now
	}
	plan.UpdatedAt = now
	encoded, err := json.Marshal(plan)
	if err != nil {
		return domain.RepurposePlan{}, err
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO repurpose_plans(id,brief,duration_ms,style,audience,title,status,provider,model,plan_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET brief=excluded.brief,duration_ms=excluded.duration_ms,style=excluded.style,audience=excluded.audience,title=excluded.title,status=excluded.status,provider=excluded.provider,model=excluded.model,plan_json=excluded.plan_json,updated_at=excluded.updated_at`, plan.ID, plan.Brief, plan.DurationMS, plan.Style, plan.Audience, plan.Title, plan.Status, plan.Provider, plan.Model, string(encoded), formatTime(plan.CreatedAt), formatTime(plan.UpdatedAt))
	if err != nil {
		return domain.RepurposePlan{}, err
	}
	return plan, nil
}

func (r *Repository) GetRepurposePlan(ctx context.Context, id string) (*domain.RepurposePlan, error) {
	var planJSON, created, updated string
	err := r.db.QueryRowContext(ctx, `SELECT plan_json,created_at,updated_at FROM repurpose_plans WHERE id=?`, id).Scan(&planJSON, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var plan domain.RepurposePlan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return nil, fmt.Errorf("decode repurpose plan: %w", err)
	}
	plan.ID = id
	plan.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	plan.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &plan, nil
}

func (r *Repository) SaveRepurposePlanRevision(ctx context.Context, plan domain.RepurposePlan, editorNote string) (domain.RepurposePlanRevision, error) {
	current, err := r.GetRepurposePlan(ctx, plan.ID)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if current == nil {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: no plan matches id %s", domain.ErrPlanNotFound, plan.ID)
	}
	if current.Status == "approved" {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: plan %s is approved and accepts no further revisions", domain.ErrPlanImmutable, plan.ID)
	}
	plan.ID = current.ID
	plan.CreatedAt = current.CreatedAt
	plan.Status = "draft"
	saved, err := r.SaveRepurposePlan(ctx, plan)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	defer tx.Rollback()
	var latest int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM repurpose_plan_revisions WHERE plan_id=?`, saved.ID).Scan(&latest); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	snapshot, err := json.Marshal(saved)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	created := time.Now().UTC()
	revision := domain.RepurposePlanRevision{ID: idgen.New(), PlanID: saved.ID, Revision: latest + 1, State: "draft", Plan: saved, EditorNote: strings.TrimSpace(editorNote), CreatedAt: created}
	_, err = tx.ExecContext(ctx, `INSERT INTO repurpose_plan_revisions(id,plan_id,revision,state,snapshot_json,editor_note,created_at) VALUES(?,?,?,?,?,?,?)`, revision.ID, revision.PlanID, revision.Revision, revision.State, string(snapshot), revision.EditorNote, formatTime(created))
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	return revision, nil
}

func (r *Repository) ListRepurposePlanRevisions(ctx context.Context, planID string) ([]domain.RepurposePlanRevision, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,revision,state,snapshot_json,editor_note,created_at,approved_at FROM repurpose_plan_revisions WHERE plan_id=? ORDER BY revision DESC`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.RepurposePlanRevision, 0)
	for rows.Next() {
		var item domain.RepurposePlanRevision
		var snapshot, created string
		var approved sql.NullString
		if err := rows.Scan(&item.ID, &item.Revision, &item.State, &snapshot, &item.EditorNote, &created, &approved); err != nil {
			return nil, err
		}
		item.PlanID = planID
		if err := json.Unmarshal([]byte(snapshot), &item.Plan); err != nil {
			return nil, fmt.Errorf("decode repurpose plan revision: %w", err)
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if approved.Valid {
			parsed, _ := time.Parse(time.RFC3339Nano, approved.String)
			item.ApprovedAt = &parsed
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ApproveRepurposePlanRevision is the write that makes a human's approval
// real, so its three preconditions -- the revision exists, it is still a
// draft, and it is the plan's latest -- are checked inside the same
// transaction as the two UPDATEs rather than by the caller beforehand. A
// caller's check cannot hold anything between its read and this write, so a
// concurrent approval can always land in that gap; only these can't be raced.
// They wrap domain sentinels for the reason SaveRepurposePlan's comment
// gives.
func (r *Repository) ApproveRepurposePlanRevision(ctx context.Context, planID string, revisionNumber int) (domain.RepurposePlanRevision, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	defer tx.Rollback()
	var id, state, snapshot, editorNote, created string
	err = tx.QueryRowContext(ctx, `SELECT id,state,snapshot_json,editor_note,created_at FROM repurpose_plan_revisions WHERE plan_id=? AND revision=?`, planID, revisionNumber).Scan(&id, &state, &snapshot, &editorNote, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: plan %s has no revision %d", domain.ErrPlanRevisionNotFound, planID, revisionNumber)
	}
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if state != "draft" {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: %s/%d is %s", domain.ErrPlanRevisionNotDraft, planID, revisionNumber, state)
	}
	var latest int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM repurpose_plan_revisions WHERE plan_id=?`, planID).Scan(&latest); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if revisionNumber != latest {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: %s/%d, latest is %d", domain.ErrPlanRevisionNotLatest, planID, revisionNumber, latest)
	}
	var plan domain.RepurposePlan
	if err := json.Unmarshal([]byte(snapshot), &plan); err != nil {
		return domain.RepurposePlanRevision{}, fmt.Errorf("decode repurpose plan snapshot: %w", err)
	}
	now := time.Now().UTC()
	plan.Status = "approved"
	plan.UpdatedAt = now
	approvedSnapshot, err := json.Marshal(plan)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repurpose_plan_revisions SET state='approved',snapshot_json=?,approved_at=? WHERE id=?`, string(approvedSnapshot), formatTime(now), id); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repurpose_plans SET status='approved',plan_json=?,updated_at=? WHERE id=?`, string(approvedSnapshot), formatTime(now), planID); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	createdAt, _ := time.Parse(time.RFC3339Nano, created)
	return domain.RepurposePlanRevision{ID: id, PlanID: planID, Revision: revisionNumber, State: "approved", Plan: plan, EditorNote: editorNote, CreatedAt: createdAt, ApprovedAt: &now}, nil
}
func (r *Repository) CreateTagCurationRun(ctx context.Context, proposals []domain.TagProposal, scanned int, strategy string) (domain.TagCurationResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.TagCurationResult{}, err
	}
	defer tx.Rollback()
	runID := idgen.New()
	now := formatTime(time.Now())
	stats, _ := json.Marshal(map[string]any{"unresolved_scanned": scanned, "proposals": len(proposals)})
	if _, err = tx.ExecContext(ctx, `INSERT INTO tag_curation_runs(id,state,strategy,input_revision,stats_json,created_at,finished_at) VALUES(?,'completed',?,?,?,?,?)`, runID, strategy, fmt.Sprint(scanned), string(stats), now, now); err != nil {
		return domain.TagCurationResult{}, err
	}
	for _, p := range proposals {
		payload, _ := json.Marshal(p.Payload)
		if _, err = tx.ExecContext(ctx, `INSERT INTO tag_change_proposals(id,run_id,state,proposal_type,canonical_name,payload_json,confidence,reason,affected_assets,created_at) VALUES(?,?,'pending',?,?,?,?,?,?,?)`, idgen.New(), runID, p.ProposalType, nullString(p.CanonicalName), string(payload), p.Confidence, p.Reason, p.AffectedAssets, now); err != nil {
			return domain.TagCurationResult{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return domain.TagCurationResult{}, err
	}
	return domain.TagCurationResult{RunID: runID, Strategy: strategy, UnresolvedScanned: scanned, ProposalsCreated: len(proposals)}, nil
}
func (r *Repository) ListTagProposals(ctx context.Context, state string, limit int) ([]domain.TagProposal, error) {
	q := `SELECT id,run_id,state,proposal_type,COALESCE(canonical_name,''),payload_json,confidence,reason,affected_assets,created_at,reviewed_at,COALESCE(review_note,'') FROM tag_change_proposals`
	args := []any{}
	if state != "" {
		q += ` WHERE state=?`
		args = append(args, state)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.TagProposal
	for rows.Next() {
		var p domain.TagProposal
		var payload, created string
		var reviewed sql.NullString
		if err := rows.Scan(&p.ID, &p.RunID, &p.State, &p.ProposalType, &p.CanonicalName, &payload, &p.Confidence, &p.Reason, &p.AffectedAssets, &created, &reviewed, &p.ReviewNote); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(payload), &p.Payload)
		p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if reviewed.Valid {
			t, _ := time.Parse(time.RFC3339Nano, reviewed.String)
			p.ReviewedAt = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (r *Repository) ReviewTagProposal(ctx context.Context, id, action, note string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var typ, canonical, payload, state string
	if err = tx.QueryRowContext(ctx, `SELECT proposal_type,COALESCE(canonical_name,''),payload_json,state FROM tag_change_proposals WHERE id=?`, id).Scan(&typ, &canonical, &payload, &state); err != nil {
		return err
	}
	if state != "pending" {
		return fmt.Errorf("proposal already reviewed")
	}
	now := formatTime(time.Now())
	if action == "reject" {
		_, err = tx.ExecContext(ctx, `UPDATE tag_change_proposals SET state='rejected',reviewed_at=?,review_note=? WHERE id=?`, now, note, id)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(payload), &body)
	var tagID string
	if v, ok := body["canonical_tag_id"].(string); ok {
		tagID = v
	} else {
		tagID = idgen.New()
		_, err = tx.ExecContext(ctx, `INSERT INTO tag_catalog(id,canonical_name,category,status,created_by,created_at,updated_at) VALUES(?,?,?,'active','curator',?,?)`, tagID, canonical, valueString(body["category"], "general"), now, now)
		if err != nil {
			return err
		}
	}
	aliases := stringSlice(body["aliases"])
	aliases = append(aliases, canonical)
	for _, alias := range aliases {
		n := normalizeTagValue(alias)
		if n == "" {
			continue
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO tag_aliases_v2(alias_normalized,alias_display,canonical_tag_id,source,confidence,created_at) VALUES(?,?,?,'curator',1.0,?) ON CONFLICT(alias_normalized) DO UPDATE SET canonical_tag_id=excluded.canonical_tag_id,alias_display=excluded.alias_display`, n, alias, tagID, now)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE asset_tag_links SET canonical_tag_id=?,updated_at=? WHERE normalized_tag=?`, tagID, now, n)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE tag_change_proposals SET state='approved',reviewed_at=?,review_note=? WHERE id=?`, now, note, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func valueString(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}
func stringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
