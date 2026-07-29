package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ev/timingdex/internal/domain"
	"github.com/ev/timingdex/internal/idgen"
	"github.com/ev/timingdex/internal/remote"
)

// PrepareWorkerArtifact verifies the active derive lease before an upload is
// accepted and returns the asset target plus any previously recorded artifact.
// The lease check is repeated by CommitWorkerArtifact, because a worker may
// take a long time to upload a large proxy.
func (r *Repository) PrepareWorkerArtifact(ctx context.Context, jobID, workerID, artifactType, profileHash string) (string, *domain.DerivedArtifact, error) {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(workerID) == "" {
		return "", nil, errors.New("job ID and worker ID are required")
	}
	if strings.TrimSpace(artifactType) == "" || strings.TrimSpace(profileHash) == "" {
		return "", nil, errors.New("artifact type and profile hash are required")
	}
	now := formatTime(time.Now().UTC())
	var assetID string
	err := r.db.QueryRowContext(ctx, `SELECT asset_id FROM jobs WHERE id=? AND job_type=? AND state='running' AND lease_owner=? AND lease_expires_at>?`, jobID, string(domain.JobDerive), workerID, now).Scan(&assetID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, errors.New("worker does not own active job")
	}
	if err != nil {
		return "", nil, err
	}
	var artifact domain.DerivedArtifact
	err = r.db.QueryRowContext(ctx, `SELECT id,asset_id,artifact_type,profile_hash,local_path,size_bytes FROM derived_artifacts WHERE asset_id=? AND artifact_type=? AND profile_hash=?`, assetID, artifactType, profileHash).Scan(&artifact.ID, &artifact.AssetID, &artifact.Type, &artifact.ProfileHash, &artifact.LocalPath, &artifact.SizeBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return assetID, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	return assetID, &artifact, nil
}

// CommitWorkerArtifact atomically validates the current lease and persists an
// artifact record. When preserveExisting is true, an existing record wins so
// a retried upload cannot replace a valid result with partial or stale data.
func (r *Repository) CommitWorkerArtifact(ctx context.Context, jobID, workerID string, artifact domain.DerivedArtifact, preserveExisting bool) (domain.DerivedArtifact, bool, error) {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(workerID) == "" {
		return domain.DerivedArtifact{}, false, errors.New("job ID and worker ID are required")
	}
	if strings.TrimSpace(artifact.Type) == "" || strings.TrimSpace(artifact.ProfileHash) == "" {
		return domain.DerivedArtifact{}, false, errors.New("artifact type and profile hash are required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.DerivedArtifact{}, false, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var assetID string
	err = tx.QueryRowContext(ctx, `SELECT asset_id FROM jobs WHERE id=? AND job_type=? AND state='running' AND lease_owner=? AND lease_expires_at>?`, jobID, string(domain.JobDerive), workerID, formatTime(now)).Scan(&assetID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DerivedArtifact{}, false, errors.New("worker does not own active job")
	}
	if err != nil {
		return domain.DerivedArtifact{}, false, err
	}
	if artifact.AssetID != "" && artifact.AssetID != assetID {
		return domain.DerivedArtifact{}, false, errors.New("artifact asset does not match leased job")
	}
	artifact.AssetID = assetID
	var existing domain.DerivedArtifact
	err = tx.QueryRowContext(ctx, `SELECT id,asset_id,artifact_type,profile_hash,local_path,size_bytes FROM derived_artifacts WHERE asset_id=? AND artifact_type=? AND profile_hash=?`, assetID, artifact.Type, artifact.ProfileHash).Scan(&existing.ID, &existing.AssetID, &existing.Type, &existing.ProfileHash, &existing.LocalPath, &existing.SizeBytes)
	if err == nil && preserveExisting {
		if err := tx.Commit(); err != nil {
			return domain.DerivedArtifact{}, false, err
		}
		return existing, true, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.DerivedArtifact{}, false, err
	}
	if artifact.ID == "" {
		artifact.ID = idgen.New()
	}
	created := formatTime(now)
	_, err = tx.ExecContext(ctx, `INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(asset_id,artifact_type,profile_hash) DO UPDATE SET id=excluded.id,local_path=excluded.local_path,size_bytes=excluded.size_bytes,created_at=excluded.created_at`, artifact.ID, artifact.AssetID, artifact.Type, artifact.ProfileHash, artifact.LocalPath, artifact.SizeBytes, created)
	if err != nil {
		return domain.DerivedArtifact{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO job_events(id,job_id,worker_id,event_type,message,created_at) VALUES(?,?,?,?,?,?)`, `evt-`+jobID+"-artifact-"+fmt.Sprint(now.UnixNano()), jobID, workerID, "artifact_uploaded", artifact.Type+"/"+artifact.ProfileHash, created)
	if err != nil {
		return domain.DerivedArtifact{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.DerivedArtifact{}, false, err
	}
	return artifact, false, nil
}

// RecordProviderCredentialLease persists only the scope of a short-lived
// credential, never its API key or endpoint. The operation must match a job
// currently leased by the requesting Worker.
func (r *Repository) RecordProviderCredentialLease(ctx context.Context, jobID, workerID, provider, operation string, expiresAt time.Time) error {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(workerID) == "" || strings.TrimSpace(provider) == "" {
		return errors.New("job ID, worker ID and provider are required")
	}
	expectedType := map[string]domain.JobType{
		"video_analysis": domain.JobAnalyze,
		"asr":            domain.JobTranscribe,
	}[operation]
	if expectedType == "" {
		return fmt.Errorf("unsupported provider credential operation %q", operation)
	}
	now := time.Now().UTC()
	if expiresAt.IsZero() || !expiresAt.After(now) {
		return errors.New("credential lease expiry must be in the future")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owned int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id=? AND job_type=? AND state='running' AND lease_owner=? AND lease_expires_at>?`, jobID, string(expectedType), workerID, formatTime(now)).Scan(&owned)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("worker does not own active job for credential operation")
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO provider_credential_leases(id,job_id,worker_id,provider,operation,expires_at,issued_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(job_id,worker_id,provider,operation) DO UPDATE SET expires_at=excluded.expires_at,issued_at=excluded.issued_at`, idgen.New(), jobID, workerID, provider, operation, formatTime(expiresAt), formatTime(now))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) LeaseNextWorkerDerive(ctx context.Context, worker remote.Worker, lease time.Duration) (*remote.WorkerJob, error) {
	if strings.TrimSpace(worker.ID) == "" {
		return nil, errors.New("worker ID is required")
	}
	if !worker.Capabilities.Proxy && !worker.Capabilities.Thumbnail {
		return nil, nil
	}
	if len(worker.Capabilities.LibraryRoots) == 0 {
		return nil, nil
	}
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(worker.Capabilities.LibraryRoots)), ",")
	args := make([]any, 0, len(worker.Capabilities.LibraryRoots)+4)
	args = append(args, string(domain.JobDerive), worker.ID)
	for _, root := range worker.Capabilities.LibraryRoots {
		args = append(args, root)
	}
	now := time.Now().UTC()
	args = append(args, formatTime(now), formatTime(now), worker.ID)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`SELECT j.id,j.asset_id,al.root_id,al.relative_path,al.modified_ns,j.job_type,j.attempt_count,j.max_attempts,j.current_stage,j.progress,COALESCE(j.preferred_worker_id,''),COALESCE(j.assigned_worker_id,'') FROM jobs j JOIN asset_locations al ON al.asset_id=j.asset_id AND al.is_primary=1 AND al.exists_now=1 WHERE j.job_type=? AND (j.assigned_worker_id IS NULL OR j.assigned_worker_id=?) AND j.state IN ('pending','failed') AND j.terminal=0 AND j.attempt_count<j.max_attempts AND al.root_id IN (%s) AND j.run_after<=? AND (j.lease_expires_at IS NULL OR j.lease_expires_at<=?) ORDER BY CASE WHEN j.preferred_worker_id=? THEN 0 ELSE 1 END,j.priority DESC,j.created_at LIMIT 1`, placeholders)
	var job remote.WorkerJob
	err = tx.QueryRowContext(ctx, query, args...).Scan(&job.JobID, &job.AssetID, &job.RootID, &job.RelativePath, &job.ModifiedNS, &job.JobType, &job.AttemptCount, &job.MaxAttempts, &job.CurrentStage, &job.Progress, &job.PreferredWorkerID, &job.AssignedWorkerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state='running',attempt_count=attempt_count+1,current_stage='derive',progress=0,lease_owner=?,lease_expires_at=?,updated_at=? WHERE id=? AND state IN ('pending','failed') AND (assigned_worker_id IS NULL OR assigned_worker_id=?)`, worker.ID, formatTime(now.Add(lease)), formatTime(now), job.JobID, worker.ID)
	if err != nil {
		return nil, err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return nil, nil
	}
	job.AttemptCount++
	job.CurrentStage = "derive"
	job.Progress = 0
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(id,job_id,worker_id,event_type,message,created_at,stage,progress) SELECT ?,?,?,?, ?,?,'derive',0 WHERE EXISTS(SELECT 1 FROM workers WHERE id=?)`, `evt-`+job.JobID+"-"+fmt.Sprint(now.UnixNano()), job.JobID, worker.ID, "leased", "worker derive lease", formatTime(now), worker.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

func (r *Repository) RecordWorkerJobEvent(ctx context.Context, jobID, workerID, event, message string) error {
	if jobID == "" || workerID == "" || event == "" {
		return errors.New("job ID, worker ID and event are required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := assertActiveWorkerLease(ctx, tx, jobID, workerID); err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(id,job_id,worker_id,event_type,message,created_at) VALUES(?,?,?,?,?,?)`, `evt-`+jobID+"-"+fmt.Sprint(now.UnixNano()), jobID, workerID, event, message, formatTime(now)); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordWorkerJobProgress updates the current stage and appends an event only
// while workerID owns a live lease. This keeps progress reports from a stale
// or unrelated Worker from rewriting visible job state.
func (r *Repository) RecordWorkerJobProgress(ctx context.Context, jobID, workerID, stage string, progress float64, event, message string) error {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(workerID) == "" || strings.TrimSpace(stage) == "" || strings.TrimSpace(event) == "" {
		return errors.New("job ID, worker ID, stage and event are required")
	}
	if math.IsNaN(progress) || math.IsInf(progress, 0) || progress < 0 || progress > 100 {
		return errors.New("worker progress must be between 0 and 100")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := assertActiveWorkerLease(ctx, tx, jobID, workerID); err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET current_stage=?,progress=?,updated_at=? WHERE id=? AND state='running' AND lease_owner=?`, stage, progress, formatTime(now), jobID, workerID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(id,job_id,worker_id,event_type,message,created_at,stage,progress) VALUES(?,?,?,?,?,?,?,?)`, `evt-`+jobID+"-"+fmt.Sprint(now.UnixNano()), jobID, workerID, event, message, formatTime(now), stage, progress); err != nil {
		return err
	}
	return tx.Commit()
}

// RetryWorkerJob records a failure and returns a live lease to the queue after
// delay. The last failure remains visible even after a later successful retry.
func (r *Repository) RetryWorkerJob(ctx context.Context, jobID, workerID, stage, errorCode, errorMessage string, delay time.Duration) error {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(workerID) == "" || strings.TrimSpace(stage) == "" {
		return errors.New("job ID, worker ID and stage are required")
	}
	if delay < 0 {
		delay = 0
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := assertActiveWorkerLease(ctx, tx, jobID, workerID); err != nil {
		return err
	}
	now := time.Now().UTC()
	var attempts, maxAttempts int
	if err := tx.QueryRowContext(ctx, `SELECT attempt_count,max_attempts FROM jobs WHERE id=?`, jobID).Scan(&attempts, &maxAttempts); err != nil {
		return err
	}
	nextState := domain.JobState(domain.JobPending)
	eventType := "retry_scheduled"
	retryable := 1
	if attempts >= maxAttempts {
		nextState = domain.JobFailed
		eventType = "failed"
		retryable = 0
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?,run_after=?,current_stage=?,progress=0,last_error_code=?,last_error_message=?,last_failure_at=?,last_failure_worker_id=?,last_failure_stage=?,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND state='running' AND lease_owner=?`, string(nextState), formatTime(now.Add(delay)), stage, nullString(errorCode), nullString(errorMessage), formatTime(now), workerID, stage, formatTime(now), jobID, workerID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(id,job_id,worker_id,event_type,message,created_at,stage,progress,error_code,retryable) VALUES(?,?,?,?,?,?,?,?,?,?)`, `evt-`+jobID+"-"+fmt.Sprint(now.UnixNano()), jobID, workerID, eventType, errorMessage, formatTime(now), stage, 0, errorCode, retryable); err != nil {
		return err
	}
	return tx.Commit()
}

// SetDeriveWorkerAssignment changes manual routing only while a derive job is
// not running. Required assignments are enforced in the lease predicate;
// preferred assignments only affect ordering among capable Workers.
func (r *Repository) SetDeriveWorkerAssignment(ctx context.Context, jobID, workerID string, mode remote.WorkerAssignmentMode) error {
	if strings.TrimSpace(jobID) == "" {
		return errors.New("job ID is required")
	}
	if mode != remote.WorkerAssignmentAny && mode != remote.WorkerAssignmentPreferred && mode != remote.WorkerAssignmentRequired {
		return fmt.Errorf("unsupported worker assignment mode %q", mode)
	}
	if mode != remote.WorkerAssignmentAny && strings.TrimSpace(workerID) == "" {
		return errors.New("worker ID is required for a worker assignment")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=? AND job_type=?`, jobID, string(domain.JobDerive)).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return errors.New("derive job not found")
	} else if err != nil {
		return err
	}
	if state == string(domain.JobRunning) {
		return errors.New("cannot change worker assignment while job is running")
	}
	if mode != remote.WorkerAssignmentAny {
		var status remote.WorkerStatus
		if err := tx.QueryRowContext(ctx, `SELECT status FROM workers WHERE id=?`, workerID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
			return errors.New("worker not found")
		} else if err != nil {
			return err
		} else if status == remote.WorkerRevoked {
			return errors.New("cannot assign job to revoked worker")
		}
	}
	var preferred, assigned any
	switch mode {
	case remote.WorkerAssignmentPreferred:
		preferred = workerID
	case remote.WorkerAssignmentRequired:
		assigned = workerID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET preferred_worker_id=?,assigned_worker_id=?,updated_at=? WHERE id=?`, preferred, assigned, formatTime(time.Now().UTC()), jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func assertActiveWorkerLease(ctx context.Context, tx *sql.Tx, jobID, workerID string) error {
	var owned int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id=? AND state='running' AND lease_owner=? AND lease_expires_at>?`, jobID, workerID, formatTime(time.Now().UTC())).Scan(&owned)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("worker does not own active job")
	}
	return err
}

func (r *Repository) GetWorkerJobStatus(ctx context.Context, jobID string) (remote.WorkerJobStatus, error) {
	if strings.TrimSpace(jobID) == "" {
		return remote.WorkerJobStatus{}, errors.New("job ID is required")
	}
	var status remote.WorkerJobStatus
	var runAfter, leaseExpires, lastFailure string
	var leaseOwner, lastCode, lastMessage, lastWorker, lastStage, preferred, assigned string
	err := r.db.QueryRowContext(ctx, `SELECT id,COALESCE(asset_id,''),job_type,state,priority,attempt_count,max_attempts,run_after,COALESCE(lease_owner,''),COALESCE(lease_expires_at,''),current_stage,progress,COALESCE(last_error_code,''),COALESCE(last_error_message,''),COALESCE(last_failure_at,''),COALESCE(last_failure_worker_id,''),last_failure_stage,COALESCE(preferred_worker_id,''),COALESCE(assigned_worker_id,'') FROM jobs WHERE id=?`, jobID).Scan(&status.JobID, &status.AssetID, &status.JobType, &status.State, &status.Priority, &status.AttemptCount, &status.MaxAttempts, &runAfter, &leaseOwner, &leaseExpires, &status.CurrentStage, &status.Progress, &lastCode, &lastMessage, &lastFailure, &lastWorker, &lastStage, &preferred, &assigned)
	if errors.Is(err, sql.ErrNoRows) {
		return remote.WorkerJobStatus{}, errors.New("worker job not found")
	}
	if err != nil {
		return remote.WorkerJobStatus{}, err
	}
	status.RunAfter, _ = time.Parse(time.RFC3339Nano, runAfter)
	status.LeaseOwner = leaseOwner
	status.LeaseExpiresAt, _ = time.Parse(time.RFC3339Nano, leaseExpires)
	status.LastErrorCode = lastCode
	status.LastErrorMessage = lastMessage
	status.LastFailureAt, _ = time.Parse(time.RFC3339Nano, lastFailure)
	status.LastFailureWorkerID = lastWorker
	status.LastFailureStage = lastStage
	status.PreferredWorkerID = preferred
	status.AssignedWorkerID = assigned
	status.Events, err = r.listWorkerJobEvents(ctx, jobID)
	return status, err
}

func (r *Repository) listWorkerJobEvents(ctx context.Context, jobID string) ([]remote.WorkerJobEvent, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,job_id,COALESCE(worker_id,''),event_type,stage,progress,error_code,retryable,message,created_at FROM job_events WHERE job_id=? ORDER BY created_at,id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []remote.WorkerJobEvent
	for rows.Next() {
		var event remote.WorkerJobEvent
		var created string
		var retryable int
		if err := rows.Scan(&event.ID, &event.JobID, &event.WorkerID, &event.EventType, &event.Stage, &event.Progress, &event.ErrorCode, &retryable, &event.Message, &created); err != nil {
			return nil, err
		}
		event.Retryable = retryable != 0
		event.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (r *Repository) CompleteWorkerJob(ctx context.Context, jobID, workerID string, state domain.JobState, message string) error {
	if state != domain.JobSucceeded && state != domain.JobFailed {
		return errors.New("worker job state must be succeeded or failed")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var assetID, inputHash, currentStage string
	var currentProgress float64
	var jobType domain.JobType
	err = tx.QueryRowContext(ctx, `SELECT asset_id,job_type,input_hash,current_stage,progress FROM jobs WHERE id=? AND lease_owner=? AND state='running'`, jobID, workerID).Scan(&assetID, &jobType, &inputHash, &currentStage, &currentProgress)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("worker does not own active job")
	}
	if err != nil {
		return err
	}
	stage := currentStage
	progress := currentProgress
	if state == domain.JobSucceeded {
		stage = "complete"
		progress = 100
	} else {
		stage = "failed"
	}
	update := `UPDATE jobs SET state=?,last_error_message=?,current_stage=?,progress=?,lease_owner=NULL,lease_expires_at=NULL,updated_at=?`
	args := []any{string(state), nullString(message), stage, progress, formatTime(now)}
	if state == domain.JobFailed {
		update += `,last_error_code=NULL,last_failure_at=?,last_failure_worker_id=?,last_failure_stage=?`
		args = append(args, formatTime(now), workerID, stage)
	}
	update += ` WHERE id=? AND lease_owner=? AND state='running'`
	args = append(args, jobID, workerID)
	result, err := tx.ExecContext(ctx, update, args...)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return errors.New("worker does not own active job")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(id,job_id,worker_id,event_type,message,created_at,stage,progress) VALUES(?,?,?,?,?,?,?,?)`, `evt-`+jobID+"-complete-"+fmt.Sprint(now.UnixNano()), jobID, workerID, string(state), message, formatTime(now), stage, progress); err != nil {
		return err
	}
	if state == domain.JobSucceeded && jobType == domain.JobDerive {
		var audioExists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM derived_artifacts WHERE asset_id=? AND artifact_type='audio')`, assetID).Scan(&audioExists); err != nil {
			return err
		}
		nextType, nextHash, priority := domain.JobAnalyze, inputHash+"|worker-analyze-v1", 30
		if audioExists != 0 {
			nextType, nextHash, priority = domain.JobSpeechGate, inputHash+"|worker-speech-v1", 80
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at) VALUES(?,?,?,'pending',?,0,3,?,?,?,?)`, idgen.New(), assetID, string(nextType), priority, formatTime(now), nextHash, formatTime(now), formatTime(now)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(id,job_id,worker_id,event_type,message,created_at) VALUES(?,?,?,?,?,?)`, `evt-`+jobID+"-next-"+fmt.Sprint(now.UnixNano()), jobID, workerID, "workflow_enqueued", string(nextType), formatTime(now)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
