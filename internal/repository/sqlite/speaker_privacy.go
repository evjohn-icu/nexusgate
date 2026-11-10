package sqlite

import (
	"context"
	"database/sql"
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

// CreateSpeakerProfile inserts a profile and its sources atomically.
func (r *Repository) CreateSpeakerProfile(ctx context.Context, profile domain.SpeakerProfile, sources []domain.SpeakerProfileSource) error {
	if strings.TrimSpace(profile.ID) == "" {
		return fmt.Errorf("speaker profile id is required")
	}
	now := formatTime(time.Now().UTC())
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO speaker_profiles(id,generation,state,created_at) VALUES(?,?,'active',?)`,
		profile.ID, profile.Generation, now); err != nil {
		return err
	}
	for _, source := range sources {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO speaker_profile_sources(profile_id,asset_id,diarization_generation_id,local_speaker_id) VALUES(?,?,?,?)`,
			profile.ID, source.AssetID, source.DiarizationGenerationID, source.LocalSpeakerID); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	if _, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state='failed',terminal=1,last_error_code='cancelled',last_error_message='speaker profile deleted',updated_at=?
		WHERE id IN (SELECT job_id FROM inference_tasks WHERE operation='speaker_embedding')
		  AND state IN ('pending','running')
		  AND id IN (SELECT job_id FROM speaker_embedding_staging WHERE profile_id=?)`, now, profileID); err != nil {
		return nil, err
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
			state=excluded.state, similarity=excluded.similarity,
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

// ListPurgeRefs returns the pending deletion worklist.
func (r *Repository) ListPurgeRefs(ctx context.Context, limit int) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT secret_ref FROM biometric_purge_refs ORDER BY created_at LIMIT ?`, limit)
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

// PutSpeakerEmbeddingStaging records a staged biometric extraction.
func (r *Repository) PutSpeakerEmbeddingStaging(ctx context.Context, staging domain.SpeakerEmbeddingStaging) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO speaker_embedding_staging(operation_id,job_id,profile_id,profile_generation,consent_generation,secret_ref,created_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(operation_id) DO UPDATE SET secret_ref=excluded.secret_ref`,
		staging.OperationID, staging.JobID, staging.ProfileID, staging.ProfileGeneration,
		staging.ConsentGeneration, staging.SecretRef, formatTime(time.Now().UTC()))
	return err
}

// GetSpeakerEmbeddingStaging returns one staged extraction by operation id.
func (r *Repository) GetSpeakerEmbeddingStaging(ctx context.Context, operationID string) (domain.SpeakerEmbeddingStaging, error) {
	var staging domain.SpeakerEmbeddingStaging
	err := r.db.QueryRowContext(ctx, `
		SELECT operation_id,job_id,profile_id,profile_generation,consent_generation,secret_ref
		FROM speaker_embedding_staging WHERE operation_id=?`, operationID).
		Scan(&staging.OperationID, &staging.JobID, &staging.ProfileID, &staging.ProfileGeneration,
			&staging.ConsentGeneration, &staging.SecretRef)
	return staging, err
}

// GenerationExists reports whether a modality generation row exists.
func (r *Repository) GenerationExists(ctx context.Context, generationID string) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM modality_generations WHERE id=?`, generationID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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
