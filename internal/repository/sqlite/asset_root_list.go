package sqlite

import (
	"context"
)

// ListAssetsByRoot returns the ids of assets with a current location under one
// library root. Rebuild commands use it to scope work without loading whole
// asset rows.
func (r *Repository) ListAssetsByRoot(ctx context.Context, rootID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT asset_id FROM asset_locations
		WHERE root_id=? AND exists_now=1
		ORDER BY asset_id`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
