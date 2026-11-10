package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// CreateSearchReference records a Hub-managed query image. The bytes are
// already on disk (written by the service under a generated name); this row is
// the authority that makes them reachable and expires them.
func (r *Repository) CreateSearchReference(ctx context.Context, ref domain.SearchReference) error {
	if strings.TrimSpace(ref.ID) == "" || strings.TrimSpace(ref.ContentHash) == "" || strings.TrimSpace(ref.RelativeCachePath) == "" {
		return fmt.Errorf("search reference is incomplete")
	}
	if ref.CreatedAt.IsZero() || ref.ExpiresAt.IsZero() || !ref.ExpiresAt.After(ref.CreatedAt) {
		return fmt.Errorf("search reference has no valid lifetime")
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO search_references(id,content_hash,media_type,relative_cache_path,width,height,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		ref.ID, ref.ContentHash, ref.MediaType, ref.RelativeCachePath, ref.Width, ref.Height, formatTime(ref.CreatedAt), formatTime(ref.ExpiresAt))
	return err
}

// GetSearchReference returns one unexpired reference. An expired row is
// reported as absent so a caller cannot keep searching with bytes the service
// is about to delete.
func (r *Repository) GetSearchReference(ctx context.Context, id string) (domain.SearchReference, error) {
	var ref domain.SearchReference
	var created, expires string
	err := r.db.QueryRowContext(ctx, `SELECT id,content_hash,media_type,relative_cache_path,width,height,created_at,expires_at FROM search_references WHERE id=?`, id).
		Scan(&ref.ID, &ref.ContentHash, &ref.MediaType, &ref.RelativeCachePath, &ref.Width, &ref.Height, &created, &expires)
	if err != nil {
		return domain.SearchReference{}, err
	}
	ref.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return domain.SearchReference{}, err
	}
	ref.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return domain.SearchReference{}, err
	}
	if !ref.ExpiresAt.After(time.Now().UTC()) {
		return domain.SearchReference{}, fmt.Errorf("search reference %s expired: %w", id, domain.ErrSearchReferenceExpired)
	}
	return ref, nil
}

// DeleteSearchReference removes the row and reports whether it existed.
func (r *Repository) DeleteSearchReference(ctx context.Context, id string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM search_references WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ExpiredSearchReferences returns the ids whose lifetime ended, so the service
// can cancel dependent tasks and delete the managed files before removing rows.
func (r *Repository) ExpiredSearchReferences(ctx context.Context, now time.Time) ([]domain.SearchReference, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,content_hash,media_type,relative_cache_path,width,height,created_at,expires_at FROM search_references WHERE expires_at<=?`, formatTime(now.UTC()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SearchReference
	for rows.Next() {
		var ref domain.SearchReference
		var created, expires string
		if err := rows.Scan(&ref.ID, &ref.ContentHash, &ref.MediaType, &ref.RelativeCachePath, &ref.Width, &ref.Height, &created, &expires); err != nil {
			return nil, err
		}
		ref.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		ref.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
		out = append(out, ref)
	}
	return out, rows.Err()
}

// CancelInferenceTasksForReference fails queued query tasks bound to a
// reference and flags running ones for cancellation. Deleting a query image
// must take its in-flight work with it: a late result would otherwise publish
// vectors for bytes the operator deleted.
func (r *Repository) CancelInferenceTasksForReference(ctx context.Context, referenceID string) error {
	if strings.TrimSpace(referenceID) == "" {
		return nil
	}
	now := formatTime(time.Now().UTC())
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE inference_tasks SET cancel_requested=1,updated_at=? WHERE query_reference_id=?`, now, referenceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state='failed',terminal=1,last_error_code='cancelled',last_error_message='query reference deleted',updated_at=?
		WHERE id IN (SELECT job_id FROM inference_tasks WHERE query_reference_id=?)
		  AND state='pending'`, now, referenceID); err != nil {
		return err
	}
	return tx.Commit()
}
