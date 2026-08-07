package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// GetAlignmentWords returns the word-level forced-alignment result for the
// asset's latest succeeded alignment run, ordered by ordinal. A nil result
// with no error means no alignment exists: the caller falls back to the ASR
// transcript. Alignment words carry the strongest timing evidence the
// pipeline has, so video analysis must consume them when they exist — the
// align stage used to write them here and nothing ever read them back.
func (r *Repository) GetAlignmentWords(ctx context.Context, assetID string) ([]domain.AlignmentWord, error) {
	var runID string
	// rowid breaks created_at ties: SaveAlignment upserts on
	// (asset_id,provider,model,input_hash) without bumping created_at, so two
	// runs written within one timestamp tick could otherwise be picked
	// nondeterministically.
	err := r.db.QueryRowContext(ctx, `SELECT id FROM alignment_runs WHERE asset_id=? AND state='succeeded' ORDER BY created_at DESC, rowid DESC LIMIT 1`, assetID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT start_ms,end_ms,text,confidence FROM transcript_words WHERE alignment_run_id=? ORDER BY ordinal`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var words []domain.AlignmentWord
	for rows.Next() {
		var w domain.AlignmentWord
		var confidence sql.NullFloat64
		if err := rows.Scan(&w.StartMS, &w.EndMS, &w.Text, &confidence); err != nil {
			return nil, err
		}
		if confidence.Valid {
			value := confidence.Float64
			w.Confidence = &value
		}
		words = append(words, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return words, nil
}
