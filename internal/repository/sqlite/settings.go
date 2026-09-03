package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// PipelineThrottleConfigured reports whether a pipeline_throttle settings row
// exists. The pipeline uses it to distinguish "0 = the default" from "0 = the
// operator explicitly disabled the disk preflight in the settings page"; only
// an existing row can carry an intentional zero.
func (r *Repository) PipelineThrottleConfigured(ctx context.Context) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM settings WHERE key='pipeline_throttle')`).Scan(&exists)
	return exists, err
}

// GetPipelineThrottle reads the throttle from the settings table. When the row
// is absent (fresh install) it returns the default with a nil error. When the
// stored JSON is corrupt it still returns the default so the caller can
// continue safely, but with a non-nil error so the corruption is not silent.
func (r *Repository) GetPipelineThrottle(ctx context.Context) (domain.PipelineThrottle, error) {
	var raw string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='pipeline_throttle'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DefaultPipelineThrottle(), nil
	}
	if err != nil {
		return domain.DefaultPipelineThrottle(), err
	}
	var throttle domain.PipelineThrottle
	if err := json.Unmarshal([]byte(raw), &throttle); err != nil {
		return domain.DefaultPipelineThrottle(), err
	}
	return throttle, nil
}

// SavePipelineThrottle validates the throttle and upserts it as JSON under the
// key "pipeline_throttle". A failing Validate prevents the write entirely.
func (r *Repository) SavePipelineThrottle(ctx context.Context, throttle domain.PipelineThrottle) error {
	if err := throttle.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(throttle)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES('pipeline_throttle',?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, string(raw), formatTime(time.Now().UTC()))
	return err
}
