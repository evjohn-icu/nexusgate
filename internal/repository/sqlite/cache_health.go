package sqlite

import (
	"context"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

func (r *Repository) MatchingDerivedArtifactsByProfilePrefixes(ctx context.Context, prefixes []string) ([]domain.DerivedArtifact, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(prefixes))
	args := make([]any, len(prefixes))
	for i, prefix := range prefixes {
		placeholders[i] = "profile_hash LIKE ?"
		args[i] = prefix + "%"
	}
	query := `SELECT id,asset_id,artifact_type,profile_hash,local_path,size_bytes FROM derived_artifacts WHERE artifact_type IN ('thumbnail','proxy') AND (` + strings.Join(placeholders, " OR ") + `) ORDER BY asset_id,artifact_type`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var artifacts []domain.DerivedArtifact
	for rows.Next() {
		var a domain.DerivedArtifact
		if err := rows.Scan(&a.ID, &a.AssetID, &a.Type, &a.ProfileHash, &a.LocalPath, &a.SizeBytes); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, a)
	}
	return artifacts, rows.Err()
}

// DeleteDerivedArtifactsByPaths removes only the rows whose local_path is
// exactly one of the given paths, and returns the distinct asset IDs whose
// rows were removed. Exact matching is the safety property, not convenience:
// cache repair has already selected the files it can manage on disk, so a row
// is deleted only when it names one of those files — a profile-prefix match
// would also sweep up rows whose files were never selected, stranding bytes
// outside the repair's reach. Files are owned by the cache command and are
// deleted separately, file first and row second, so a crash between the two
// leaves a row whose missing file the derive stage re-creates.
func (r *Repository) DeleteDerivedArtifactsByPaths(ctx context.Context, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	unique := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		unique = append(unique, p)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	assets := make([]string, 0, len(unique))
	for start := 0; start < len(unique); start += knownAssetIDsChunkSize {
		end := start + knownAssetIDsChunkSize
		if end > len(unique) {
			end = len(unique)
		}
		chunk := unique[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for i, p := range chunk {
			args[i] = p
		}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT asset_id FROM derived_artifacts WHERE local_path IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			assets = append(assets, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM derived_artifacts WHERE local_path IN (`+placeholders+`)`, args...); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return assets, nil
}

// ListDerivedArtifacts returns every derived_artifacts row. It backs the
// cache-health commands (`nexusgate cache gc`, `nexusgate cache verify`),
// which need the full row inventory to decide what on disk is orphaned and
// what is a rebuildable gap — SaveArtifact/GetArtifact only ever look at one
// artifact, and a consistency check that queried per-asset would never see
// the directory that has no asset.
func (r *Repository) ListDerivedArtifacts(ctx context.Context) ([]domain.DerivedArtifact, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,asset_id,artifact_type,profile_hash,local_path,size_bytes FROM derived_artifacts ORDER BY asset_id,artifact_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var artifacts []domain.DerivedArtifact
	for rows.Next() {
		var a domain.DerivedArtifact
		if err := rows.Scan(&a.ID, &a.AssetID, &a.Type, &a.ProfileHash, &a.LocalPath, &a.SizeBytes); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, a)
	}
	return artifacts, rows.Err()
}

// ListCacheProtectedAssetIDs returns the assets whose derived files cache
// maintenance must not delete. An asset is protected while a nonterminal
// pending, running, or failed job for it still has attempts remaining — the
// job will run (or run again) and its stage reads those files — and while a
// running job holds a live lease even on its final attempt, where a renewal
// or completion is still in flight. Files deleted under a queued job would
// turn "rebuild on demand" into "fail on next stage", so protection is keyed
// to schedulable work, not just to live leases.
func (r *Repository) ListCacheProtectedAssetIDs(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT asset_id FROM jobs WHERE asset_id IS NOT NULL AND terminal=0 AND ((state IN ('pending','running','failed') AND attempt_count<max_attempts) OR (state='running' AND lease_expires_at IS NOT NULL AND lease_expires_at>?)) ORDER BY asset_id`, formatTime(now))
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

// HasDurableRederive reports whether a nonterminal `cache-rederive:` job for
// the asset is already recorded. The cache commands gate deletion on a
// durable repair: a rebuild that is only promised after the files are gone is
// not a rebuild, it is a loss, so files may be removed when this job exists —
// whether it was enqueued by this invocation or is still queued from a
// previous one.
func (r *Repository) HasDurableRederive(ctx context.Context, assetID string) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE asset_id=? AND job_type='derive' AND input_hash LIKE 'cache-rederive:%' AND terminal=0 AND state IN ('pending','running','failed'))`, assetID).Scan(&exists)
	return exists, err
}

// ListAllAssetIDs returns every asset id in the assets table. It backs
// OrphanDirectories: a per-asset cache directory whose name matches no row
// here is an orphan.
func (r *Repository) ListAllAssetIDs(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM assets`)
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

// knownAssetIDsChunkSize keeps one IN clause below SQLite's variable limit.
// A single cache with tens of thousands of directories would blow past it if
// the whole id list went into one query.
const knownAssetIDsChunkSize = 500

// KnownAssetIDs returns the subset of ids that exist in the assets table,
// deduplicated and in first-seen input order. It is the repository half of
// cache orphan detection: the cache side lists its directories, and only the
// ones the database has never heard of are stale. The result ordering
// matters — the caller pairs it with its own directory list.
func (r *Repository) KnownAssetIDs(ctx context.Context, ids []string) ([]string, error) {
	unique := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return nil, nil
	}
	known := make(map[string]struct{}, len(unique))
	for start := 0; start < len(unique); start += knownAssetIDsChunkSize {
		end := start + knownAssetIDsChunkSize
		if end > len(unique) {
			end = len(unique)
		}
		chunk := unique[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := r.db.QueryContext(ctx, `SELECT id FROM assets WHERE id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			known[id] = struct{}{}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	result := make([]string, 0, len(known))
	for _, id := range unique {
		if _, ok := known[id]; ok {
			result = append(result, id)
		}
	}
	return result, nil
}
