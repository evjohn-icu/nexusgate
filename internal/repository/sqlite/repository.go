package sqlite

import (
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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ev/timingdex/internal/capture"
	"github.com/ev/timingdex/internal/discovery"
	"github.com/ev/timingdex/internal/domain"
	"github.com/ev/timingdex/internal/idgen"
	"github.com/ev/timingdex/internal/remote"
	"github.com/ev/timingdex/internal/textindex"
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

func (r *Repository) HeartbeatWorker(ctx context.Context, workerID string, capabilities remote.WorkerCapabilities) error {
	raw, err := json.Marshal(capabilities)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `UPDATE workers SET status='online',capabilities_json=?,last_seen_at=? WHERE id=? AND status!='revoked'`, string(raw), formatTime(time.Now().UTC()), workerID)
	return err
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
	db                  *sql.DB
	semanticVectorMu    sync.RWMutex
	semanticVectorCache map[string][]float64
}

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
	return &Repository{db: db, semanticVectorCache: make(map[string][]float64)}, nil
}

func (r *Repository) Close() error { return r.db.Close() }

func (r *Repository) Migrate(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		var exists int
		if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, entry.Name()).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(content)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, entry.Name(), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return r.ensureCJKBigramFTS(ctx)
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
	root := domain.LibraryRoot{ID: idgen.New(), Path: path, CreatedAt: now, UpdatedAt: now}
	_, err := r.db.ExecContext(ctx, `INSERT INTO library_roots(id, path, created_at, updated_at) VALUES (?, ?, ?, ?)`, root.ID, root.Path, formatTime(now), formatTime(now))
	if err != nil {
		return domain.LibraryRoot{}, err
	}
	return root, nil
}

func (r *Repository) ListLibraryRoots(ctx context.Context) ([]domain.LibraryRoot, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, path, created_at, updated_at FROM library_roots ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roots := []domain.LibraryRoot{}
	for rows.Next() {
		var root domain.LibraryRoot
		var created, updated string
		if err := rows.Scan(&root.ID, &root.Path, &created, &updated); err != nil {
			return nil, err
		}
		root.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		root.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

func (r *Repository) GetLibraryRoot(ctx context.Context, id string) (domain.LibraryRoot, error) {
	var root domain.LibraryRoot
	var created, updated string
	err := r.db.QueryRowContext(ctx, `SELECT id, path, created_at, updated_at FROM library_roots WHERE id = ?`, id).Scan(&root.ID, &root.Path, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.LibraryRoot{}, fmt.Errorf("library root not found: %s", id)
	}
	if err != nil {
		return domain.LibraryRoot{}, err
	}
	root.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	root.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return root, nil
}

func (r *Repository) UpsertScannedFile(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	var assetID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM assets WHERE quick_fingerprint = ? AND file_size = ? LIMIT 1`, fingerprint, info.Size()).Scan(&assetID)
	created := false
	if errors.Is(err, sql.ErrNoRows) {
		assetID = idgen.New()
		_, err = tx.ExecContext(ctx, `INSERT INTO assets(id, quick_fingerprint, file_size, state, first_seen_at, last_seen_at) VALUES (?, ?, ?, 'discovered', ?, ?)`, assetID, fingerprint, info.Size(), formatTime(now), formatTime(now))
		created = true
	}
	if err != nil {
		return false, err
	}

	var locationID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM asset_locations WHERE root_id = ? AND relative_path = ?`, root.ID, relativePath).Scan(&locationID)
	if errors.Is(err, sql.ErrNoRows) {
		locationID = idgen.New()
		_, err = tx.ExecContext(ctx, `INSERT INTO asset_locations(id, asset_id, root_id, relative_path, absolute_path, modified_ns, exists_now, is_primary, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, 1, 1, ?)`, locationID, assetID, root.ID, relativePath, absolutePath, info.ModTime().UnixNano(), formatTime(now))
	} else if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE asset_locations SET asset_id = ?, absolute_path = ?, modified_ns = ?, exists_now = 1, last_seen_at = ? WHERE id = ?`, assetID, absolutePath, info.ModTime().UnixNano(), formatTime(now), locationID)
	}
	if err != nil {
		return false, err
	}

	_, err = tx.ExecContext(ctx, `UPDATE assets SET state = 'discovered', last_seen_at = ?, missing_since = NULL WHERE id = ?`, formatTime(now), assetID)
	if err != nil {
		return false, err
	}

	return created, tx.Commit()
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
		if missing.Valid {
			parsed, _ := time.Parse(time.RFC3339Nano, missing.String)
			asset.MissingSince = &parsed
		}
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func (r *Repository) GetPrimaryLocation(ctx context.Context, assetID string) (domain.AssetLocation, error) {
	var v domain.AssetLocation
	var existsNow, primary int
	var last string
	err := r.db.QueryRowContext(ctx, `SELECT id,asset_id,root_id,relative_path,absolute_path,COALESCE(file_id,''),modified_ns,exists_now,is_primary,last_seen_at FROM asset_locations WHERE asset_id=? AND exists_now=1 ORDER BY is_primary DESC,last_seen_at DESC LIMIT 1`, assetID).Scan(&v.ID, &v.AssetID, &v.RootID, &v.RelativePath, &v.AbsolutePath, &v.FileID, &v.ModifiedNS, &existsNow, &primary, &last)
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO provider_channels(id,capability,label,provider_name,protocol,endpoint,model,enabled,route_order,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET capability=excluded.capability,label=excluded.label,provider_name=excluded.provider_name,protocol=excluded.protocol,endpoint=excluded.endpoint,model=excluded.model,enabled=excluded.enabled,route_order=excluded.route_order,deleted_at=NULL,updated_at=excluded.updated_at`, channel.ID, channel.Capability, channel.Label, channel.ProviderName, channel.Protocol, channel.Endpoint, channel.Model, boolInt(channel.Enabled), channel.RouteOrder, formatTime(now), formatTime(now)); err != nil {
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
	query := `SELECT id,capability,label,provider_name,protocol,endpoint,model,enabled,route_order,created_at,updated_at FROM provider_channels WHERE deleted_at IS NULL`
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
		if err := rows.Scan(&channel.ID, &channel.Capability, &channel.Label, &channel.ProviderName, &channel.Protocol, &channel.Endpoint, &channel.Model, &enabled, &channel.RouteOrder, &created, &updated); err != nil {
			return nil, err
		}
		channel.Enabled = enabled != 0
		channel.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		channel.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		memberRows, err := r.db.QueryContext(ctx, `SELECT id,channel_id,label,secret_ref,enabled,weight,max_inflight FROM provider_channel_members WHERE channel_id=? ORDER BY label`, channel.ID)
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
			channelsMember := member
			channel.Members = append(channel.Members, channelsMember)
		}
		if err := memberRows.Close(); err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	return channels, rows.Err()
}

// RebuildAutomaticShootSessions materializes deterministic, conservative
// capture groups for one library root. Manual sessions are intentionally left
// untouched; source files and metadata are never modified.
func (r *Repository) RebuildAutomaticShootSessions(ctx context.Context, rootID string) error {
	rows, err := r.db.QueryContext(ctx, `SELECT a.id,COALESCE(al.relative_path,''),COALESCE(cm.vendor,''),COALESCE(cm.model,''),COALESCE(cm.device_serial,''),cm.captured_at,COALESCE(m.duration_ms,0),COALESCE(cm.session_marker,''),COALESCE(cm.reel,'') FROM assets a JOIN asset_locations al ON al.asset_id=a.id AND al.root_id=? AND al.exists_now=1 LEFT JOIN capture_metadata cm ON cm.asset_id=a.id LEFT JOIN media_metadata m ON m.asset_id=a.id WHERE cm.captured_at IS NOT NULL`, rootID)
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

func (r *Repository) SaveArtifact(ctx context.Context, a domain.DerivedArtifact) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(asset_id,artifact_type,profile_hash) DO UPDATE SET local_path=excluded.local_path,size_bytes=excluded.size_bytes`, a.ID, a.AssetID, a.Type, a.ProfileHash, a.LocalPath, a.SizeBytes, formatTime(time.Now()))
	return err
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
func (r *Repository) LeaseNextJob(ctx context.Context, worker string, lease time.Duration, filter domain.LeaseFilter) (*domain.Job, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now()
	var j domain.Job
	var run string
	var last sql.NullString
	// INDEXED BY is deliberate, not a hint: without it SQLite picks
	// idx_jobs_ready and sorts every leasable row into a temp b-tree on each
	// lease. idx_jobs_lease_order (migration 0018) already stores the rows in
	// ORDER BY sequence, so the scan stops at the first match. Its partial
	// WHERE clause must keep matching the two terms below or SQLite rejects the
	// query outright -- a loud failure, which is the point.
	//
	// The size ceiling is expressed as "no oversized asset exists for this job"
	// rather than as a join so the shape above survives: a join would give the
	// planner a second table to order by and could cost the ordered scan. The
	// subquery is a primary-key lookup, and it is skipped entirely when the
	// ceiling is zero.
	err = tx.QueryRowContext(ctx, `SELECT id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,last_error_message FROM jobs INDEXED BY idx_jobs_lease_order WHERE state IN ('pending','failed') AND terminal=0 AND attempt_count<max_attempts AND run_after<=? AND (lease_expires_at IS NULL OR lease_expires_at<=?) AND (?=0 OR NOT EXISTS (SELECT 1 FROM assets a WHERE a.id=jobs.asset_id AND a.file_size>?)) ORDER BY priority DESC,created_at LIMIT 1`, formatTime(now), formatTime(now), filter.MaxAssetBytes, filter.MaxAssetBytes).Scan(&j.ID, &j.AssetID, &j.Type, &j.State, &j.Priority, &j.AttemptCount, &j.MaxAttempts, &run, &j.InputHash, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.RunAfter, _ = time.Parse(time.RFC3339Nano, run)
	if last.Valid {
		j.LastError = last.String
	}
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state='running',attempt_count=attempt_count+1,lease_owner=?,lease_expires_at=?,updated_at=? WHERE id=? AND state IN ('pending','failed')`, worker, formatTime(now.Add(lease)), formatTime(now), j.ID)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, nil
	}
	j.State = domain.JobRunning
	j.AttemptCount++
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &j, nil
}
func (r *Repository) CompleteJob(ctx context.Context, id string, state domain.JobState, errMsg string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE jobs SET state=?,last_error_message=?,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=?`, string(state), nullString(errMsg), formatTime(time.Now()), id)
	return err
}

// FailJobTerminally marks a job failed and flags it as terminal so the lease
// predicate stops returning it. CompleteJob alone leaves state='failed' with
// attempts still available, and LeaseNextJob deliberately picks failed jobs back
// up — so a failure the pipeline classified as permanent would otherwise be
// retried anyway, immediately and without even the backoff a retryable error
// gets. For a provider 4xx that means paying for the same rejected call again.
//
// attempt_count is left alone on purpose. An earlier implementation exhausted it
// to make the predicate skip the row, which worked but reported a job that ran
// once as "3/3" on the progress page.
func (r *Repository) FailJobTerminally(ctx context.Context, id, errMsg string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE jobs SET state=?,terminal=1,last_error_message=?,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=?`, string(domain.JobFailed), nullString(errMsg), formatTime(time.Now()), id)
	return err
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

// RetryJob returns a leased job to the pending queue after a bounded delay.
// Leasing already increments attempt_count, so the normal lease predicate
// enforces max_attempts without a separate mutable retry counter.
func (r *Repository) RetryJob(ctx context.Context, id, errMsg string, delay time.Duration) error {
	if delay < 0 {
		delay = 0
	}
	now := time.Now()
	_, err := r.db.ExecContext(ctx, `UPDATE jobs SET state='pending',run_after=?,last_error_message=?,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND state='running' AND attempt_count<max_attempts`, formatTime(now.Add(delay)), nullString(errMsg), formatTime(now), id)
	return err
}
func (r *Repository) ListJobs(ctx context.Context, limit int) ([]domain.Job, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,COALESCE(asset_id,''),job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,COALESCE(last_error_message,''),terminal FROM jobs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Job
	for rows.Next() {
		var j domain.Job
		var run string
		if err := rows.Scan(&j.ID, &j.AssetID, &j.Type, &j.State, &j.Priority, &j.AttemptCount, &j.MaxAttempts, &run, &j.InputHash, &j.LastError, &j.Terminal); err != nil {
			return nil, err
		}
		j.RunAfter, _ = time.Parse(time.RFC3339Nano, run)
		out = append(out, j)
	}
	return out, rows.Err()
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
	_ = r.db.QueryRowContext(ctx, `SELECT summary,scene_tags_json,subjects_json,mood_tags_json,extra_tags_json,editorial_reason FROM asset_analysis WHERE asset_id=?`, assetID).Scan(&summary, &tags, &subjects, &moods, &extra, &reason)
	_ = r.db.QueryRowContext(ctx, `SELECT full_text FROM transcripts WHERE asset_id=? AND status='succeeded' ORDER BY created_at DESC LIMIT 1`, assetID).Scan(&transcript)

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

func (r *Repository) ReplaceAssetShots(ctx context.Context, assetID, sourceRunID string, shots []domain.AssetShot) error {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO shot_semantic_vectors(shot_id,model,vector_json,source_text,created_at) VALUES(?,?,?,?,?)`, id, "semantic-hash-v1", string(vector), sourceText, formatTime(created)); err != nil {
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

func validateAssetShots(shots []domain.AssetShot) error {
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

func (r *Repository) SearchShots(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
	if limit <= 0 || strings.TrimSpace(q) == "" {
		return []domain.ShotSearchResult{}, nil
	}
	ftsQuery := buildFTSQuery(q)
	if ftsQuery == "" {
		return []domain.ShotSearchResult{}, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT s.id,s.asset_id,COALESCE(s.source_run_id,''),s.ordinal,s.start_ms,s.end_ms,s.description,s.tags_json,s.objects_json,s.actions_json,s.mood_json,s.confidence,s.created_at,bm25(asset_shot_search) FROM asset_shot_search JOIN asset_shots s ON s.id=asset_shot_search.shot_id WHERE asset_shot_search MATCH ? ORDER BY bm25(asset_shot_search),s.ordinal LIMIT ?`, ftsQuery, limit)
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

// HybridSearchShots blends local lexical FTS with deterministic semantic
// features generated from the model's already-persisted visual observations.
// It stays SQLite-first and returns source shot time ranges.
func (r *Repository) HybridSearchShots(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
	if limit <= 0 || strings.TrimSpace(q) == "" {
		return []domain.ShotSearchResult{}, nil
	}
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
			if rank < 0 {
				rank = -rank
			}
			lexical[id] = 1 / (1 + rank)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	queryVector := discovery.VectorForText(q)
	rows, err := r.db.QueryContext(ctx, `SELECT s.id,s.asset_id,COALESCE(s.source_run_id,''),s.ordinal,s.start_ms,s.end_ms,s.description,s.tags_json,s.objects_json,s.actions_json,s.mood_json,s.confidence,s.created_at,v.vector_json FROM asset_shots s JOIN shot_semantic_vectors v ON v.shot_id=s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make([]domain.ShotSearchResult, 0)
	for rows.Next() {
		var result domain.ShotSearchResult
		var tags, objects, actions, mood, created, encodedVector string
		if err := rows.Scan(&result.ID, &result.AssetID, &result.SourceRunID, &result.Ordinal, &result.StartMS, &result.EndMS, &result.Description, &tags, &objects, &actions, &mood, &result.Confidence, &created, &encodedVector); err != nil {
			return nil, err
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
		result.LexicalScore = lexical[result.ID]
		result.Score = 0.70*result.SemanticScore + 0.30*result.LexicalScore
		if result.Score > 0 {
			results = append(results, result)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].ID < results[j].ID
		}
		return results[i].Score > results[j].Score
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (r *Repository) SimilarShots(ctx context.Context, shotID string, limit int) ([]domain.ShotSearchResult, error) {
	if strings.TrimSpace(shotID) == "" || limit <= 0 {
		return []domain.ShotSearchResult{}, nil
	}
	var encoded string
	err := r.db.QueryRowContext(ctx, `SELECT vector_json FROM shot_semantic_vectors WHERE shot_id=?`, shotID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("shot not found: %s", shotID)
	}
	if err != nil {
		return nil, err
	}
	queryVector, err := r.semanticVector(shotID, encoded)
	if err != nil {
		return nil, fmt.Errorf("decode source shot vector: %w", err)
	}
	records, err := r.loadSemanticShotRecords(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]domain.ShotSearchResult, 0, len(records))
	for _, record := range records {
		if record.result.ID == shotID {
			continue
		}
		record.result.SemanticScore = discovery.Cosine(queryVector, record.vector)
		record.result.Score = record.result.SemanticScore
		if record.result.Score > 0 {
			results = append(results, record.result)
		}
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (r *Repository) DiscoverRareShots(ctx context.Context, limit int) ([]domain.RareShot, error) {
	if limit <= 0 {
		return []domain.RareShot{}, nil
	}
	records, err := r.loadSemanticShotRecords(ctx)
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

func (r *Repository) loadSemanticShotRecords(ctx context.Context) ([]semanticShotRecord, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT s.id,s.asset_id,COALESCE(s.source_run_id,''),s.ordinal,s.start_ms,s.end_ms,s.description,s.tags_json,s.objects_json,s.actions_json,s.mood_json,s.confidence,s.created_at,v.vector_json FROM asset_shots s JOIN shot_semantic_vectors v ON v.shot_id=s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]semanticShotRecord, 0)
	for rows.Next() {
		var record semanticShotRecord
		var tags, objects, actions, mood, created, encodedVector string
		if err := rows.Scan(&record.result.ID, &record.result.AssetID, &record.result.SourceRunID, &record.result.Ordinal, &record.result.StartMS, &record.result.EndMS, &record.result.Description, &tags, &objects, &actions, &mood, &record.result.Confidence, &created, &encodedVector); err != nil {
			return nil, err
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

func (r *Repository) Search(ctx context.Context, q string, limit int) ([]string, error) {
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
		rows, err := r.db.QueryContext(ctx, `SELECT asset_id FROM (
SELECT l.asset_id FROM asset_tag_links l WHERE l.normalized_tag=?
UNION
SELECT l.asset_id FROM asset_tag_links l JOIN tag_catalog t ON t.id=l.canonical_tag_id WHERE t.canonical_name=?
UNION
SELECT l.asset_id FROM asset_tag_links l JOIN tag_aliases_v2 a ON a.canonical_tag_id=l.canonical_tag_id WHERE a.alias_normalized=?
)
LIMIT ?`, tag, tag, tag, limit-len(ids))
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
	rows, err := r.db.QueryContext(ctx, `SELECT asset_id FROM asset_search WHERE asset_search MATCH ? LIMIT ?`, ftsQuery, limit-len(ids))
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

func (r *Repository) CreateModelRun(ctx context.Context, assetID, capability, provider, model, inputHash, promptVersion, schemaVersion, requestJSON string) (string, bool, error) {
	var existingID, state string
	err := r.db.QueryRowContext(ctx, `SELECT id,state FROM model_runs WHERE capability=? AND provider=? AND model=? AND input_hash=? AND prompt_version=? AND schema_version=?`, capability, provider, model, inputHash, promptVersion, schemaVersion).Scan(&existingID, &state)
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

func (r *Repository) FailModelRun(ctx context.Context, runID, code, message, raw string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE model_runs SET state='failed',raw_response=?,error_code=?,error_message=?,finished_at=? WHERE id=?`, raw, code, message, formatTime(time.Now()), runID)
	return err
}

func (r *Repository) StageModelRun(ctx context.Context, runID, raw, parsed string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE model_runs SET state='validated',raw_response=?,parsed_json=?,validation_errors=NULL,finished_at=? WHERE id=?`, raw, parsed, formatTime(time.Now()), runID)
	return err
}

func (r *Repository) CommitAnalysis(ctx context.Context, assetID, runID, schemaVersion string, a domain.StructuredAnalysis) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := r.commitAnalysisTx(ctx, tx, assetID, runID, schemaVersion, a); err != nil {
		return err
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
func (r *Repository) CommitAnalysisWithShots(ctx context.Context, assetID, runID, schemaVersion string, a domain.StructuredAnalysis, shots []domain.AssetShot) error {
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
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	_, err := tx.ExecContext(ctx, `INSERT INTO asset_analysis(asset_id,source_run_id,schema_version,asset_type,shot_size,camera_motion,audio_type,lighting,people_count,has_speech,quality,summary,scene_tags_json,subjects_json,mood_tags_json,usable_as_json,quality_flags_json,extra_tags_json,editorial_reason,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(asset_id) DO UPDATE SET source_run_id=excluded.source_run_id,schema_version=excluded.schema_version,asset_type=excluded.asset_type,shot_size=excluded.shot_size,camera_motion=excluded.camera_motion,audio_type=excluded.audio_type,lighting=excluded.lighting,people_count=excluded.people_count,has_speech=excluded.has_speech,quality=excluded.quality,summary=excluded.summary,scene_tags_json=excluded.scene_tags_json,subjects_json=excluded.subjects_json,mood_tags_json=excluded.mood_tags_json,usable_as_json=excluded.usable_as_json,quality_flags_json=excluded.quality_flags_json,extra_tags_json=excluded.extra_tags_json,editorial_reason=excluded.editorial_reason,updated_at=excluded.updated_at`, assetID, runID, schemaVersion, a.AssetType, a.ShotSize, a.CameraMotion, a.AudioType, a.Lighting, a.PeopleCount, boolInt(a.HasSpeech), a.Quality, a.Summary, j(a.SceneTags), j(a.Subjects), j(a.MoodTags), j(a.UsableAs), j(a.QualityFlags), j(a.ExtraTags), a.EditorialReason, formatTime(time.Now()))
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE model_runs SET state='committed',committed_at=? WHERE id=? AND state='validated'`, formatTime(time.Now()), runID); err != nil {
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

func (r *Repository) SaveTranscript(ctx context.Context, assetID, provider, model, inputHash string, t domain.Transcript) error {
	segments, _ := json.Marshal(t.Segments)
	_, err := r.db.ExecContext(ctx, `INSERT INTO transcripts(id,asset_id,provider,model,input_hash,language,full_text,segments_json,raw_response,status,created_at)
VALUES(?,?,?,?,?,?,?,?,?,'succeeded',?) ON CONFLICT(asset_id,input_hash) DO UPDATE SET language=excluded.language,full_text=excluded.full_text,segments_json=excluded.segments_json,raw_response=excluded.raw_response,status='succeeded'`,
		idgen.New(), assetID, provider, model, inputHash, t.Language, t.Text, string(segments), t.RawResponse, formatTime(time.Now().UTC()))
	return err
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
	d.Asset.FirstSeenAt, _ = time.Parse(time.RFC3339Nano, first)
	d.Asset.LastSeenAt, _ = time.Parse(time.RFC3339Nano, last)
	if missing.Valid {
		t, _ := time.Parse(time.RFC3339Nano, missing.String)
		d.Asset.MissingSince = &t
	}
	if loc, err := r.GetPrimaryLocation(ctx, assetID); err == nil {
		d.Location = &loc
	}
	d.Metadata, _ = r.GetMediaMetadata(ctx, assetID)
	d.Transcript, _ = r.GetTranscript(ctx, assetID)
	if a, _ := r.GetArtifact(ctx, assetID, "thumbnail"); a != nil {
		d.ThumbnailPath = a.LocalPath
	}
	if a, _ := r.GetArtifact(ctx, assetID, "proxy"); a != nil {
		d.ProxyPath = a.LocalPath
	}
	var raw string
	if err := r.db.QueryRowContext(ctx, `SELECT json_object('asset_type',asset_type,'scene_tags',json(scene_tags_json),'subjects',json(subjects_json),'people_count',people_count,'shot_size',shot_size,'camera_motion',camera_motion,'lighting',lighting,'audio_type',audio_type,'has_speech',has_speech,'summary',summary,'usable_as',json(usable_as_json),'mood_tags',json(mood_tags_json),'quality',quality,'quality_flags',json(quality_flags_json),'extra_tags',json(extra_tags_json),'editorial_reason',editorial_reason) FROM asset_analysis WHERE asset_id=?`, assetID).Scan(&raw); err == nil {
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
	for typ, values := range groups {
		for _, raw := range values {
			n := normalizeTagValue(raw)
			if n == "" {
				continue
			}
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

func (r *Repository) SaveRepurposePlan(ctx context.Context, plan domain.RepurposePlan) (domain.RepurposePlan, error) {
	var currentState string
	err := r.db.QueryRowContext(ctx, `SELECT status FROM repurpose_plans WHERE id=?`, plan.ID).Scan(&currentState)
	if err == nil && currentState == "approved" {
		return domain.RepurposePlan{}, fmt.Errorf("approved repurpose plan is immutable: %s", plan.ID)
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
		return domain.RepurposePlanRevision{}, fmt.Errorf("repurpose plan not found: %s", plan.ID)
	}
	if current.Status == "approved" {
		return domain.RepurposePlanRevision{}, fmt.Errorf("approved repurpose plan is immutable: %s", plan.ID)
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

func (r *Repository) ApproveRepurposePlanRevision(ctx context.Context, planID string, revisionNumber int) (domain.RepurposePlanRevision, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	defer tx.Rollback()
	var id, state, snapshot, editorNote, created string
	err = tx.QueryRowContext(ctx, `SELECT id,state,snapshot_json,editor_note,created_at FROM repurpose_plan_revisions WHERE plan_id=? AND revision=?`, planID, revisionNumber).Scan(&id, &state, &snapshot, &editorNote, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RepurposePlanRevision{}, fmt.Errorf("repurpose plan revision not found: %s/%d", planID, revisionNumber)
	}
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if state != "draft" {
		return domain.RepurposePlanRevision{}, fmt.Errorf("repurpose plan revision is not draft: %s/%d", planID, revisionNumber)
	}
	var latest int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM repurpose_plan_revisions WHERE plan_id=?`, planID).Scan(&latest); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if revisionNumber != latest {
		return domain.RepurposePlanRevision{}, fmt.Errorf("only latest repurpose plan revision can be approved")
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
