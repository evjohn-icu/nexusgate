package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
	"github.com/evjohn-icu/nexusgate/internal/remote"
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
		return "", nil, leaseLostErr(jobID, workerID)
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
		return domain.DerivedArtifact{}, false, leaseLostErr(jobID, workerID)
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
		// Same predicate shape and same condition as every other lease check
		// in this file -- job_type narrows it to the operation's expected
		// stage, but a miss still means "this Worker does not hold the
		// job's lease right now." server.go already collapsed this and the
		// artifact/progress misses into one 409, so classify it with the
		// same sentinel rather than inventing a credential-specific one.
		return leaseLostErr(jobID, workerID)
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

func (r *Repository) LeaseNextWorkerDerive(ctx context.Context, worker remote.Worker, lease time.Duration, filter domain.LeaseFilter) (*remote.WorkerJob, error) {
	if strings.TrimSpace(worker.ID) == "" {
		return nil, errors.New("worker ID is required")
	}
	if !worker.Capabilities.Proxy && !worker.Capabilities.Thumbnail {
		return nil, nil
	}
	if len(worker.Capabilities.LibraryRoots) == 0 {
		return nil, nil
	}
	// Version gate: a worker whose binary predates the Hub's minimum
	// compatible version must never receive a lease. The version comes from
	// the workers row (enrollment + heartbeat), not from the lease request,
	// so the Worker cannot claim a fresher binary than it actually is. The
	// verdict lives in domain (not app) because this package must not import
	// app, and an unknown version is advisory (UpgradeRecommended), never a
	// refusal -- see domain.WorkerCompatibility.
	if domain.WorkerCompatibility(worker.Version).Verdict == domain.WorkerVerdictIncompatible {
		return nil, nil
	}
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(worker.Capabilities.LibraryRoots)), ",")
	args := make([]any, 0, len(worker.Capabilities.LibraryRoots)+4)
	for _, root := range worker.Capabilities.LibraryRoots {
		args = append(args, root)
	}
	args = append(args, string(domain.JobDerive), worker.ID)
	now := time.Now().UTC()
	args = append(args, formatTime(now), formatTime(now), filter.MaxAssetBytes, filter.MaxAssetBytes, worker.ID)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := reclaimExhaustedLeases(ctx, tx, now); err != nil {
		return nil, err
	}
	// j.state also admits 'running': a derive lease that expired mid-job --
	// the Worker died, or the encode simply outran lease_expires_at -- is
	// exactly as leasable as a job that never started. The
	// (j.lease_expires_at IS NULL OR j.lease_expires_at<=?) term below already
	// keeps a *live* running job out of the result, since pending/failed jobs
	// always carry a NULL lease_expires_at, so widening the state list costs
	// nothing for the ordinary case.
	//
	// The al.root_id IN (%s) slot is filled with placeholders produced by
	// strings.Repeat("?,", n) above: the format verb only expands '?'
	// characters.  Actual root-ID values arrive via the args slice as bound
	// parameters, so there is no SQL-injection path here.
	query := fmt.Sprintf(`SELECT j.id,j.asset_id,al.root_id,al.relative_path,al.modified_ns,j.job_type,j.attempt_count,j.max_attempts,j.current_stage,j.progress,COALESCE(j.preferred_worker_id,''),COALESCE(j.assigned_worker_id,''),COALESCE((SELECT a.file_size FROM assets a WHERE a.id=j.asset_id),0) FROM jobs j JOIN asset_locations al ON al.id=(SELECT el.id FROM asset_locations el JOIN library_roots er ON er.id=el.root_id WHERE el.asset_id=j.asset_id AND el.exists_now=1 AND er.health_state<>'unavailable' AND el.root_id IN (%s) ORDER BY el.is_primary DESC,el.last_seen_at DESC,er.created_at,er.id,el.relative_path,el.id LIMIT 1) WHERE j.job_type=? AND (j.assigned_worker_id IS NULL OR j.assigned_worker_id=?) AND j.state IN ('pending','failed','running') AND j.terminal=0 AND j.attempt_count<j.max_attempts AND j.run_after<=? AND (j.lease_expires_at IS NULL OR j.lease_expires_at<=?) AND (?=0 OR NOT EXISTS (SELECT 1 FROM assets a WHERE a.id=j.asset_id AND a.file_size>?)) ORDER BY CASE WHEN j.preferred_worker_id=? THEN 0 ELSE 1 END,j.priority DESC,j.created_at LIMIT 1`, placeholders)
	var job remote.WorkerJob
	err = tx.QueryRowContext(ctx, query, args...).Scan(&job.JobID, &job.AssetID, &job.RootID, &job.RelativePath, &job.ModifiedNS, &job.JobType, &job.AttemptCount, &job.MaxAttempts, &job.CurrentStage, &job.Progress, &job.PreferredWorkerID, &job.AssignedWorkerID, &job.SourceBytes)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing to lease, but reclaimExhaustedLeases above may still have
		// terminally failed an unrelated exhausted job in this same
		// transaction; that write has to survive, or a job that repeatedly
		// kills its Worker never actually goes terminal -- every attempt to
		// clean it up would find nothing to lease and roll itself back out.
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	// The extra "OR (state='running' AND lease_expires_at<=?)" arm is the
	// actual compare-and-swap for a reclaim: it re-checks expiry at UPDATE
	// time, not just at SELECT time, so a Worker that renews its lease (
	// RecordWorkerJobProgress) in the gap between this transaction's SELECT
	// and this UPDATE loses the race instead of having its live job stolen --
	// RowsAffected below still reports 0 either way, same as any other CAS
	// miss in this file.
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state='running',attempt_count=attempt_count+1,current_stage='derive',progress=0,lease_owner=?,lease_expires_at=?,updated_at=? WHERE id=? AND (state IN ('pending','failed') OR (state='running' AND lease_expires_at<=?)) AND (assigned_worker_id IS NULL OR assigned_worker_id=?)`, worker.ID, formatTime(now.Add(lease)), formatTime(now), job.JobID, formatTime(now), worker.ID)
	if err != nil {
		return nil, err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		// Lost the CAS race for this job, but reclaimExhaustedLeases's write
		// still needs to survive -- see the comment on the ErrNoRows branch
		// above.
		return nil, tx.Commit()
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

// workerLeaseRenewal is how far a progress report pushes lease_expires_at
// into the future. It is deliberately generous rather than tied to the
// original lease duration a Worker was granted: FFmpegDeriver.Derive (see
// internal/worker/runtime.go) is a single blocking call that produces
// thumbnail, proxy and audio together with no progress report in between, so
// the "derive started" report at 25% is the *last* renewal before the
// encode -- the one that actually has to survive it. A 4K software-x264
// proxy routinely runs past the old fixed 2-minute lease; this window has to
// comfortably outlast that or renewal buys nothing. It cannot outlast every
// possible encode (a huge or very long clip could still exceed it), but that
// is no longer catastrophic: reclaimExhaustedLeases and the widened lease
// predicates in LeaseNextWorkerDerive/LeaseNextJob mean an expired lease is
// retried with backoff instead of stranding the job.
const workerLeaseRenewal = 15 * time.Minute

// RecordWorkerJobProgress updates the current stage and appends an event only
// while workerID owns a live lease. This keeps progress reports from a stale
// or unrelated Worker from rewriting visible job state. It also renews the
// lease: without this, RecordWorkerJobProgress could faithfully report "25%,
// still going" right up until the fixed lease expired out from under a job
// that was never actually stuck.
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
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET current_stage=?,progress=?,lease_expires_at=?,updated_at=? WHERE id=? AND state='running' AND lease_owner=?`, stage, progress, formatTime(now.Add(workerLeaseRenewal)), formatTime(now), jobID, workerID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(id,job_id,worker_id,event_type,message,created_at,stage,progress) VALUES(?,?,?,?,?,?,?,?)`, `evt-`+jobID+"-"+fmt.Sprint(now.UnixNano()), jobID, workerID, event, message, formatTime(now), stage, progress); err != nil {
		return err
	}
	return tx.Commit()
}

// RetryWorkerJob records a failure and returns a live lease to the queue after
// delay. The last failure remains visible even after a later successful retry.
//
// This is the explicit-delay entry point; CompleteWorkerJob is the one thing
// that actually calls it in production (with a computed backoff, since the
// wire only ever reports a bare failure message) via recordWorkerJobFailure,
// which holds the shared UPDATE/event logic so the two do not drift apart.
func (r *Repository) RetryWorkerJob(ctx context.Context, jobID, workerID, stage, errorCode, errorMessage string, delay time.Duration) error {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(workerID) == "" || strings.TrimSpace(stage) == "" {
		return errors.New("job ID, worker ID and stage are required")
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
	if err := recordWorkerJobFailure(ctx, tx, jobID, workerID, stage, errorCode, errorMessage, attempts, maxAttempts, now, delay); err != nil {
		return err
	}
	return tx.Commit()
}

// workerRetryDelay mirrors app.retryDelay's schedule (1s, 2s, 4s, ... capped
// at 30s, per CLAUDE.md). It is redefined here rather than imported because
// this package is the repository app depends on -- importing app back would
// be a cycle -- and the backoff schedule is a policy constant shared by both
// call sites, not business logic that belongs to either one.
func workerRetryDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return time.Second
	}
	delay := time.Second << min(attempt-1, 5)
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

// recordWorkerJobFailure applies a Worker failure with the same retry
// budget/backoff RetryWorkerJob was written to give: attempts remaining get a
// delayed retry, attempts exhausted go terminal. It is shared by
// RetryWorkerJob and CompleteWorkerJob so that policy is defined once. The
// caller must have already verified jobID is currently leased by workerID
// (both current callers do, via an ownership-checked SELECT earlier in the
// same transaction); the UPDATE's own WHERE clause is the actual
// compare-and-swap and its RowsAffected is what is trusted.
//
// terminal=1 is set on exhaustion for the same reason FailJobTerminally sets
// it for the Hub-local case (repository.go): attempt_count<max_attempts
// already keeps an exhausted row unleasable, but terminal is what /progress
// and RequeueFailedJobs use to tell "ran out of attempts" apart from "still
// has budget."
func recordWorkerJobFailure(ctx context.Context, tx *sql.Tx, jobID, workerID, stage, errorCode, errorMessage string, attempts, maxAttempts int, now time.Time, delay time.Duration) error {
	if delay < 0 {
		delay = 0
	}
	nextState := domain.JobPending
	terminal := 0
	eventType := "retry_scheduled"
	retryable := 1
	runAfter := now.Add(delay)
	if attempts >= maxAttempts {
		nextState = domain.JobFailed
		terminal = 1
		eventType = "failed"
		retryable = 0
		runAfter = now
	}
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?,terminal=?,run_after=?,current_stage=?,progress=0,last_error_code=?,last_error_message=?,last_failure_at=?,last_failure_worker_id=?,last_failure_stage=?,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND state='running' AND lease_owner=?`, string(nextState), terminal, formatTime(runAfter), stage, nullString(errorCode), nullString(errorMessage), formatTime(now), workerID, stage, formatTime(now), jobID, workerID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return leaseLostErr(jobID, workerID)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO job_events(id,job_id,worker_id,event_type,message,created_at,stage,progress,error_code,retryable) VALUES(?,?,?,?,?,?,?,?,?,?)`, `evt-`+jobID+"-"+fmt.Sprint(now.UnixNano()), jobID, workerID, eventType, errorMessage, formatTime(now), stage, 0, errorCode, retryable)
	return err
}

// reclaimExhaustedLeases terminally fails a running job whose lease expired
// and whose attempt budget is spent. The reclaim predicates in LeaseNextJob
// and LeaseNextWorkerDerive already refuse to hand such a job back out
// (attempt_count<max_attempts excludes it), so without this a Worker (or the
// Hub) that keeps dying on the same job would leave it stuck at
// state='running' with a dead lease forever -- nothing else ever visits it
// again. There is deliberately no background sweeper for this: it runs
// inline, in the same transaction as every lease attempt, so the next Worker
// or Hub pipeline run that asks for any work cleans it up as a side effect.
func reclaimExhaustedLeases(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET state='failed',terminal=1,last_error_message=COALESCE(last_error_message,'worker lease expired after exhausting all attempts'),lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE state='running' AND lease_expires_at IS NOT NULL AND lease_expires_at<=? AND attempt_count>=max_attempts`, formatTime(now), formatTime(now))
	return err
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
		return fmt.Errorf("%w: derive job not found", domain.ErrInvalidAssignment)
	} else if err != nil {
		return err
	}
	if state == string(domain.JobRunning) {
		return fmt.Errorf("%w: cannot change worker assignment while job is running", domain.ErrJobNotAssignable)
	}
	if mode != remote.WorkerAssignmentAny {
		var status remote.WorkerStatus
		if err := tx.QueryRowContext(ctx, `SELECT status FROM workers WHERE id=?`, workerID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: worker not found", domain.ErrInvalidAssignment)
		} else if err != nil {
			return err
		} else if status == remote.WorkerRevoked {
			return fmt.Errorf("%w: cannot assign job to revoked worker", domain.ErrInvalidAssignment)
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
		return leaseLostErr(jobID, workerID)
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
		return remote.WorkerJobStatus{}, domain.ErrWorkerJobNotFound
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
	var attemptCount, maxAttempts int
	var jobType domain.JobType
	// lease_expires_at>? closes the asymmetry PrepareWorkerArtifact and
	// CommitWorkerArtifact already had: both refuse an upload once the lease
	// has expired (renewed on every RecordWorkerJobProgress call -- see
	// workerLeaseRenewal above for why that window is as wide as it is), but
	// this endpoint used to skip the expiry check entirely. A Worker whose
	// lease lapsed mid-derive could still call Complete(Succeeded) after
	// every one of its artifact uploads had already been rejected, recording
	// a "succeeded" derive with nothing behind it and letting the pipeline
	// chain (analyze/speech_gate) advance on that lie. Requiring the same
	// expiry here means a lapsed Worker now fails instead of silently
	// succeeding, and the widened reclaim predicates in LeaseNextJob /
	// LeaseNextWorkerDerive pick the job back up for a real attempt.
	err = tx.QueryRowContext(ctx, `SELECT asset_id,job_type,input_hash,current_stage,attempt_count,max_attempts FROM jobs WHERE id=? AND lease_owner=? AND state='running' AND lease_expires_at>?`, jobID, workerID, formatTime(now)).Scan(&assetID, &jobType, &inputHash, &currentStage, &attemptCount, &maxAttempts)
	if errors.Is(err, sql.ErrNoRows) {
		return leaseLostErr(jobID, workerID)
	}
	if err != nil {
		return err
	}
	if state == domain.JobFailed {
		// RetryWorkerJob was written to give a Worker failure the same
		// backoff a Hub-local provider failure gets (isRetryableJobError /
		// RetryJob in internal/app/pipeline.go), but nothing on the wire ever
		// called it: this endpoint is the only Worker failure path, and it
		// used to just set state='failed' with no run_after, so a failing
		// Worker re-leased and re-failed the same job up to three times back
		// to back with no delay at all. There is no structured error type to
		// classify the way isRetryableJobError does -- Complete only ever
		// receives a bare message string over the wire -- so attempt budget
		// is the only signal available here, which is exactly what
		// RetryWorkerJob already used.
		if err := recordWorkerJobFailure(ctx, tx, jobID, workerID, currentStage, "", message, attemptCount, maxAttempts, now, workerRetryDelay(attemptCount)); err != nil {
			return err
		}
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?,last_error_message=?,current_stage='complete',progress=100,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND lease_owner=? AND state='running' AND lease_expires_at>?`, string(state), nullString(message), formatTime(now), jobID, workerID, formatTime(now))
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return leaseLostErr(jobID, workerID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(id,job_id,worker_id,event_type,message,created_at,stage,progress) VALUES(?,?,?,?,?,?,?,?)`, `evt-`+jobID+"-complete-"+fmt.Sprint(now.UnixNano()), jobID, workerID, string(state), message, formatTime(now), "complete", 100); err != nil {
		return err
	}
	if jobType == domain.JobDerive {
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
