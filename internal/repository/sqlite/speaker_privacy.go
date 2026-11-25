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
	"github.com/evjohn-icu/nexusgate/internal/idgen"
)

// GetSpeakerConsent returns the singleton opt-in row, defaulting to disabled
// generation 0 when the migration's seed row is missing.
func (r *Repository) GetSpeakerConsent(ctx context.Context) (domain.SpeakerConsent, error) {
	var consent domain.SpeakerConsent
	var enabled int
	var updated string
	err := r.db.QueryRowContext(ctx, `SELECT enabled,generation,updated_at FROM speaker_consent WHERE singleton=1`).
		Scan(&enabled, &consent.Generation, &updated)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.SpeakerConsent{Enabled: false}, nil
		}
		return domain.SpeakerConsent{}, err
	}
	consent.Enabled = enabled != 0
	consent.UpdatedAt = updated
	return consent, nil
}

// SetSpeakerConsent records the operator's decision and bumps the generation
// whenever the value actually changes. Every extraction captures the generation
// it started under; a mismatch at commit is a permanent rejection, so a late
// result can never be adopted by a newer consent state.
func (r *Repository) SetSpeakerConsent(ctx context.Context, enabled bool) (domain.SpeakerConsent, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.SpeakerConsent{}, err
	}
	defer tx.Rollback()
	now := formatTime(time.Now().UTC())
	var current bool
	var generation int64
	err = tx.QueryRowContext(ctx, `SELECT enabled,generation FROM speaker_consent WHERE singleton=1`).Scan(&current, &generation)
	if err != nil && err != sql.ErrNoRows {
		return domain.SpeakerConsent{}, err
	}
	if err == nil && current == enabled {
		if err := tx.Commit(); err != nil {
			return domain.SpeakerConsent{}, err
		}
		return domain.SpeakerConsent{Enabled: enabled, Generation: generation, UpdatedAt: now}, nil
	}
	generation++
	value := 0
	if enabled {
		value = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO speaker_consent(singleton,enabled,generation,updated_at) VALUES(1,?,?,?)
		ON CONFLICT(singleton) DO UPDATE SET enabled=excluded.enabled,generation=excluded.generation,updated_at=excluded.updated_at`,
		value, generation, now); err != nil {
		return domain.SpeakerConsent{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.SpeakerConsent{}, err
	}
	return domain.SpeakerConsent{Enabled: enabled, Generation: generation, UpdatedAt: now}, nil
}

// CreateSpeakerProfileWithTasks persists a profile, its sources and every
// extraction job/task in one transaction, gated by a consent-generation CAS:
// if speaker_consent is disabled, missing, or its generation has moved past
// consentGeneration since the caller captured it, nothing is written and
// domain.ErrSpeakerConsentGenerationInactive is returned. The tasks reuse the
// shared enqueue transaction, so request validation and task-key
// deduplication behave exactly like EnqueueInferenceTask; a duplicate key
// returns the existing job id instead of a second job.
func (r *Repository) CreateSpeakerProfileWithTasks(ctx context.Context, profile domain.SpeakerProfile, sources []domain.SpeakerProfileSource, tasks []domain.InferenceTask, priority int, consentGeneration int64) ([]string, error) {
	if strings.TrimSpace(profile.ID) == "" {
		return nil, fmt.Errorf("speaker profile id is required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var enabled int
	var generation int64
	err = tx.QueryRowContext(ctx, `SELECT enabled,generation FROM speaker_consent WHERE singleton=1`).Scan(&enabled, &generation)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (enabled == 0 || generation != consentGeneration)) {
		return nil, domain.ErrSpeakerConsentGenerationInactive
	}
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO speaker_profiles(id,generation,state,created_at) VALUES(?,?,'active',?)`,
		profile.ID, profile.Generation, formatTime(now)); err != nil {
		return nil, err
	}
	for _, source := range sources {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO speaker_profile_sources(profile_id,asset_id,diarization_generation_id,local_speaker_id) VALUES(?,?,?,?)`,
			profile.ID, source.AssetID, source.DiarizationGenerationID, source.LocalSpeakerID); err != nil {
			return nil, err
		}
	}
	jobIDs := make([]string, 0, len(tasks))
	for _, task := range tasks {
		id, err := enqueueInferenceTaskTx(ctx, tx, nil, task, priority, now)
		if err != nil {
			return nil, err
		}
		jobIDs = append(jobIDs, id)
	}
	return jobIDs, tx.Commit()
}

// DiarizationGenerationIdentity reports the committed generation's asset and
// operation, so profile creation can prove the requested generation really is
// this asset's committed diarization before any row exists. sql.ErrNoRows is
// returned for an unknown or not-yet-committed generation.
func (r *Repository) DiarizationGenerationIdentity(ctx context.Context, generationID string) (string, domain.InferenceOperation, error) {
	var assetID, operation string
	err := r.db.QueryRowContext(ctx, `SELECT asset_id,operation FROM modality_generations WHERE id=? AND state='committed'`, generationID).Scan(&assetID, &operation)
	if err != nil {
		return "", "", err
	}
	return assetID, domain.InferenceOperation(operation), nil
}

// ListSpeakerProfiles returns active profiles, never tombstoned ones.
func (r *Repository) ListSpeakerProfiles(ctx context.Context) ([]domain.SpeakerProfile, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT p.id,p.generation,s.asset_id,s.local_speaker_id
		FROM speaker_profiles p
		LEFT JOIN speaker_profile_sources s ON s.profile_id=p.id
		WHERE p.state='active'
		ORDER BY p.created_at,p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.SpeakerProfile{}
	for rows.Next() {
		var profile domain.SpeakerProfile
		var assetID, speaker sql.NullString
		if err := rows.Scan(&profile.ID, &profile.Generation, &assetID, &speaker); err != nil {
			return nil, err
		}
		profile.State = "active"
		profile.AssetID = assetID.String
		profile.LocalSpeakerID = speaker.String
		out = append(out, profile)
	}
	return out, rows.Err()
}

// DeleteSpeakerProfile tombstones the profile, bumps its generation, deletes its
// sources, embeddings, associations and staging rows, and returns every secret
// reference the caller must destroy. It is one transaction: a crash either
// leaves the profile intact or leaves the purge worklist populated, never a
// half-deleted profile with live ciphertext.
func (r *Repository) DeleteSpeakerProfile(ctx context.Context, profileID string) ([]string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := formatTime(time.Now().UTC())
	res, err := tx.ExecContext(ctx, `UPDATE speaker_profiles SET state='deleted',deleted_at=?,generation=generation+1 WHERE id=? AND state='active'`, now, profileID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("speaker profile %s is not active", profileID)
	}
	refs, err := collectSecretRefs(ctx, tx, `SELECT secret_ref FROM speaker_embeddings WHERE profile_id=?`, profileID)
	if err != nil {
		return nil, err
	}
	staged, err := collectSecretRefs(ctx, tx, `
		SELECT secret_ref FROM speaker_embedding_staging
		WHERE job_id IN (SELECT job_id FROM inference_tasks WHERE operation='speaker_embedding') AND profile_id=?`, profileID)
	if err != nil {
		return nil, err
	}
	// Flag and terminalize the extraction jobs while the staging rows the
	// predicate subquery needs are still present, using the shared
	// cancellation contract: cancel_requested is set atomically and every
	// nonterminal job moves to terminal failed/cancelled with no lease.
	if _, _, err := cancelInferenceTasksTx(ctx, tx, time.Now().UTC(), "speaker profile deleted",
		`operation='speaker_embedding' AND (json_extract(request_json,'$.speaker.profile_id')=? OR job_id IN
		     (SELECT job_id FROM speaker_embedding_staging WHERE profile_id=?))`,
		profileID, profileID); err != nil {
		return nil, err
	}
	for _, ref := range append(refs, staged...) {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO biometric_purge_refs(secret_ref,created_at) VALUES(?,?)`, ref, now); err != nil {
			return nil, err
		}
	}
	for _, query := range []string{
		`DELETE FROM speaker_profile_sources WHERE profile_id=?`,
		`DELETE FROM speaker_embeddings WHERE profile_id=?`,
		`DELETE FROM speaker_associations WHERE left_profile_id=? OR right_profile_id=?`,
		`DELETE FROM speaker_embedding_staging WHERE profile_id=?`,
	} {
		args := []any{profileID}
		if strings.Contains(query, "OR right_profile_id") {
			args = append(args, profileID)
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return append(refs, staged...), nil
}

func collectSecretRefs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// AddSpeakerEmbedding records one encrypted speaker vector reference.
func (r *Repository) AddSpeakerEmbedding(ctx context.Context, record domain.SpeakerEmbedding) error {
	if strings.TrimSpace(record.SecretRef) == "" {
		return fmt.Errorf("speaker embedding needs a secret reference")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO speaker_embeddings(id,profile_id,asset_id,generation_id,space_id,start_ms,end_ms,speech_duration_ms,quality_json,secret_ref,consent_generation,profile_generation,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		idgen.New(), record.ProfileID, record.AssetID, record.GenerationID, record.SpaceID,
		record.StartMS, record.EndMS, record.SpeechDurationMS, record.QualityJSON,
		record.SecretRef, record.ConsentGeneration, record.ProfileGeneration,
		formatTime(time.Now().UTC()))
	return err
}

// ListSpeakerEmbeddings returns the references for one profile. The ciphertext
// stays in the secret store; this only names it.
func (r *Repository) ListSpeakerEmbeddings(ctx context.Context, profileID string) ([]domain.SpeakerEmbedding, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id,profile_id,asset_id,generation_id,space_id,start_ms,end_ms,speech_duration_ms,quality_json,secret_ref,consent_generation,profile_generation
		FROM speaker_embeddings WHERE profile_id=? ORDER BY created_at,id`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.SpeakerEmbedding{}
	for rows.Next() {
		var record domain.SpeakerEmbedding
		if err := rows.Scan(&record.ID, &record.ProfileID, &record.AssetID, &record.GenerationID, &record.SpaceID,
			&record.StartMS, &record.EndMS, &record.SpeechDurationMS, &record.QualityJSON,
			&record.SecretRef, &record.ConsentGeneration, &record.ProfileGeneration); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// UpsertSpeakerAssociation writes a candidate pairing. The CHECK on ordering
// means callers must pass the lower profile id first; this helper enforces it.
//
// A similarity refresh never overrides an operator's decision: a pairing the
// user confirmed or rejected keeps its state even when a new match round
// recomputes its similarity, because only the explicitly authorized decision
// methods (SetSpeakerAssociationState, SplitSpeakerCluster) change state.
func (r *Repository) UpsertSpeakerAssociation(ctx context.Context, association domain.SpeakerAssociation) error {
	left, right := association.LeftProfileID, association.RightProfileID
	if left > right {
		left, right = right, left
	}
	now := formatTime(time.Now().UTC())
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO speaker_associations(id,left_profile_id,right_profile_id,state,similarity,calibration_id,consent_generation,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(left_profile_id,right_profile_id) DO UPDATE SET
			state=CASE
				WHEN speaker_associations.state IN ('confirmed_by_user','rejected')
				THEN speaker_associations.state
				ELSE excluded.state
			END,
			similarity=excluded.similarity,
			calibration_id=excluded.calibration_id, consent_generation=excluded.consent_generation,
			updated_at=excluded.updated_at`,
		idgen.New(), left, right, association.State, association.Similarity,
		nullString(association.CalibrationID), association.ConsentGeneration, now, now)
	return err
}

// ListSpeakerAssociations returns every association touching a profile.
func (r *Repository) ListSpeakerAssociations(ctx context.Context, profileID string) ([]domain.SpeakerAssociation, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id,left_profile_id,right_profile_id,state,similarity,COALESCE(calibration_id,''),consent_generation
		FROM speaker_associations WHERE left_profile_id=? OR right_profile_id=?`, profileID, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.SpeakerAssociation{}
	for rows.Next() {
		var association domain.SpeakerAssociation
		if err := rows.Scan(&association.ID, &association.LeftProfileID, &association.RightProfileID,
			&association.State, &association.Similarity, &association.CalibrationID, &association.ConsentGeneration); err != nil {
			return nil, err
		}
		out = append(out, association)
	}
	return out, rows.Err()
}

// SetSpeakerAssociationState updates one association's state.
func (r *Repository) SetSpeakerAssociationState(ctx context.Context, id, state string) error {
	switch state {
	case domain.AssociationConfirmed, domain.AssociationRejected:
	default:
		return fmt.Errorf("association state %q cannot be set by an operator", state)
	}
	res, err := r.db.ExecContext(ctx, `UPDATE speaker_associations SET state=?,updated_at=? WHERE id=?`, state, formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("association %s not found", id)
	}
	return nil
}

// AddPurgeRef appends a secret reference to the deletion worklist. It runs
// before the ciphertext exists, so a crash between writing the secret and
// publishing the row still leaves a deletion obligation.
func (r *Repository) AddPurgeRef(ctx context.Context, secretRef string) error {
	if strings.TrimSpace(secretRef) == "" {
		return fmt.Errorf("purge reference is empty")
	}
	_, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO biometric_purge_refs(secret_ref,created_at) VALUES(?,?)`,
		secretRef, formatTime(time.Now().UTC()))
	return err
}

// biometricPurgePageSize is the bounded batch the purge drain reads per pass.
const biometricPurgePageSize = 256

// ListPurgeRefs returns the next bounded page of the pending deletion worklist
// in stable secret_ref order. after is the last reference of the previous page
// ("" for the first). Keyset pagination matters because a failed delete must
// never stall the rows after it: the caller advances the keyset across
// failures instead of retrying the same failing head of the list forever.
func (r *Repository) ListPurgeRefs(ctx context.Context, after string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = biometricPurgePageSize
	}
	rows, err := r.db.QueryContext(ctx, `SELECT secret_ref FROM biometric_purge_refs WHERE ?='' OR secret_ref>? ORDER BY secret_ref LIMIT ?`,
		after, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// ClassifyBiometricPurgeRefs explains what the purge drain may do with the
// given queued references. It reads live state, never the queue's word: a
// canonical ciphertext stays while a committed embedding row names it, a
// staged extraction with unrevoked consent and an active profile keeps its
// ciphertext until its commit, and a staging row whose job is no longer live
// is obsolete metadata that goes away together with the obligation.
func (r *Repository) ClassifyBiometricPurgeRefs(ctx context.Context, refs []string) (map[string]domain.BiometricPurgeRefDisposition, error) {
	out := make(map[string]domain.BiometricPurgeRefDisposition, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(refs)), ",")
	args := make([]any, len(refs))
	for i, ref := range refs {
		args[i] = ref
	}
	for _, statement := range []struct {
		query string
		set   func(*domain.BiometricPurgeRefDisposition)
	}{
		{
			query: `SELECT secret_ref FROM speaker_embeddings WHERE secret_ref IN (` + marks + `)`,
			set:   func(d *domain.BiometricPurgeRefDisposition) { d.Canonical = true },
		},
		{
			query: `SELECT s.secret_ref FROM speaker_embedding_staging s WHERE s.secret_ref IN (` + marks + `)`,
			set:   func(d *domain.BiometricPurgeRefDisposition) { d.Staged = true },
		},
		{
			query: `SELECT s.secret_ref
				FROM speaker_embedding_staging s
				JOIN jobs j ON j.id=s.job_id AND j.job_type='inference' AND j.terminal=0 AND j.state IN ('pending','running')
				JOIN speaker_consent c ON c.singleton=1 AND c.enabled=1
				JOIN speaker_profiles p ON p.id=s.profile_id AND p.state='active'
				WHERE s.secret_ref IN (` + marks + `)`,
			set: func(d *domain.BiometricPurgeRefDisposition) { d.LiveStaging = true },
		},
	} {
		rows, err := r.db.QueryContext(ctx, statement.query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var ref string
			if err := rows.Scan(&ref); err != nil {
				rows.Close()
				return nil, err
			}
			disposition := out[ref]
			statement.set(&disposition)
			out[ref] = disposition
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// ClearPurgeRef removes one completed deletion.
func (r *Repository) ClearPurgeRef(ctx context.Context, secretRef string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM biometric_purge_refs WHERE secret_ref=?`, secretRef)
	return err
}

// RecordPurgeFailure keeps the worklist entry and records why it failed.
func (r *Repository) RecordPurgeFailure(ctx context.Context, secretRef, code string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE biometric_purge_refs SET last_error_code=? WHERE secret_ref=?`, code, secretRef)
	return err
}

// ClearStagingBySecretRef removes the staging row that names a reference, used
// by the purge drain when that staging row's job is cancelled or terminal and
// the ciphertext it named was just deleted. The staging row of a live job is
// never touched here.
func (r *Repository) ClearStagingBySecretRef(ctx context.Context, secretRef string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM speaker_embedding_staging WHERE secret_ref=?`, secretRef)
	return err
}

// PutSpeakerEmbeddingStaging records a staged biometric extraction with the
// digest identity of its staged bytes. A conflict on operation_id repairs the
// row from a newer valid submission: the fresh secret reference and digest
// replace the old ones, and the superseded reference's deletion obligation is
// still on the worklist from its own stage.
func (r *Repository) PutSpeakerEmbeddingStaging(ctx context.Context, staging domain.SpeakerEmbeddingStaging) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO speaker_embedding_staging(operation_id,job_id,profile_id,profile_generation,consent_generation,secret_ref,result_digest,created_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(operation_id) DO UPDATE SET secret_ref=excluded.secret_ref,result_digest=excluded.result_digest`,
		staging.OperationID, staging.JobID, staging.ProfileID, staging.ProfileGeneration,
		staging.ConsentGeneration, staging.SecretRef, nullString(staging.ResultDigest), formatTime(time.Now().UTC()))
	return err
}

// GetSpeakerEmbeddingStaging returns one staged extraction by operation id.
func (r *Repository) GetSpeakerEmbeddingStaging(ctx context.Context, operationID string) (domain.SpeakerEmbeddingStaging, error) {
	var staging domain.SpeakerEmbeddingStaging
	var digest sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT operation_id,job_id,profile_id,profile_generation,consent_generation,secret_ref,result_digest
		FROM speaker_embedding_staging WHERE operation_id=?`, operationID).
		Scan(&staging.OperationID, &staging.JobID, &staging.ProfileID, &staging.ProfileGeneration,
			&staging.ConsentGeneration, &staging.SecretRef, &digest)
	if err == nil {
		staging.ResultDigest = digest.String
	}
	return staging, err
}

// SpeakerResultCommitState reports the pre-commit view of one speaker job: the
// job state, whether owner still holds an unexpired running lease, the task's
// cancellation flag, and the result digest recorded at commit time. It is a
// read-only optimization ahead of the encrypted write — the commit
// transaction's own compare-and-swap remains the only authority that
// publishes.
func (r *Repository) SpeakerResultCommitState(ctx context.Context, jobID, owner string) (domain.SpeakerResultCommitState, error) {
	var state domain.SpeakerResultCommitState
	var leaseOwner, leaseExpires string
	var digest sql.NullString
	var cancelRequested int
	err := r.db.QueryRowContext(ctx, `
		SELECT j.state,COALESCE(j.lease_owner,''),COALESCE(j.lease_expires_at,''),t.cancel_requested,t.result_digest
		FROM jobs j JOIN inference_tasks t ON t.job_id=j.id
		WHERE j.id=? AND j.job_type=?`, jobID, string(domain.JobInference)).
		Scan(&state.State, &leaseOwner, &leaseExpires, &cancelRequested, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SpeakerResultCommitState{}, fmt.Errorf("inference job %s: %w", jobID, domain.ErrJobLeaseLost)
	}
	if err != nil {
		return domain.SpeakerResultCommitState{}, err
	}
	if digest.Valid {
		state.CommittedDigest = digest.String
	}
	state.CancelRequested = cancelRequested != 0
	if state.State == string(domain.JobRunning) && leaseOwner == owner {
		expires, parseErr := time.Parse(time.RFC3339Nano, leaseExpires)
		state.LeaseValid = parseErr == nil && expires.After(time.Now().UTC())
	}
	return state, nil
}

// CommitSpeakerEmbedding publishes metadata for vectors already encrypted by
// the Hub secret store. It never accepts or serializes a plaintext vector.
//
// digest is the opaque one-way identity the Hub computed from the result it
// received (remote.DigestResult). The transaction refuses to publish anything
// the Hub did not stage under exactly this digest: the staging row for this
// job and operation must exist, must carry the same digest, and every record
// must name the staged ciphertext. A job that already succeeded under the same
// digest is a read-only idempotent success — a lost HTTP response must not
// stage or publish a second vector — while a different or legacy-unknown
// digest is rejected without any mutation. Lease ownership is re-checked by
// the final UPDATE, so only the executor that holds the lease can drive the
// transition.
func (r *Repository) CommitSpeakerEmbedding(ctx context.Context, jobID, owner string, consentGeneration, profileGeneration int64, digest string, records []domain.SpeakerEmbedding) error {
	if strings.TrimSpace(digest) == "" {
		return domain.Permanent(errors.New("speaker embedding commit has no result digest"))
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	var assetID, operation, requestJSON string
	var cancelRequested int
	var committedDigest sql.NullString
	var state, leaseOwner, leaseExpires string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(j.asset_id,''),t.operation,t.request_json,t.cancel_requested,t.result_digest,j.state,COALESCE(j.lease_owner,''),COALESCE(j.lease_expires_at,'') FROM inference_tasks t JOIN jobs j ON j.id=t.job_id WHERE j.id=? AND j.job_type=?`, jobID, string(domain.JobInference)).Scan(&assetID, &operation, &requestJSON, &cancelRequested, &committedDigest, &state, &leaseOwner, &leaseExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return leaseLostErr(jobID, owner)
	}
	if err != nil {
		return err
	}
	if state == string(domain.JobSucceeded) {
		// The job transition and the canonical writes are one transaction, so
		// an already-succeeded task whose recorded digest matches is a safe
		// same-identity lost-response retry. This branch writes nothing: a
		// different executor, or a different result under the same operation
		// id, can never mutate rows here.
		if committedDigest.Valid && committedDigest.String == digest {
			return nil
		}
		return domain.Permanent(errors.New("speaker result digest does not match the committed result"))
	}
	if cancelRequested != 0 {
		if err := failCancelledInferenceTx(ctx, tx, jobID, owner); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return domain.Permanent(domain.ErrInferenceCancelled)
	}
	if state != string(domain.JobRunning) {
		return leaseLostErr(jobID, owner)
	}
	expires, parseErr := time.Parse(time.RFC3339Nano, leaseExpires)
	if leaseOwner != owner || parseErr != nil || !expires.After(now) {
		return leaseLostErr(jobID, owner)
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
		return domain.Permanent(domain.ErrSpeakerConsentGenerationInactive)
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
	var stagedRef, stagedDigest string
	err = tx.QueryRowContext(ctx, `SELECT secret_ref,COALESCE(result_digest,'') FROM speaker_embedding_staging WHERE job_id=? AND operation_id=?`, jobID, request.OperationID).Scan(&stagedRef, &stagedDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Permanent(errors.New("speaker embedding result was never staged"))
	}
	if err != nil {
		return err
	}
	if stagedDigest != digest {
		return domain.Permanent(errors.New("staged speaker digest does not match the committed result"))
	}
	for index, record := range records {
		if strings.TrimSpace(record.SecretRef) == "" || record.SecretRef != stagedRef || record.ProfileID != request.Speaker.ProfileID || record.AssetID != assetID || record.SpaceID != request.SpaceID || record.ConsentGeneration != consentGeneration || record.ProfileGeneration != profileGeneration || strings.TrimSpace(record.GenerationID) == "" {
			return domain.Permanent(fmt.Errorf("speaker embedding record %d has mismatched identity, generation or ciphertext reference", index))
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
	if _, err := tx.ExecContext(ctx, `UPDATE inference_tasks SET result_digest=?,updated_at=? WHERE job_id=? AND operation=? AND config_hash=? AND model_revision=? AND cancel_requested=0`, digest, formatTime(now), jobID, string(request.Operation), request.ConfigHash, request.ModelRevision); err != nil {
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

// ListSpeakerTurnsForGeneration returns the stored turns of one diarization
// generation, which is what profile creation selects eligible samples from.
func (r *Repository) ListSpeakerTurnsForGeneration(ctx context.Context, generationID string) ([]domain.SpeakerTurnRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id,generation_id,asset_id,start_ms,end_ms,COALESCE(local_speaker_id,''),confidence,overlap
		FROM speaker_turns WHERE generation_id=? ORDER BY start_ms,end_ms,id`, generationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.SpeakerTurnRow{}
	for rows.Next() {
		var row domain.SpeakerTurnRow
		var confidence *float64
		var overlap int
		if err := rows.Scan(&row.ID, &row.GenerationID, &row.AssetID, &row.StartMS, &row.EndMS, &row.LocalSpeakerID, &confidence, &overlap); err != nil {
			return nil, err
		}
		row.Confidence = confidence
		row.Overlap = overlap != 0
		out = append(out, row)
	}
	return out, rows.Err()
}

// SpeakerEmbeddingRefsBySpace lists every encrypted reference for a space,
// which is the authorised in-memory scan the matcher performs.
func (r *Repository) SpeakerEmbeddingRefsBySpace(ctx context.Context, spaceID string) ([]domain.SpeakerEmbedding, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id,e.profile_id,e.asset_id,e.generation_id,e.space_id,e.start_ms,e.end_ms,e.speech_duration_ms,e.quality_json,e.secret_ref,e.consent_generation,e.profile_generation
		FROM speaker_embeddings e
		JOIN speaker_profiles p ON p.id=e.profile_id AND p.state='active'
		WHERE e.space_id=?
		ORDER BY e.profile_id,e.start_ms`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.SpeakerEmbedding{}
	for rows.Next() {
		var record domain.SpeakerEmbedding
		if err := rows.Scan(&record.ID, &record.ProfileID, &record.AssetID, &record.GenerationID, &record.SpaceID,
			&record.StartMS, &record.EndMS, &record.SpeechDurationMS, &record.QualityJSON,
			&record.SecretRef, &record.ConsentGeneration, &record.ProfileGeneration); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}
