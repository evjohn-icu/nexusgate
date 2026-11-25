package sqlite

import (
	"context"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// ListDeriveModalityOutbox returns a bounded page of completed derives whose
// modality plan still needs to be queued. Their input identity is taken from
// the durable job, including after a Hub restart.
func (r *Repository) ListDeriveModalityOutbox(ctx context.Context, after string, limit int) ([]domain.Job, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT j.id,j.asset_id,j.input_hash FROM derive_modality_outbox o JOIN jobs j ON j.id=o.derive_job_id WHERE o.derive_job_id>? ORDER BY o.derive_job_id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []domain.Job
	for rows.Next() {
		var job domain.Job
		if err := rows.Scan(&job.ID, &job.AssetID, &job.InputHash); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (r *Repository) ClearDeriveModalityOutbox(ctx context.Context, jobID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM derive_modality_outbox WHERE derive_job_id=?`, jobID)
	return err
}
