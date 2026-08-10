package sqlite

import (
	"context"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// ListDerivedArtifacts returns every derived_artifacts row. It backs the
// cache-health commands (`timingdex cache gc`, `timingdex cache verify`),
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
