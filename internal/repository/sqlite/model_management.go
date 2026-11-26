package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

// localModelTarget is the stable maintenance target for the Hub's own device.
// It is deliberately independent of the process lease owner: Hub-local tasks
// stay leasable across restarts, and attestations for them key on this value.
const localModelTarget = "local"

// GetWorker returns one registered Worker row by id. It deliberately returns
// revoked rows too: callers that authorize maintenance work need to see the
// revocation, not have it look like an unknown Worker.
func (r *Repository) GetWorker(ctx context.Context, workerID string) (remote.Worker, error) {
	if strings.TrimSpace(workerID) == "" {
		return remote.Worker{}, errors.New("worker id is required")
	}
	var worker remote.Worker
	var rawCapabilities, lastSeen, created string
	err := r.db.QueryRowContext(ctx, `SELECT id,name,platform,version,status,capabilities_json,last_seen_at,created_at FROM workers WHERE id=?`, workerID).
		Scan(&worker.ID, &worker.Name, &worker.Platform, &worker.Version, &worker.Status, &rawCapabilities, &lastSeen, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return remote.Worker{}, fmt.Errorf("worker %s not found", workerID)
	}
	if err != nil {
		return remote.Worker{}, err
	}
	if err := json.Unmarshal([]byte(rawCapabilities), &worker.Capabilities); err != nil {
		return remote.Worker{}, err
	}
	worker.LastSeenAt, _ = time.Parse(time.RFC3339Nano, lastSeen)
	worker.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return worker, nil
}

// CompleteModelManagement records a successful authorized install or probe and
// moves its job to succeeded in one transaction. The caller passes the lease
// owner that currently holds the job; the Hub, never the Worker, writes the
// attestation from the task row it queued. Checks, in order:
//
//   - idempotence first: a job that already succeeded with the same
//     Worker/target, catalog, manifest digest and action attested returns
//     success again, so a Worker retrying after a lost HTTP response is not
//     punished for the Hub's own answer never arriving;
//   - job owner, unexpired lease, target and action pairing -- another Worker,
//     an expired lease or a revoked Worker cannot complete the task;
//   - the current maintenance grant on the target Worker still authorizes the
//     action, so work leased before a revocation is not attested after it;
//   - the task's manifest digest still matches the locked catalog entry, so a
//     lock that changed underneath the queue is never attested as the old
//     revision.
//
// Failed completions never route through here: a probe that reports an
// unavailable model is a failure record, not an attestation.
func (r *Repository) CompleteModelManagement(ctx context.Context, jobID, owner string) error {
	jobID, owner = strings.TrimSpace(jobID), strings.TrimSpace(owner)
	if jobID == "" || owner == "" {
		return errors.New("model management completion requires a job and lease owner")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var jobType domain.JobType
	var state string
	var leaseOwner, leaseExpires sql.NullString
	var target, catalogID, manifestDigest, action, completedBy string
	var cancelled bool
	err = tx.QueryRowContext(ctx, `SELECT j.job_type,j.state,j.lease_owner,j.lease_expires_at,m.target_worker_id,m.catalog_id,m.manifest_digest,m.action,m.completed_by,m.cancel_requested FROM jobs j JOIN model_management_tasks m ON m.job_id=j.id WHERE j.id=?`, jobID).
		Scan(&jobType, &state, &leaseOwner, &leaseExpires, &target, &catalogID, &manifestDigest, &action, &completedBy, &cancelled)
	if errors.Is(err, sql.ErrNoRows) {
		return leaseLostErr(jobID, owner)
	}
	if err != nil {
		return err
	}
	if (jobType != domain.JobModelInstall || action != "install") && (jobType != domain.JobModelProbe || action != "probe") {
		return leaseLostErr(jobID, owner)
	}
	if target != localModelTarget && owner != target {
		return leaseLostErr(jobID, owner)
	}
	if state == string(domain.JobSucceeded) {
		// Success already recorded for this exact identity: repeat or retried
		// completions are idempotent and must not rewrite the attestation.
		if completedBy != owner {
			return leaseLostErr(jobID, owner)
		}
		exists, err := attestationExists(ctx, tx, target, catalogID, manifestDigest, action)
		if err != nil {
			return err
		}
		if !exists {
			return leaseLostErr(jobID, owner)
		}
		return tx.Commit()
	}
	if cancelled || state != string(domain.JobRunning) || !leaseOwner.Valid || leaseOwner.String != owner {
		return leaseLostErr(jobID, owner)
	}
	if !leaseExpires.Valid {
		return leaseLostErr(jobID, owner)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, leaseExpires.String)
	if err != nil {
		return err
	}
	if !expiresAt.After(now) {
		return leaseLostErr(jobID, owner)
	}
	if target != localModelTarget {
		// The grant read here is the enrollment-time trust decision still in
		// force; a heartbeat can narrow availability but the durable grant is
		// what makes this completion authorized.
		var rawCapabilities, status string
		err := tx.QueryRowContext(ctx, `SELECT capabilities_json,status FROM workers WHERE id=?`, target).Scan(&rawCapabilities, &status)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && status == string(remote.WorkerRevoked)) {
			return leaseLostErr(jobID, owner)
		}
		if err != nil {
			return err
		}
		var capabilities remote.WorkerCapabilities
		if err := json.Unmarshal([]byte(rawCapabilities), &capabilities); err != nil {
			return err
		}
		if !slices.Contains(capabilities.ModelManagementOperations, action) {
			return leaseLostErr(jobID, owner)
		}
	}
	entry, ok := inferencecatalog.Lookup(catalogID)
	if !ok || entry.ManifestDigest() != manifestDigest {
		return fmt.Errorf("model management job %s no longer matches the locked catalog entry %q", jobID, catalogID)
	}
	detailJSON := "{}"
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state='succeeded',terminal=0,last_error_code=NULL,last_error_message=NULL,current_stage='complete',progress=100,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND job_type=? AND state='running' AND lease_owner=? AND lease_expires_at>?`, formatTime(now), jobID, string(jobType), owner, formatTime(now))
	if err != nil {
		return err
	}
	if count, err := res.RowsAffected(); err != nil {
		return err
	} else if count != 1 {
		return leaseLostErr(jobID, owner)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE model_management_tasks SET completed_by=?,updated_at=? WHERE job_id=?`, owner, formatTime(now), jobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO model_attestations(worker_id,catalog_id,manifest_digest,action,verified_at,detail_json) VALUES(?,?,?,?,?,?)
		ON CONFLICT(worker_id,catalog_id,manifest_digest,action)
		DO UPDATE SET verified_at=excluded.verified_at, detail_json=excluded.detail_json`,
		target, catalogID, manifestDigest, action, formatTime(now), detailJSON); err != nil {
		return err
	}
	// Local completions have no Worker row to reference; NULL keeps the
	// foreign key honest instead of naming a process owner that never was one.
	eventWorker := any(nil)
	if target != localModelTarget {
		eventWorker = target
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(id,job_id,worker_id,event_type,message,created_at,stage,progress) VALUES(?,?,?,?,?,?,?,?)`,
		`evt-`+jobID+"-model-management-"+fmt.Sprint(now.UnixNano()), jobID, eventWorker, string(domain.JobSucceeded), "model management "+action+" attested", formatTime(now), "complete", 100); err != nil {
		return err
	}
	return tx.Commit()
}

// attestationExists answers whether the exact Worker/target, catalog, digest
// and action tuple is already attested, inside the caller's transaction.
func attestationExists(ctx context.Context, tx *sql.Tx, workerID, catalogID, manifestDigest, action string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM model_attestations WHERE worker_id=? AND catalog_id=? AND manifest_digest=? AND action=?`, workerID, catalogID, manifestDigest, action).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

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
