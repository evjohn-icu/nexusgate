package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
)

func (r *Repository) EnqueueInferenceTask(ctx context.Context, jobID *string, task domain.InferenceTask, priority int) (string, error) {
	if strings.TrimSpace(task.TaskKey) == "" {
		return "", errors.New("inference task key is required")
	}
	if !task.Operation.Valid() {
		return "", fmt.Errorf("unsupported inference operation %q", task.Operation)
	}
	var request domain.InferenceRequest
	if err := json.Unmarshal([]byte(task.RequestJSON), &request); err != nil {
		return "", fmt.Errorf("decode inference request: %w", err)
	}
	if err := request.Validate(); err != nil {
		return "", fmt.Errorf("invalid inference request: %w", err)
	}
	if request.Operation != task.Operation || request.ConfigHash != task.ConfigHash || request.ModelRevision != task.ModelRevision {
		return "", errors.New("inference task identity does not match its request")
	}
	if request.Input.Kind == domain.InputKindAsset && request.Input.AssetID != task.AssetID {
		return "", errors.New("inference task asset does not match its request")
	}
	if request.Input.Kind != domain.InputKindAsset && task.AssetID != "" {
		return "", errors.New("assetless inference request cannot have an asset id")
	}

	now := time.Now().UTC()
	id := idgen.New()
	if jobID != nil {
		if strings.TrimSpace(*jobID) == "" {
			return "", errors.New("provided inference job id is empty")
		}
		id = *jobID
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at) VALUES(?,?,?,'pending',?,0,3,?,?,?,?)`, id, nullString(task.AssetID), string(domain.JobInference), priority, formatTime(now), task.TaskKey, formatTime(now), formatTime(now))
	if err != nil {
		return "", err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if inserted == 0 {
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT j.id FROM jobs j LEFT JOIN inference_tasks t ON t.job_id=j.id WHERE j.job_type=? AND ((t.task_key=? ) OR (j.asset_id IS NULL AND j.input_hash=?)) ORDER BY CASE WHEN t.task_key=? THEN 0 ELSE 1 END LIMIT 1`, string(domain.JobInference), task.TaskKey, task.TaskKey, task.TaskKey).Scan(&existing)
		if err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		return existing, nil
	}

	result, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO inference_tasks(job_id,task_key,operation,request_json,query_reference_id,config_hash,model_revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, task.TaskKey, string(task.Operation), task.RequestJSON, nullString(request.Input.ReferenceID), task.ConfigHash, task.ModelRevision, formatTime(now), formatTime(now))
	if err != nil {
		return "", err
	}
	inserted, err = result.RowsAffected()
	if err != nil {
		return "", err
	}
	if inserted == 0 {
		var existing string
		if err := tx.QueryRowContext(ctx, `SELECT job_id FROM inference_tasks WHERE task_key=?`, task.TaskKey).Scan(&existing); err != nil {
			return "", err
		}
		_ = tx.Rollback()
		return existing, nil
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (r *Repository) GetInferenceTask(ctx context.Context, jobID string) (domain.InferenceTask, error) {
	return getInferenceTask(ctx, r.db, jobID)
}

type inferenceTaskQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getInferenceTask(ctx context.Context, q inferenceTaskQueryer, jobID string) (domain.InferenceTask, error) {
	var task domain.InferenceTask
	var operation string
	var staged, modelRun, reference sql.NullString
	var cancelRequested int
	err := q.QueryRowContext(ctx, `SELECT t.job_id,COALESCE(j.asset_id,''),t.task_key,t.operation,t.request_json,t.model_run_id,t.query_reference_id,t.staged_json,t.cancel_requested,t.config_hash,t.model_revision,t.created_at,t.updated_at FROM inference_tasks t JOIN jobs j ON j.id=t.job_id WHERE t.job_id=? AND j.job_type=?`, jobID, string(domain.JobInference)).Scan(&task.JobID, &task.AssetID, &task.TaskKey, &operation, &task.RequestJSON, &modelRun, &reference, &staged, &cancelRequested, &task.ConfigHash, &task.ModelRevision, &task.CreatedAt, &task.UpdatedAt)
	if err != nil {
		return domain.InferenceTask{}, err
	}
	task.Operation = domain.InferenceOperation(operation)
	task.CancelRequested = cancelRequested != 0
	if modelRun.Valid {
		task.ModelRunID = modelRun.String
	}
	if reference.Valid {
		task.QueryReferenceID = reference.String
	}
	if staged.Valid {
		task.StagedJSON = staged.String
	}
	return task, nil
}

func (r *Repository) StageInferenceTask(ctx context.Context, jobID, owner string, result domain.InferenceResult) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var task domain.InferenceTask
	var operation, state, leaseOwner, leaseExpires string
	var assetID string
	err = tx.QueryRowContext(ctx, `SELECT t.job_id,COALESCE(j.asset_id,''),t.task_key,t.operation,t.request_json,t.config_hash,t.model_revision,j.state,COALESCE(j.lease_owner,''),COALESCE(j.lease_expires_at,'') FROM inference_tasks t JOIN jobs j ON j.id=t.job_id WHERE t.job_id=? AND j.job_type=?`, jobID, string(domain.JobInference)).Scan(&task.JobID, &assetID, &task.TaskKey, &operation, &task.RequestJSON, &task.ConfigHash, &task.ModelRevision, &state, &leaseOwner, &leaseExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return leaseLostErr(jobID, owner)
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if state != string(domain.JobRunning) || leaseOwner != owner {
		return leaseLostErr(jobID, owner)
	}
	expires, parseErr := time.Parse(time.RFC3339Nano, leaseExpires)
	if parseErr != nil || !expires.After(now) {
		return leaseLostErr(jobID, owner)
	}
	task.AssetID = assetID
	task.Operation = domain.InferenceOperation(operation)
	var request domain.InferenceRequest
	if err := json.Unmarshal([]byte(task.RequestJSON), &request); err != nil {
		return domain.Permanent(fmt.Errorf("stored inference request is unreadable: %w", err))
	}
	if err := request.Validate(); err != nil {
		return domain.Permanent(fmt.Errorf("stored inference request is invalid: %w", err))
	}
	if !storedTaskMatchesRequest(task, request) || (request.Input.Kind == domain.InputKindAsset && request.Input.AssetID != assetID) || (request.Input.Kind != domain.InputKindAsset && assetID != "") {
		return leaseLostErr(jobID, owner)
	}
	if result.OperationID != request.OperationID {
		return domain.Permanent(fmt.Errorf("result operation id %q does not match request %q", result.OperationID, request.OperationID))
	}
	if request.Operation == domain.OpSpeakerEmbedding {
		// The Hub-only encrypted biometric store is not part of this repository
		// boundary. Never place a speaker vector in staged_json or model_runs.
		return domain.Permanent(errors.New("speaker embeddings require encrypted biometric staging"))
	}
	dimension, err := embeddingDimension(ctx, tx, request.SpaceID)
	if err != nil {
		return err
	}
	if err := result.ValidateFor(request, request.Input.DurationMS, dimension); err != nil {
		return domain.Permanent(err)
	}
	staged, err := marshalStoredInferenceResult(result)
	if err != nil {
		return err
	}
	stageNow := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `UPDATE inference_tasks SET staged_json=?,updated_at=? WHERE job_id=? AND operation=? AND config_hash=? AND model_revision=? AND EXISTS(SELECT 1 FROM jobs j WHERE j.id=inference_tasks.job_id AND j.job_type=? AND j.state='running' AND j.lease_owner=? AND j.lease_expires_at>?)`, staged, formatTime(stageNow), jobID, string(request.Operation), request.ConfigHash, request.ModelRevision, string(domain.JobInference), owner, formatTime(stageNow))
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return leaseLostErr(jobID, owner)
	}
	return tx.Commit()
}

func (r *Repository) CommitInferenceTask(ctx context.Context, jobID, owner string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var task domain.InferenceTask
	var operation, state, leaseOwner string
	var expires sql.NullString
	var staged sql.NullString
	var modelRun sql.NullString
	var assetID string
	err = tx.QueryRowContext(ctx, `SELECT t.job_id,COALESCE(j.asset_id,''),t.task_key,t.operation,t.request_json,t.model_run_id,t.staged_json,t.config_hash,t.model_revision,j.state,COALESCE(j.lease_owner,''),j.lease_expires_at FROM inference_tasks t JOIN jobs j ON j.id=t.job_id WHERE t.job_id=? AND j.job_type=?`, jobID, string(domain.JobInference)).Scan(&task.JobID, &task.AssetID, &task.TaskKey, &operation, &task.RequestJSON, &modelRun, &staged, &task.ConfigHash, &task.ModelRevision, &state, &leaseOwner, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return leaseLostErr(jobID, owner)
	}
	if err != nil {
		return err
	}
	task.Operation = domain.InferenceOperation(operation)
	if modelRun.Valid {
		task.ModelRunID = modelRun.String
	}
	assetID = task.AssetID
	var request domain.InferenceRequest
	if err := json.Unmarshal([]byte(task.RequestJSON), &request); err != nil {
		return domain.Permanent(fmt.Errorf("stored inference request is unreadable: %w", err))
	}
	if err := request.Validate(); err != nil {
		return domain.Permanent(fmt.Errorf("stored inference request is invalid: %w", err))
	}
	if !storedTaskMatchesRequest(task, request) || (request.Input.Kind == domain.InputKindAsset && request.Input.AssetID != assetID) || (request.Input.Kind != domain.InputKindAsset && assetID != "") {
		return leaseLostErr(jobID, owner)
	}
	if state == string(domain.JobSucceeded) {
		// Commit's job transition and canonical writes are one transaction, so
		// an already-succeeded task is a safe same-identity lost-response retry.
		if staged.Valid {
			return tx.Commit()
		}
		return leaseLostErr(jobID, owner)
	}
	now := time.Now().UTC()
	leaseTime, parseErr := time.Parse(time.RFC3339Nano, expires.String)
	if state != string(domain.JobRunning) || leaseOwner != owner || !expires.Valid || parseErr != nil || !leaseTime.After(now) {
		return leaseLostErr(jobID, owner)
	}
	if !staged.Valid {
		return domain.Permanent(errors.New("inference task has no staged result"))
	}
	result, err := unmarshalStoredInferenceResult(staged.String)
	if err != nil {
		return domain.Permanent(fmt.Errorf("staged inference result is unreadable: %w", err))
	}
	if request.Operation == domain.OpSpeakerEmbedding {
		return domain.Permanent(errors.New("speaker embeddings require encrypted biometric staging"))
	}
	dimension, err := embeddingDimension(ctx, tx, request.SpaceID)
	if err != nil {
		return err
	}
	if err := result.ValidateFor(request, request.Input.DurationMS, dimension); err != nil {
		return domain.Permanent(err)
	}
	if request.Operation.NeedsSpace() && request.SpaceID != "" {
		if err := validatePersistedVectors(result.Embeddings, dimension); err != nil {
			return domain.Permanent(err)
		}
	}

	var modelRunID string
	var shotSetHash string
	if assetID != "" {
		if request.Input.Kind != domain.InputKindAsset {
			return domain.Permanent(errors.New("asset-bound inference task has a non-asset request"))
		}
		modelRunID, err = createOrReuseInferenceModelRun(ctx, tx, assetID, request, task.RequestJSON, result, jobID, owner, now)
		if err != nil {
			return err
		}
		if request.Operation == domain.OpVisualEmbedding {
			shotSetHash, err = shotSetHashTx(ctx, tx, assetID)
			if err != nil {
				return err
			}
			requestedShotSet := ""
			if request.Visual != nil {
				requestedShotSet = request.Visual.ShotSetHash
			}
			if requestedShotSet == "" || requestedShotSet != shotSetHash {
				return domain.Permanent(fmt.Errorf("visual shot set changed: request %q, current %q", requestedShotSet, shotSetHash))
			}
		}
	}

	generationID := ""
	if assetID != "" {
		generationID = idgen.New()
		processed := domain.IntervalSet(result.Coverage).Normalize()
		if processed == nil {
			processed = domain.IntervalSet{}
		}
		processedJSON, err := json.Marshal(processed)
		if err != nil {
			return err
		}
		failureJSON := []byte("[]")
		_, err = tx.ExecContext(ctx, `INSERT INTO modality_generations(id,asset_id,operation,model_run_id,input_hash,config_hash,space_id,shot_set_hash,state,processed_intervals_json,failure_intervals_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?, 'committed',?,?,?,?)`, generationID, assetID, string(request.Operation), nullString(modelRunID), request.InputHash, request.ConfigHash, nullString(request.SpaceID), nullString(shotSetHash), string(processedJSON), string(failureJSON), formatTime(now), formatTime(now))
		if err != nil {
			return err
		}
		if err := insertInferenceRows(ctx, tx, generationID, assetID, request, result, dimension, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO modality_heads(asset_id,operation,config_hash,generation_id,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(asset_id,operation,config_hash) DO UPDATE SET generation_id=excluded.generation_id,updated_at=excluded.updated_at`, assetID, string(request.Operation), request.ConfigHash, generationID, formatTime(now)); err != nil {
			return err
		}
	}
	if modelRunID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE inference_tasks SET model_run_id=?,updated_at=? WHERE job_id=? AND operation=? AND config_hash=? AND model_revision=?`, modelRunID, formatTime(now), jobID, string(request.Operation), request.ConfigHash, request.ModelRevision); err != nil {
			return err
		}
	}
	finalNow := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state='succeeded',terminal=0,last_error_code=NULL,last_error_message=NULL,current_stage='complete',progress=100,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND job_type=? AND state='running' AND lease_owner=? AND lease_expires_at>?`, formatTime(finalNow), jobID, string(domain.JobInference), owner, formatTime(finalNow))
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return leaseLostErr(jobID, owner)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// CommitSpeakerEmbedding publishes metadata for vectors already encrypted by
// the Hub secret store. It never accepts or serializes a plaintext vector.
func (r *Repository) CommitSpeakerEmbedding(ctx context.Context, jobID, owner string, consentGeneration, profileGeneration int64, records []domain.SpeakerEmbedding) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	var assetID, operation, requestJSON string
	var cancelRequested int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(j.asset_id,''),t.operation,t.request_json,t.cancel_requested FROM inference_tasks t JOIN jobs j ON j.id=t.job_id WHERE j.id=? AND j.job_type=? AND j.state='running' AND j.lease_owner=? AND j.lease_expires_at>?`, jobID, string(domain.JobInference), owner, formatTime(now)).Scan(&assetID, &operation, &requestJSON, &cancelRequested)
	if errors.Is(err, sql.ErrNoRows) {
		return leaseLostErr(jobID, owner)
	}
	if err != nil {
		return err
	}
	if cancelRequested != 0 {
		return domain.Permanent(errors.New("speaker embedding was cancelled"))
	}
	var request domain.InferenceRequest
	if err := json.Unmarshal([]byte(requestJSON), &request); err != nil {
		return domain.Permanent(fmt.Errorf("stored inference request is unreadable: %w", err))
	}
	if domain.InferenceOperation(operation) != domain.OpSpeakerEmbedding || request.Operation != domain.OpSpeakerEmbedding || request.Input.Kind != domain.InputKindAsset || request.Input.AssetID != assetID || request.Speaker == nil || strings.TrimSpace(request.Speaker.ProfileID) == "" || assetID == "" {
		return domain.Permanent(errors.New("speaker embedding request has no matching asset profile"))
	}
	if err := request.Validate(); err != nil {
		return domain.Permanent(fmt.Errorf("stored inference request is invalid: %w", err))
	}
	if len(records) == 0 {
		return domain.Permanent(errors.New("speaker embedding commit has no records"))
	}
	var enabled int
	var currentConsentGeneration int64
	err = tx.QueryRowContext(ctx, `SELECT enabled,generation FROM speaker_consent WHERE singleton=1`).Scan(&enabled, &currentConsentGeneration)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (enabled == 0 || currentConsentGeneration != consentGeneration) {
		return domain.Permanent(errors.New("speaker consent generation is no longer active"))
	}
	if err != nil {
		return err
	}
	var currentProfileGeneration int64
	var profileState string
	err = tx.QueryRowContext(ctx, `SELECT generation,state FROM speaker_profiles WHERE id=?`, request.Speaker.ProfileID).Scan(&currentProfileGeneration, &profileState)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (profileState != "active" || currentProfileGeneration != profileGeneration) {
		return domain.Permanent(errors.New("speaker profile generation is no longer active"))
	}
	if err != nil {
		return err
	}
	for index, record := range records {
		if strings.TrimSpace(record.SecretRef) == "" || record.ProfileID != request.Speaker.ProfileID || record.AssetID != assetID || record.SpaceID != request.SpaceID || record.ConsentGeneration != consentGeneration || record.ProfileGeneration != profileGeneration || strings.TrimSpace(record.GenerationID) == "" {
			return domain.Permanent(fmt.Errorf("speaker embedding record %d has mismatched identity or generation", index))
		}
		interval := domain.TimeInterval{StartMS: record.StartMS, EndMS: record.EndMS}
		if err := interval.Validate(request.Input.DurationMS); err != nil {
			return domain.Permanent(fmt.Errorf("speaker embedding record %d: %w", index, err))
		}
		if interval.StartMS < request.Speaker.Interval.StartMS || interval.EndMS > request.Speaker.Interval.EndMS {
			return domain.Permanent(fmt.Errorf("speaker embedding record %d is outside the requested interval", index))
		}
	}
	for _, record := range records {
		if _, err := tx.ExecContext(ctx, `INSERT INTO speaker_embeddings(id,profile_id,asset_id,generation_id,space_id,start_ms,end_ms,speech_duration_ms,quality_json,secret_ref,consent_generation,profile_generation,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, idgen.New(), record.ProfileID, record.AssetID, record.GenerationID, record.SpaceID, record.StartMS, record.EndMS, record.SpeechDurationMS, record.QualityJSON, record.SecretRef, record.ConsentGeneration, record.ProfileGeneration, formatTime(now)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM biometric_purge_refs WHERE secret_ref=?`, record.SecretRef); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM speaker_embedding_staging WHERE job_id=? AND operation_id=?`, jobID, request.OperationID); err != nil {
		return err
	}
	finalNow := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state='succeeded',terminal=0,last_error_code=NULL,last_error_message=NULL,current_stage='complete',progress=100,lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND job_type=? AND state='running' AND lease_owner=? AND lease_expires_at>?`, formatTime(finalNow), jobID, string(domain.JobInference), owner, formatTime(finalNow))
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return leaseLostErr(jobID, owner)
	}
	return tx.Commit()
}
func (r *Repository) CancelInferenceTask(ctx context.Context, jobID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := formatTime(time.Now().UTC())
	res, err := tx.ExecContext(ctx, `UPDATE inference_tasks SET cancel_requested=1,updated_at=? WHERE job_id=? AND EXISTS(SELECT 1 FROM jobs WHERE id=? AND job_type=?)`, now, jobID, jobID, string(domain.JobInference))
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state='failed',terminal=1,last_error_code='cancelled',last_error_message='cancelled',current_stage='cancelled',lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND job_type=? AND state='pending'`, now, jobID, string(domain.JobInference)); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) GetMultimodalSettings(ctx context.Context) (domain.MultimodalSettings, error) {
	var raw, updated string
	err := r.db.QueryRowContext(ctx, `SELECT settings_json,updated_at FROM multimodal_settings WHERE singleton=1`).Scan(&raw, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DefaultMultimodalSettings(), nil
	}
	if err != nil {
		return domain.MultimodalSettings{}, err
	}
	var settings domain.MultimodalSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return domain.MultimodalSettings{}, fmt.Errorf("decode multimodal settings: %w", err)
	}
	if settings.Enabled == nil {
		settings.Enabled = domain.DefaultMultimodalSettings().Enabled
	}
	if settings.Profiles == nil {
		settings.Profiles = map[string]string{}
	}
	settings.UpdatedAt = updated
	return settings, nil
}

func (r *Repository) PutMultimodalSettings(ctx context.Context, settings domain.MultimodalSettings) error {
	defaults := domain.DefaultMultimodalSettings()
	if settings.Enabled == nil {
		settings.Enabled = defaults.Enabled
	}
	if settings.Profiles == nil {
		settings.Profiles = map[string]string{}
	}
	for op := range settings.Enabled {
		if !op.Valid() {
			return fmt.Errorf("unknown inference operation in settings: %q", op)
		}
	}
	for op := range defaults.Enabled {
		if _, ok := settings.Enabled[op]; !ok {
			settings.Enabled[op] = false
		}
	}
	settings.UpdatedAt = formatTime(time.Now().UTC())
	encoded, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO multimodal_settings(singleton,settings_json,updated_at) VALUES(1,?,?) ON CONFLICT(singleton) DO UPDATE SET settings_json=excluded.settings_json,updated_at=excluded.updated_at`, string(encoded), settings.UpdatedAt)
	return err
}

func (r *Repository) UpsertEmbeddingSpace(ctx context.Context, space domain.EmbeddingSpace) error {
	id, err := domain.CanonicalSpaceID(space)
	if err != nil {
		return err
	}
	if space.ID != "" && space.ID != id {
		return fmt.Errorf("embedding space id %q does not match derived id %q", space.ID, id)
	}
	space.ID = id
	_, err = r.db.ExecContext(ctx, `INSERT INTO embedding_spaces(id,modality,provider,model_id,model_revision,dimension,normalization,preprocessing_json,runtime_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET modality=excluded.modality,provider=excluded.provider,model_id=excluded.model_id,model_revision=excluded.model_revision,dimension=excluded.dimension,normalization=excluded.normalization,preprocessing_json=excluded.preprocessing_json,runtime_digest=excluded.runtime_digest`, space.ID, string(space.Modality), space.Provider, space.ModelID, space.ModelRevision, space.Dimension, space.Normalization, space.PreprocessingJSON, space.RuntimeDigest, formatTime(time.Now().UTC()))
	return err
}

func (r *Repository) GetEmbeddingSpace(ctx context.Context, id string) (domain.EmbeddingSpace, error) {
	var space domain.EmbeddingSpace
	var modality string
	err := r.db.QueryRowContext(ctx, `SELECT id,modality,provider,model_id,model_revision,dimension,normalization,preprocessing_json,runtime_digest FROM embedding_spaces WHERE id=?`, id).Scan(&space.ID, &modality, &space.Provider, &space.ModelID, &space.ModelRevision, &space.Dimension, &space.Normalization, &space.PreprocessingJSON, &space.RuntimeDigest)
	if err != nil {
		return domain.EmbeddingSpace{}, err
	}
	space.Modality = domain.Modality(modality)
	return space, nil
}

func (r *Repository) ActiveGeneration(ctx context.Context, assetID string, op domain.InferenceOperation, configHash string) (domain.ModalityGeneration, error) {
	var generation domain.ModalityGeneration
	var operation, state, processed, failures string
	err := r.db.QueryRowContext(ctx, `SELECT g.id,g.asset_id,g.operation,COALESCE(g.model_run_id,''),g.input_hash,g.config_hash,COALESCE(g.space_id,''),COALESCE(g.shot_set_hash,''),g.state,g.processed_intervals_json,g.failure_intervals_json FROM modality_heads h JOIN modality_generations g ON g.id=h.generation_id WHERE h.asset_id=? AND h.operation=? AND h.config_hash=?`, assetID, string(op), configHash).Scan(&generation.ID, &generation.AssetID, &operation, &generation.ModelRunID, &generation.InputHash, &generation.ConfigHash, &generation.SpaceID, &generation.ShotSetHash, &state, &processed, &failures)
	if err != nil {
		return domain.ModalityGeneration{}, err
	}
	generation.Operation = domain.InferenceOperation(operation)
	generation.State = domain.GenerationState(state)
	if err := json.Unmarshal([]byte(processed), &generation.ProcessedIntervals); err != nil {
		return domain.ModalityGeneration{}, fmt.Errorf("decode processed intervals: %w", err)
	}
	if err := json.Unmarshal([]byte(failures), &generation.FailureIntervals); err != nil {
		return domain.ModalityGeneration{}, fmt.Errorf("decode failure intervals: %w", err)
	}
	return generation, nil
}

type storedInferenceResult struct {
	Result  domain.InferenceResult `json:"result"`
	Vectors [][]float32            `json:"vectors,omitempty"`
}

func marshalStoredInferenceResult(result domain.InferenceResult) (string, error) {
	stored := storedInferenceResult{Result: result}
	if len(result.Embeddings) != 0 {
		stored.Vectors = make([][]float32, len(result.Embeddings))
		for i := range result.Embeddings {
			stored.Vectors[i] = result.Embeddings[i].Vector
		}
	}
	encoded, err := json.Marshal(stored)
	return string(encoded), err
}

func unmarshalStoredInferenceResult(raw string) (domain.InferenceResult, error) {
	var stored storedInferenceResult
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return domain.InferenceResult{}, err
	}
	if len(stored.Vectors) != len(stored.Result.Embeddings) {
		if len(stored.Result.Embeddings) != 0 || len(stored.Vectors) != 0 {
			return domain.InferenceResult{}, errors.New("staged embedding vector count does not match records")
		}
	}
	for i := range stored.Result.Embeddings {
		stored.Result.Embeddings[i].Vector = stored.Vectors[i]
	}
	return stored.Result, nil
}

func storedTaskMatchesRequest(task domain.InferenceTask, request domain.InferenceRequest) bool {
	return task.Operation == request.Operation && task.ModelRevision == request.ModelRevision && task.ConfigHash == request.ConfigHash
}

func embeddingDimension(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, spaceID string) (int, error) {
	if spaceID == "" {
		return 0, nil
	}
	var dimension int
	err := q.QueryRowContext(ctx, `SELECT dimension FROM embedding_spaces WHERE id=?`, spaceID).Scan(&dimension)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return dimension, err
}

func validatePersistedVectors(records []domain.EmbeddingRecord, dimension int) error {
	for i, record := range records {
		if len(record.Vector) == 0 || (dimension > 0 && len(record.Vector) != dimension) {
			return fmt.Errorf("embedding %d has invalid dimension %d, want %d", i, len(record.Vector), dimension)
		}
		var norm float64
		for _, value := range record.Vector {
			v := float64(value)
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("embedding %d contains a non-finite value", i)
			}
			norm += v * v
		}
		if norm <= 0 || math.IsInf(norm, 0) || math.IsNaN(norm) {
			return fmt.Errorf("embedding %d has zero or non-finite norm", i)
		}
	}
	return nil
}

func shotSetHashTx(ctx context.Context, tx *sql.Tx, assetID string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,start_ms,end_ms FROM asset_shots WHERE asset_id=? ORDER BY start_ms,end_ms,id`, assetID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var canonical strings.Builder
	for rows.Next() {
		var id string
		var startMS, endMS int64
		if err := rows.Scan(&id, &startMS, &endMS); err != nil {
			return "", err
		}
		fmt.Fprintf(&canonical, "%s:%d:%d\n", id, startMS, endMS)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical.String()))
	return "shots-" + hex.EncodeToString(sum[:]), nil
}

func createOrReuseInferenceModelRun(ctx context.Context, tx *sql.Tx, assetID string, request domain.InferenceRequest, requestJSON string, result domain.InferenceResult, jobID, owner string, now time.Time) (string, error) {
	provider := "inference"
	if request.SpaceID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT provider FROM embedding_spaces WHERE id=?`, request.SpaceID).Scan(&provider); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	var runID, state string
	err := tx.QueryRowContext(ctx, `SELECT id,state FROM model_runs WHERE capability=? AND provider=? AND model=? AND input_hash=? AND prompt_version=? AND schema_version=? AND state!='failed' ORDER BY started_at,id LIMIT 1`, string(request.Operation), provider, request.ModelID, request.InputHash, request.ConfigHash, request.SchemaVersion).Scan(&runID, &state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if errors.Is(err, sql.ErrNoRows) {
		runID = idgen.New()
		res, insertErr := tx.ExecContext(ctx, `INSERT INTO model_runs(id,asset_id,capability,provider,model,input_hash,prompt_version,schema_version,state,request_json,started_at) SELECT ?,?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM jobs WHERE id=? AND asset_id=? AND job_type=? AND state='running' AND lease_owner=? AND lease_expires_at>?)`, runID, assetID, string(request.Operation), provider, request.ModelID, request.InputHash, request.ConfigHash, request.SchemaVersion, "running", requestJSON, formatTime(now), jobID, assetID, string(domain.JobInference), owner, formatTime(now))
		if insertErr != nil {
			return "", insertErr
		}
		count, affectedErr := res.RowsAffected()
		if affectedErr != nil {
			return "", affectedErr
		}
		if count != 1 {
			return "", leaseLostErr(jobID, owner)
		}
		state = "running"
	}
	if state == "committed" {
		return runID, nil
	}
	parsed, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	res, err := tx.ExecContext(ctx, `UPDATE model_runs SET state='validated',request_json=?,parsed_json=?,validation_errors=NULL,finished_at=? WHERE id=? AND asset_id=? AND state IN ('running','validated') AND EXISTS(SELECT 1 FROM jobs WHERE id=? AND asset_id=? AND job_type=? AND state='running' AND lease_owner=? AND lease_expires_at>?)`, requestJSON, string(parsed), formatTime(now), runID, assetID, jobID, assetID, string(domain.JobInference), owner, formatTime(now))
	if err != nil {
		return "", err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if count != 1 {
		return "", leaseLostErr(jobID, owner)
	}
	res, err = tx.ExecContext(ctx, `UPDATE model_runs SET state='committed',committed_at=? WHERE id=? AND asset_id=? AND state='validated' AND EXISTS(SELECT 1 FROM jobs WHERE id=? AND asset_id=? AND job_type=? AND state='running' AND lease_owner=? AND lease_expires_at>?)`, formatTime(now), runID, assetID, jobID, assetID, string(domain.JobInference), owner, formatTime(now))
	if err != nil {
		return "", err
	}
	count, err = res.RowsAffected()
	if err != nil {
		return "", err
	}
	if count != 1 {
		return "", leaseLostErr(jobID, owner)
	}
	return runID, nil
}

func insertInferenceRows(ctx context.Context, tx *sql.Tx, generationID, assetID string, request domain.InferenceRequest, result domain.InferenceResult, dimension int, now time.Time) error {
	timestamp := formatTime(now)
	switch request.Operation {
	case domain.OpSoundEvents:
		for _, event := range result.Events {
			if _, err := tx.ExecContext(ctx, `INSERT INTO audio_events(id,generation_id,asset_id,start_ms,end_ms,event_label,confidence,raw_score,evidence_status,boundary_resolution_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, idgen.New(), generationID, assetID, event.StartMS, event.EndMS, event.Label, nullableFloat64(event.Confidence), event.RawScore, event.EvidenceStatus, event.BoundaryResolutionMS, timestamp); err != nil {
				return err
			}
		}
	case domain.OpAudioEmbedding:
		for index, record := range result.Embeddings {
			if len(record.Vector) == 0 || (dimension > 0 && len(record.Vector) != dimension) || !validVector(record.Vector) {
				return fmt.Errorf("audio embedding %d has an invalid vector", index)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO audio_embeddings(generation_id,asset_id,space_id,start_ms,end_ms,vector,created_at) VALUES(?,?,?,?,?,?,?)`, generationID, assetID, request.SpaceID, record.StartMS, record.EndMS, encodeVectorBlob(record.Vector), timestamp); err != nil {
				return err
			}
		}
	case domain.OpVisualEmbedding:
		shots := make(map[string]domain.VisualShot, len(request.Visual.Shots))
		for _, shot := range request.Visual.Shots {
			shots[shot.ShotID] = shot
		}
		for index, record := range result.Embeddings {
			shot, exists := shots[record.ShotID]
			if !exists {
				return domain.Permanent(fmt.Errorf("visual embedding %d names a shot outside the request", index))
			}
			if record.SampleIndex >= 0 {
				if record.SampleIndex >= len(shot.TimestampsMS) || shot.TimestampsMS[record.SampleIndex] != record.TimestampMS {
					return domain.Permanent(fmt.Errorf("visual embedding %d has an unexpected sample", index))
				}
			}
			if len(record.Vector) == 0 || (dimension > 0 && len(record.Vector) != dimension) || !validVector(record.Vector) {
				return fmt.Errorf("visual embedding %d has an invalid vector", index)
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO visual_embeddings(generation_id,asset_id,shot_id,space_id,sample_index,timestamp_ms,strategy,source_hash,vector,created_at) SELECT ?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM asset_shots WHERE id=? AND asset_id=? AND start_ms=? AND end_ms=?)`, generationID, assetID, record.ShotID, request.SpaceID, record.SampleIndex, record.TimestampMS, record.Strategy, record.SourceHash, encodeVectorBlob(record.Vector), timestamp, record.ShotID, assetID, shot.StartMS, shot.EndMS)
			if err != nil {
				return err
			}
			count, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if count != 1 {
				return domain.Permanent(fmt.Errorf("visual embedding %d references a missing or changed shot", index))
			}
		}
	case domain.OpDiarization:
		for _, turn := range result.Turns {
			if _, err := tx.ExecContext(ctx, `INSERT INTO speaker_turns(id,generation_id,asset_id,start_ms,end_ms,local_speaker_id,confidence,overlap,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, idgen.New(), generationID, assetID, turn.StartMS, turn.EndMS, nullString(turn.LocalSpeakerID), nullableFloat64(turn.Confidence), boolInt(turn.Overlap), timestamp); err != nil {
				return err
			}
		}
	}
	return nil
}

func nullableFloat64(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
