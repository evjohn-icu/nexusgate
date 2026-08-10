package sqlite

import "context"

// TotalSourceBytes sums assets.file_size — the Hub's read-only estimate of how
// much original media the library holds. It is an estimate because the number
// is what the scanner observed when it fingerprinted each file, not a live
// walk: original media lives on the library roots, which the Hub never owns.
// The storage overview labels the row 估算（只读） for that reason.
func (r *Repository) TotalSourceBytes(ctx context.Context) (int64, error) {
	var total int64
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(file_size),0) FROM assets`).Scan(&total)
	return total, err
}
