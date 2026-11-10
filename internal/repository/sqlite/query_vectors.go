package sqlite

import (
	"context"
	"database/sql"
	"fmt"
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
	result, err := unmarshalStoredInferenceResult(raw)
	if err != nil {
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

// QueryVectorByContentHash returns a staged query vector for identical image
// bytes in one space. Two uploads of the same picture reuse one encoding, which
// is what "deduplicate by space + content hash" means for the read path.
func (r *Repository) QueryVectorByContentHash(ctx context.Context, contentHash, spaceID string) ([]float32, bool, error) {
	var raw string
	err := r.db.QueryRowContext(ctx, `
		SELECT t.staged_json FROM inference_tasks t
		JOIN search_references r ON r.id = t.query_reference_id
		WHERE r.content_hash = ? AND t.operation='visual_embedding' AND t.staged_json IS NOT NULL
		ORDER BY t.updated_at DESC LIMIT 1`, contentHash).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	result, err := unmarshalStoredInferenceResult(raw)
	if err != nil {
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

// QueryJobByContentHash returns the newest query job for identical image bytes.
func (r *Repository) QueryJobByContentHash(ctx context.Context, contentHash string) (string, bool, error) {
	var jobID string
	err := r.db.QueryRowContext(ctx, `
		SELECT t.job_id FROM inference_tasks t
		JOIN search_references r ON r.id = t.query_reference_id
		WHERE r.content_hash = ? ORDER BY t.updated_at DESC LIMIT 1`, contentHash).Scan(&jobID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return jobID, true, nil
}
