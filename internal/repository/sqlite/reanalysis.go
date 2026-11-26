package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
	"github.com/evjohn-icu/nexusgate/internal/ingest"
)

// EnqueueReanalysis forces the analyze stage to run again for an asset whose
// canonical analysis is already committed. Prompt/schema/validator changes
// must not require deleting the library: the old model run stays immutable
// and auditable, this call records the operator's request, and the new job's
// input hash is nonced so it can never collide with the succeeded job's
// (INSERT OR IGNORE dedup would otherwise swallow the reanalysis wholesale).
// When the new job runs, CreateModelRun's own dedup key differs on input
// hash, so a fresh run is produced and CommitAnalysisWithShots switches the
// canonical rows to it; JobIndex then rebuilds the asset-level FTS row.
func (r *Repository) EnqueueReanalysis(ctx context.Context, assetID, reason string) error {
	// Analyze requires the proxy and the media metadata; the transcript is
	// optional (the stage handles a nil transcript, and an untimed one is
	// withheld in split mode anyway). Requiring both up front keeps a doomed
	// job — one that would permanently fail before ever calling a provider —
	// out of the queue.
	proxy, err := r.GetArtifact(ctx, assetID, "proxy")
	if err != nil {
		return err
	}
	metadata, err := r.GetMediaMetadata(ctx, assetID)
	if err != nil {
		return err
	}
	if proxy == nil || metadata == nil {
		return fmt.Errorf("asset %s has no proxy artifact or media metadata; reanalysis has nothing to run on", assetID)
	}
	hash := reanalysisHash(assetID, reason)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at) VALUES(?,?,?,'pending',30,0,3,?,?,?,?)`,
		idgen.New(), assetID, string(domain.JobAnalyze), formatTime(time.Now()), hash, now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO reanalysis_requests(id,asset_id,reason,input_hash,created_at) VALUES(?,?,?,?,?)`,
		idgen.New(), assetID, reason, hash, now); err != nil {
		return err
	}
	return tx.Commit()
}

// reanalysisHash mirrors app.hashStrings without importing app: the input
// hash is the run identity that must break every dedup, so it includes the
// operator's stated reason and a per-invocation nonce.
func reanalysisHash(assetID, reason string) string {
	h := sha256.New()
	for _, s := range []string{"reanalysis-v1", assetID, reason, idgen.New()} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// HasCommittedAnalysis reports whether the asset's canonical analysis is
// committed — an asset_analysis row or any committed shot row. It is the
// "already paid for" signal the re-derive path uses to stop a JobDerive from
// re-enqueueing the paid chain after `cache gc --rebuildable` deleted the
// derived files: the files are what the clean-up asked to rebuild, never a
// second model run. Either row existing is enough: modern analysis commits
// both together (CommitAnalysisWithShots), and an asset with only the legacy
// asset_analysis row is still analyzed.
func (r *Repository) HasCommittedAnalysis(ctx context.Context, assetID string) (bool, error) {
	return hasCommittedAnalysis(ctx, r.db, assetID)
}

func hasCommittedAnalysis(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, assetID string) (bool, error) {
	var exists int
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM asset_analysis WHERE asset_id=?)
		OR EXISTS(SELECT 1 FROM asset_shots WHERE asset_id=?)`, assetID, assetID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists > 0, nil
}

// EnqueueRederive enqueues a JobDerive that re-creates an asset's rebuildable
// artifacts after `cache gc --rebuildable` or `cache repair-derived` deleted
// the files but kept the derived_artifacts rows. Only assets whose canonical
// analysis is committed are re-derived: the derive stage skips the paid chain
// for them (see Pipeline's committed check), so a cache clean-up can never
// re-bill a model run. Assets without committed analysis are left alone —
// their chain is still in flight, and a rescan/retry/reanalysis is the
// operator's path.
//
// The job's input hash is the maintenance key `cache-rederive:` plus the
// asset's canonical derive identity, hashStrings(StableAssetKey(
// fingerprint,size,modifiedNS),"derive-v1"), optionally disambiguated with the
// repair round when an earlier repair with the same identity has already run
// to a terminal state. The key is deterministic per asset — repeating a repair
// while one is queued deduplicates into the queued job instead of piling up
// duplicate work — and the `cache-rederive:` prefix is what the lease queries
// key on: a re-derive whose source is offline stays queued, spending no
// attempts, until a current root and location are online again.
//
// The enqueue deliberately works while the media root is offline. Reading the
// source is the derive stage's job; recording the repair only needs canonical
// metadata from the database. When that identity cannot be reconstructed
// (no committed probe metadata), no job is recorded and the caller keeps the
// artifacts: a repair that cannot be durably recorded must not delete.
// Returns whether a job was enqueued by this call; use HasDurableRederive to
// learn whether a durable repair exists (this one or a queued one).
func (r *Repository) EnqueueRederive(ctx context.Context, assetID string) (bool, error) {
	committed, err := r.HasCommittedAnalysis(ctx, assetID)
	if err != nil {
		return false, err
	}
	if !committed {
		return false, nil
	}
	metadata, err := r.GetMediaMetadata(ctx, assetID)
	if err != nil {
		return false, err
	}
	if metadata == nil {
		return false, nil
	}
	fingerprint, size, modifiedNS, ok := r.canonicalDeriveIdentity(ctx, assetID)
	if !ok {
		return false, nil
	}
	durable, err := r.HasDurableRederive(ctx, assetID)
	if err != nil {
		return false, err
	}
	if durable {
		return false, nil
	}
	// Repair round: an earlier repair of this identity may already be recorded
	// in a terminal state (it succeeded, or exhausted its attempts). The
	// UNIQUE(asset_id, job_type, input_hash) dedup would silently swallow a
	// repeat under the identical key, deleting files with no rebuild behind
	// them — so a fresh repair takes the next round number instead. Cache
	// maintenance holds the exclusive cache lock across enqueue-and-delete,
	// which serializes this count-and-insert per data directory.
	var round int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE asset_id=? AND job_type='derive' AND input_hash LIKE 'cache-rederive:%'`, assetID).Scan(&round); err != nil {
		return false, err
	}
	key := cacheRederiveKey(fingerprint, size, modifiedNS, round)
	now := time.Now().UTC()
	res, err := r.db.ExecContext(ctx, `INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at) VALUES(?,?,?,'pending',90,0,3,?,?,?,?)`,
		idgen.New(), assetID, string(domain.JobDerive), formatTime(time.Now()), key, formatTime(now), formatTime(now))
	if err != nil {
		return false, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return inserted > 0, nil
}

// cacheRederiveKey builds the maintenance input hash for a re-derive of the
// canonical identity (fingerprint, size, modifiedNS). The identity part is
// hashStrings(ingest.StableAssetKey(...), "derive-v1") — the exact identity an
// ordinary derive plans its modality successors from — and the round
// disambiguates repeated repairs of the same identity, so modality planning
// for any repair round lands on the same generation as the original derive.
func cacheRederiveKey(fingerprint string, size, modifiedNS int64, round int) string {
	identity := hashStringsMirror(ingest.StableAssetKey(fingerprint, size, modifiedNS), "derive-v1")
	if round <= 0 {
		return "cache-rederive:" + identity
	}
	return "cache-rederive:" + identity + "@" + strconv.Itoa(round)
}

// hashStringsMirror mirrors app.hashStrings (each string followed by a zero
// byte into one SHA-256) without importing app, which imports this package.
func hashStringsMirror(values ...string) string {
	h := sha256.New()
	for _, v := range values {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalDeriveIdentity reconstructs the asset's canonical derive identity
// from persisted metadata, with the same value GetPrimaryLocation hands the
// pipeline: quick_fingerprint, file_size, and COALESCE(probe_modified_ns,
// primary location's modified_ns). It reads no file, so it works while the
// media root is offline; when even the persisted metadata cannot produce the
// identity (no committed probe, no location row), ok is false and no durable
// repair may be recorded.
func (r *Repository) canonicalDeriveIdentity(ctx context.Context, assetID string) (fingerprint string, size, modifiedNS int64, ok bool) {
	var probeModified sql.NullInt64
	err := r.db.QueryRowContext(ctx, `SELECT quick_fingerprint,file_size,probe_modified_ns FROM assets WHERE id=?`, assetID).Scan(&fingerprint, &size, &probeModified)
	if err != nil {
		return "", 0, 0, false
	}
	if probeModified.Valid {
		return fingerprint, size, probeModified.Int64, true
	}
	// No committed probe timestamp: fall back to the primary location's
	// modified_ns exactly when GetPrimaryLocation would (exists now, root not
	// unavailable). Offline, this is the one piece of the identity a
	// scan-recorded location row can still supply.
	err = r.db.QueryRowContext(ctx, `SELECT l.modified_ns FROM asset_locations l JOIN library_roots lr ON lr.id=l.root_id WHERE l.asset_id=? AND l.exists_now=1 AND lr.health_state<>'unavailable' ORDER BY l.is_primary DESC,l.last_seen_at DESC,lr.created_at,lr.id,l.relative_path,l.id LIMIT 1`, assetID).Scan(&modifiedNS)
	if err != nil {
		return "", 0, 0, false
	}
	return fingerprint, size, modifiedNS, true
}
