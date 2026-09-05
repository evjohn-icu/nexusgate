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

// DeleteDerivedArtifactsByProfilePrefixes removes only the requested preview
// rows. Files are owned by the cache command and are deleted separately.
func (r *Repository) DeleteDerivedArtifactsByProfilePrefixes(ctx context.Context, prefixes []string) ([]string, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	placeholders := make([]string, len(prefixes))
	args := make([]any, len(prefixes))
	for i, prefix := range prefixes {
		placeholders[i] = "profile_hash LIKE ?"
		args[i] = prefix + "%"
	}
	query := `SELECT DISTINCT asset_id FROM derived_artifacts WHERE artifact_type IN ('thumbnail','proxy') AND (` + strings.Join(placeholders, " OR ") + `)`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var assets []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		assets = append(assets, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	deleteQuery := `DELETE FROM derived_artifacts WHERE artifact_type IN ('thumbnail','proxy') AND (` + strings.Join(placeholders, " OR ") + `)`
	if _, err := tx.ExecContext(ctx, deleteQuery, args...); err != nil {
		return nil, err
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

// ListLiveJobAssetIDs returns assets whose non-terminal running job still has
// a valid lease. Cache maintenance protects all of their derived files.
func (r *Repository) ListLiveJobAssetIDs(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT asset_id FROM jobs WHERE asset_id IS NOT NULL AND state='running' AND terminal=0 AND lease_expires_at IS NOT NULL AND lease_expires_at > ?`, formatTime(now))
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
