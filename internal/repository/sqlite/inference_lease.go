package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

func (r *Repository) LeaseNextWorkerInference(ctx context.Context, worker string, lease time.Duration, roots []string, ops []string, maxAssetBytes int64, now time.Time) (*remote.WorkerJob, error) {
	return r.leaseNextInference(ctx, worker, lease, roots, true, ops, maxAssetBytes, now)
}

func (r *Repository) LeaseNextLocalInference(ctx context.Context, worker string, lease time.Duration, ops []string, maxAssetBytes int64, now time.Time) (*remote.WorkerJob, error) {
	return r.leaseNextInference(ctx, worker, lease, nil, false, ops, maxAssetBytes, now)
}

func (r *Repository) leaseNextInference(ctx context.Context, worker string, lease time.Duration, roots []string, restrictRoots bool, ops []string, maxAssetBytes int64, now time.Time) (*remote.WorkerJob, error) {
	if strings.TrimSpace(worker) == "" {
		return nil, errors.New("inference lease owner is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if lease <= 0 {
		lease = 10 * time.Minute
	}
	if maxAssetBytes <= 0 {
		maxAssetBytes = 0
	}
	operations := make([]string, 0, len(ops))
	seen := make(map[string]struct{}, len(ops))
	for _, operation := range ops {
		op := domain.InferenceOperation(operation)
		if !op.Valid() {
			continue
		}
		if _, exists := seen[operation]; exists {
			continue
		}
		seen[operation] = struct{}{}
		operations = append(operations, operation)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := reclaimExhaustedLeases(ctx, tx, now); err != nil {
		return nil, err
	}
	if len(operations) == 0 {
		return nil, tx.Commit()
	}

	rootFilter := ""
	args := make([]any, 0, len(roots)+len(operations)+10)
	if restrictRoots {
		if len(roots) == 0 {
			rootFilter = " AND 0"
		} else {
			rootFilter = " AND el.root_id IN (" + placeholders(len(roots)) + ")"
			for _, root := range roots {
				args = append(args, root)
			}
		}
	}
	for _, operation := range operations {
		args = append(args, operation)
	}
	args = append(args, worker, formatTime(now), formatTime(now), maxAssetBytes, maxAssetBytes, worker)
	query := `SELECT j.id,COALESCE(j.asset_id,''),COALESCE(al.root_id,''),COALESCE(al.relative_path,''),COALESCE(al.modified_ns,0),j.job_type,j.attempt_count,j.max_attempts,j.current_stage,j.progress,COALESCE(j.preferred_worker_id,''),COALESCE(j.assigned_worker_id,''),COALESCE((SELECT a.file_size FROM assets a WHERE a.id=j.asset_id),0),t.operation,t.request_json FROM jobs j JOIN inference_tasks t ON t.job_id=j.id LEFT JOIN asset_locations al ON al.id=(SELECT el.id FROM asset_locations el JOIN library_roots lr ON lr.id=el.root_id WHERE el.asset_id=j.asset_id AND el.exists_now=1 AND lr.health_state<>'unavailable'` + rootFilter + ` ORDER BY el.is_primary DESC,el.last_seen_at DESC,lr.created_at,lr.id,el.relative_path,el.id LIMIT 1) WHERE j.job_type=? AND t.operation IN (` + placeholders(len(operations)) + `) AND t.cancel_requested=0 AND (j.assigned_worker_id IS NULL OR j.assigned_worker_id=?) AND j.state IN ('pending','failed','running') AND j.terminal=0 AND j.attempt_count<j.max_attempts AND j.run_after<=? AND (j.lease_expires_at IS NULL OR j.lease_expires_at<=?) AND (j.asset_id IS NULL OR al.id IS NOT NULL) AND (j.asset_id IS NULL OR ?=0 OR COALESCE((SELECT a.file_size FROM assets a WHERE a.id=j.asset_id),0)<=?) ORDER BY CASE WHEN j.preferred_worker_id=? THEN 0 ELSE 1 END,j.priority DESC,j.created_at,j.id LIMIT 1`
	// The first bound value is the job type; it follows any root parameters but
	// precedes the operation list in the SQL text.
	queryArgs := make([]any, 0, len(args)+1)
	rootCount := 0
	if restrictRoots && len(roots) != 0 {
		rootCount = len(roots)
	}
	queryArgs = append(queryArgs, args[:rootCount]...)
	queryArgs = append(queryArgs, string(domain.JobInference))
	queryArgs = append(queryArgs, args[rootCount:rootCount+len(operations)]...)
	queryArgs = append(queryArgs, args[rootCount+len(operations):]...)

	var job remote.WorkerJob
	var operation, requestJSON string
	err = tx.QueryRowContext(ctx, query, queryArgs...).Scan(&job.JobID, &job.AssetID, &job.RootID, &job.RelativePath, &job.ModifiedNS, &job.JobType, &job.AttemptCount, &job.MaxAttempts, &job.CurrentStage, &job.Progress, &job.PreferredWorkerID, &job.AssignedWorkerID, &job.SourceBytes, &operation, &requestJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	var request domain.InferenceRequest
	if err := json.Unmarshal([]byte(requestJSON), &request); err != nil {
		return nil, fmt.Errorf("decode request for inference job %s: %w", job.JobID, err)
	}
	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("invalid request for inference job %s: %w", job.JobID, err)
	}
	if string(request.Operation) != operation || request.Input.Kind == domain.InputKindAsset && request.Input.AssetID != job.AssetID || request.Input.Kind != domain.InputKindAsset && job.AssetID != "" {
		return nil, fmt.Errorf("inference job %s has inconsistent request identity", job.JobID)
	}
	if job.AssetID != "" {
		if job.RootID == "" || job.RelativePath == "" {
			return nil, fmt.Errorf("inference job %s has no usable asset location", job.JobID)
		}
		request.Input.RootID = job.RootID
		request.Input.RelativePath = job.RelativePath
	}

	deadline := now.Add(lease)
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state='running',attempt_count=attempt_count+1,current_stage='inference',progress=0,lease_owner=?,lease_expires_at=?,updated_at=? WHERE id=? AND job_type=? AND terminal=0 AND attempt_count<max_attempts AND run_after<=? AND (state IN ('pending','failed') OR (state='running' AND lease_expires_at<=?)) AND (assigned_worker_id IS NULL OR assigned_worker_id=?) AND EXISTS(SELECT 1 FROM inference_tasks t WHERE t.job_id=jobs.id AND t.cancel_requested=0 AND t.operation=?)`, worker, formatTime(deadline), formatTime(now), job.JobID, string(domain.JobInference), formatTime(now), formatTime(now), worker, operation)
	if err != nil {
		return nil, err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE inference_tasks SET updated_at=? WHERE job_id=?`, formatTime(now), job.JobID); err != nil {
		return nil, err
	}
	eventMessage := "worker inference lease"
	if !restrictRoots {
		eventMessage = "local inference lease"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(id,job_id,worker_id,event_type,message,created_at,stage,progress) SELECT ?,?,?,?,?,?,'inference',0 WHERE EXISTS(SELECT 1 FROM workers WHERE id=?)`, `evt-`+job.JobID+"-inference-"+fmt.Sprint(now.UnixNano()), job.JobID, worker, "leased", eventMessage, formatTime(now), worker); err != nil {
		return nil, err
	}
	job.AttemptCount++
	job.CurrentStage = "inference"
	job.Progress = 0
	job.Inference = &request
	job.LeaseDeadline = deadline
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

func (r *Repository) LeaseNextModelManagement(ctx context.Context, worker string, lease time.Duration, now time.Time) (*remote.WorkerJob, error) {
	if strings.TrimSpace(worker) == "" {
		return nil, errors.New("model management lease owner is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if lease <= 0 {
		lease = 10 * time.Minute
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := reclaimExhaustedLeases(ctx, tx, now); err != nil {
		return nil, err
	}
	var job remote.WorkerJob
	var action string
	err = tx.QueryRowContext(ctx, `SELECT j.id,COALESCE(j.asset_id,''),j.job_type,j.attempt_count,j.max_attempts,COALESCE(j.preferred_worker_id,''),COALESCE(j.assigned_worker_id,''),m.action FROM jobs j JOIN model_management_tasks m ON m.job_id=j.id WHERE j.job_type IN ('model_install','model_probe') AND ((j.job_type='model_install' AND m.action='install') OR (j.job_type='model_probe' AND m.action='probe')) AND m.target_worker_id=? AND m.cancel_requested=0 AND (j.assigned_worker_id IS NULL OR j.assigned_worker_id=?) AND j.state IN ('pending','failed','running') AND j.terminal=0 AND j.attempt_count<j.max_attempts AND j.run_after<=? AND (j.lease_expires_at IS NULL OR j.lease_expires_at<=?) ORDER BY CASE WHEN j.preferred_worker_id=? THEN 0 ELSE 1 END,j.priority DESC,j.created_at,j.id LIMIT 1`, worker, worker, formatTime(now), formatTime(now), worker).Scan(&job.JobID, &job.AssetID, &job.JobType, &job.AttemptCount, &job.MaxAttempts, &job.PreferredWorkerID, &job.AssignedWorkerID, &action)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	deadline := now.Add(lease)
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state='running',attempt_count=attempt_count+1,current_stage='model_management',progress=0,lease_owner=?,lease_expires_at=?,updated_at=? WHERE id=? AND job_type=? AND terminal=0 AND attempt_count<max_attempts AND run_after<=? AND (state IN ('pending','failed') OR (state='running' AND lease_expires_at<=?)) AND (assigned_worker_id IS NULL OR assigned_worker_id=?) AND EXISTS(SELECT 1 FROM model_management_tasks m WHERE m.job_id=jobs.id AND m.target_worker_id=? AND m.cancel_requested=0)`, worker, formatTime(deadline), formatTime(now), job.JobID, string(job.JobType), formatTime(now), formatTime(now), worker, worker)
	if err != nil {
		return nil, err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE model_management_tasks SET updated_at=? WHERE job_id=?`, formatTime(now), job.JobID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_events(id,job_id,worker_id,event_type,message,created_at,stage,progress) SELECT ?,?,?,?,?,?,? ,0 WHERE EXISTS(SELECT 1 FROM workers WHERE id=?)`, `evt-`+job.JobID+"-management-"+fmt.Sprint(now.UnixNano()), job.JobID, worker, "leased", "model management "+action, formatTime(now), "model_management", worker); err != nil {
		return nil, err
	}
	job.AttemptCount++
	job.CurrentStage = "model_management"
	job.Progress = 0
	job.LeaseDeadline = deadline
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

func placeholders(count int) string {
	return strings.TrimRight(strings.Repeat("?,", count), ",")
}
