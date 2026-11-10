package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
)

// CreateModelManagementTask inserts a maintenance job and its task row in one
// transaction. The job is a normal queue entry with job_type model_install or
// model_probe; the task row is what makes it executable and auditable.
func (r *Repository) CreateModelManagementTask(ctx context.Context, task domain.ModelManagementTask, priority int) (string, error) {
	if task.Action != "install" && task.Action != "probe" {
		return "", fmt.Errorf("unknown model management action %q", task.Action)
	}
	if strings.TrimSpace(task.CatalogID) == "" || strings.TrimSpace(task.ManifestDigest) == "" {
		return "", fmt.Errorf("model management task lacks a catalog identity or manifest digest")
	}
	jobID := task.JobID
	if jobID == "" {
		jobID = idgen.New()
	}
	now := time.Now().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	jobType := domain.JobModelInstall
	if task.Action == "probe" {
		jobType = domain.JobModelProbe
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at)
		VALUES(?,NULL,?, 'pending', ?,0,3,?,?,?,?)`,
		jobID, string(jobType), priority, formatTime(now), "model-"+task.CatalogID+"-"+task.Action, formatTime(now), formatTime(now)); err != nil {
		return "", err
	}
	licenseAccepted := any(nil)
	if task.LicenseAccepted {
		licenseAccepted = formatTime(now)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO model_management_tasks(job_id,target_worker_id,catalog_id,manifest_digest,action,license_accepted_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`,
		jobID, task.TargetWorkerID, task.CatalogID, task.ManifestDigest, task.Action, licenseAccepted, formatTime(now), formatTime(now)); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return jobID, nil
}

// GetModelManagementTask returns one maintenance unit.
func (r *Repository) GetModelManagementTask(ctx context.Context, jobID string) (domain.ModelManagementTask, error) {
	var task domain.ModelManagementTask
	var license sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT job_id,target_worker_id,catalog_id,manifest_digest,action,license_accepted_at
		FROM model_management_tasks WHERE job_id=?`, jobID).
		Scan(&task.JobID, &task.TargetWorkerID, &task.CatalogID, &task.ManifestDigest, &task.Action, &license)
	if err != nil {
		return domain.ModelManagementTask{}, err
	}
	if license.Valid {
		task.LicenseAccepted = true
		task.LicenseAcceptedAt = license.String
	}
	return task, nil
}

// RecordModelAttestation records a successful authorized install or probe. Only
// a locked profile with a matching attestation is routable.
func (r *Repository) RecordModelAttestation(ctx context.Context, workerID, catalogID, manifestDigest, action, detailJSON string) error {
	if strings.TrimSpace(workerID) == "" || strings.TrimSpace(catalogID) == "" || strings.TrimSpace(manifestDigest) == "" {
		return fmt.Errorf("model attestation is incomplete")
	}
	if action != "install" && action != "probe" {
		return fmt.Errorf("unknown model attestation action %q", action)
	}
	if strings.TrimSpace(detailJSON) == "" {
		detailJSON = "{}"
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO model_attestations(worker_id,catalog_id,manifest_digest,action,verified_at,detail_json)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(worker_id,catalog_id,manifest_digest,action)
		DO UPDATE SET verified_at=excluded.verified_at, detail_json=excluded.detail_json`,
		workerID, catalogID, manifestDigest, action, formatTime(time.Now().UTC()), detailJSON)
	return err
}

// ModelAttestationExists answers whether a Worker proved it installed a given
// catalog artifact. Routing uses this, never the Worker's own claim.
func (r *Repository) ModelAttestationExists(ctx context.Context, workerID, catalogID, manifestDigest, action string) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, `
		SELECT 1 FROM model_attestations
		WHERE worker_id=? AND catalog_id=? AND manifest_digest=? AND action=?`,
		workerID, catalogID, manifestDigest, action).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
