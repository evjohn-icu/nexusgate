package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// QueryReferenceVector returns the vector a completed assetless visual query
// task produced for one reference and space. It is the cache the search path
// reads: a query image is encoded once, and every later search with the same
// reference reuses the staged result without running a model again.
func (r *Repository) QueryReferenceVector(ctx context.Context, referenceID, spaceID string) ([]float32, bool, error) {
	var raw string
	err := r.db.QueryRowContext(ctx, `
		SELECT staged_json FROM inference_tasks
		WHERE query_reference_id=? AND operation='visual_embedding' AND staged_json IS NOT NULL
		ORDER BY updated_at DESC LIMIT 1`, referenceID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var result domain.InferenceResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, false, fmt.Errorf("staged query result is unreadable: %w", err)
	}
	if result.SpaceID != spaceID {
		return nil, false, nil
	}
	for _, record := range result.Embeddings {
		if len(record.Vector) > 0 {
			return record.Vector, true, nil
		}
	}
	return nil, false, nil
}

// QueryReferenceJob returns the most recent query task id for a reference, so
// an API can tell a caller which job to poll while the image is still encoding.
func (r *Repository) QueryReferenceJob(ctx context.Context, referenceID string) (string, bool, error) {
	var jobID string
	err := r.db.QueryRowContext(ctx, `
		SELECT job_id FROM inference_tasks
		WHERE query_reference_id=? ORDER BY updated_at DESC LIMIT 1`, referenceID).Scan(&jobID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return jobID, true, nil
}
