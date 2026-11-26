package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

// commitSpeakerInferenceResult is the Hub-side half of the biometric
// boundary. The model process returns a plaintext vector; the Hub encrypts it
// before any SQLite write, registers the deletion obligation first, and only
// then publishes metadata through CommitSpeakerEmbedding. The vector never
// enters staged_json, model_runs, logs or an API response; only its opaque
// one-way digest identity is recorded.
//
// The whole staging-and-commit sequence runs under the biometric state lock
// (acquired before any secret-store operation), and the commit transaction
// re-checks lease, consent generation, profile generation and staged digest,
// so the read ahead of the encrypted write is an optimization, never an
// authority.
func (s *Service) commitSpeakerInferenceResult(ctx context.Context, jobID, owner string, request domain.InferenceRequest, result domain.InferenceResult) error {
	if s.secrets == nil {
		return fmt.Errorf("biometric secret store is not configured")
	}
	repo, err := s.speakerPrivacy()
	if err != nil {
		return err
	}
	if request.Speaker == nil || request.Speaker.Interval == nil || strings.TrimSpace(request.Speaker.ProfileID) == "" {
		return domain.Permanent(errors.New("speaker request has no profile interval"))
	}
	if len(result.Embeddings) != 1 {
		return domain.Permanent(fmt.Errorf("speaker embedding must return exactly one vector, got %d", len(result.Embeddings)))
	}
	record := result.Embeddings[0]
	interval := domain.TimeInterval{StartMS: record.StartMS, EndMS: record.EndMS}
	if err := interval.Validate(request.Input.DurationMS); err != nil {
		return domain.Permanent(fmt.Errorf("speaker vector interval: %w", err))
	}
	if interval.StartMS < request.Speaker.Interval.StartMS || interval.EndMS > request.Speaker.Interval.EndMS {
		return domain.Permanent(errors.New("speaker vector lies outside the requested interval"))
	}
	if len(record.Vector) == 0 {
		return domain.Permanent(errors.New("speaker vector is empty"))
	}
	digest, err := remote.DigestResult(result)
	if err != nil {
		return domain.Permanent(fmt.Errorf("digest speaker result: %w", err))
	}
	// Validate the quality allowlist before the lock: an invalid payload must
	// reject the whole result before anything is encrypted or staged.
	quality, err := sanitizeSpeakerQuality(record.QualityJSON)
	if err != nil {
		return err
	}

	release, err := s.lockBiometricState()
	if err != nil {
		return err
	}
	defer release()

	// Read-only precheck: never encrypt anything when the lease is already
	// gone, the operator cancelled, or these bytes were already committed.
	// A repeat of the committed bytes is an idempotent success; a different
	// result under the same operation id is rejected without mutation.
	state, err := repo.SpeakerResultCommitState(ctx, jobID, owner)
	if err != nil {
		return err
	}
	if state.CommittedDigest != "" {
		if state.CommittedDigest == digest {
			return ErrInferenceAlreadyCommitted
		}
		return domain.Permanent(errors.New("speaker result digest does not match the committed result"))
	}
	if state.CancelRequested {
		return domain.Permanent(domain.ErrInferenceCancelled)
	}
	if !state.LeaseValid {
		return fmt.Errorf("speaker embedding %s: %w", jobID, domain.ErrJobLeaseLost)
	}

	staging, err := s.stageSpeakerInferenceLocked(ctx, repo, request, result, digest, jobID)
	if err != nil {
		return err
	}

	generationID := strings.TrimSpace(request.Speaker.DiarizationGenerationID)
	if generationID == "" {
		generationID = request.OperationID
	}
	records := []domain.SpeakerEmbedding{{
		ProfileID:         request.Speaker.ProfileID,
		AssetID:           request.Input.AssetID,
		GenerationID:      generationID,
		SpaceID:           request.SpaceID,
		StartMS:           interval.StartMS,
		EndMS:             interval.EndMS,
		SpeechDurationMS:  interval.EndMS - interval.StartMS,
		QualityJSON:       quality,
		SecretRef:         staging.SecretRef,
		ConsentGeneration: staging.ConsentGeneration,
		ProfileGeneration: staging.ProfileGeneration,
	}}
	return repo.CommitSpeakerEmbedding(ctx, jobID, owner, staging.ConsentGeneration, staging.ProfileGeneration, digest, records)
}

// speakerQualityMetrics is the allowlist of measured eligibility metrics the
// speaker adapter publishes (see inference/adapters/speaker.py). The struct is
// the persistence format: marshalling it re-emits exactly these keys in a
// deterministic order, so no vector, identifier or arbitrary biometric record
// can ride into SQLite through quality_json.
type speakerQualityMetrics struct {
	ClippedFraction  *float64 `json:"clipped_fraction,omitempty"`
	RmsDBFS          *float64 `json:"rms_dbfs,omitempty"`
	SpeechDurationMS *int64   `json:"speech_duration_ms,omitempty"`
}

// sanitizeSpeakerQuality reduces an adapter's quality payload to the allowed
// measured metrics with validated types and ranges. A payload that is not a
// JSON object, or an allowed key with an impossible value, is a permanent
// invalid-output rejection: measured metrics that arrive in the wrong shape
// are untrustworthy, and silently normalizing them would launder bad
// instrumentation into a stored record. Unknown keys are dropped, which is
// what keeps a stray array or nested object from persisting anything.
func sanitizeSpeakerQuality(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return "{}", nil
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return "", domain.Permanent(fmt.Errorf("speaker quality metrics are not a JSON object: %w", err))
	}
	metrics := speakerQualityMetrics{}
	if field, ok := decoded["clipped_fraction"]; ok {
		var value float64
		if err := json.Unmarshal(field, &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return "", domain.Permanent(fmt.Errorf("speaker quality clipped_fraction is not a fraction in [0,1]"))
		}
		metrics.ClippedFraction = &value
	}
	if field, ok := decoded["rms_dbfs"]; ok {
		var value float64
		if err := json.Unmarshal(field, &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value > 0 {
			return "", domain.Permanent(fmt.Errorf("speaker quality rms_dbfs is not a non-positive level"))
		}
		metrics.RmsDBFS = &value
	}
	if field, ok := decoded["speech_duration_ms"]; ok {
		var value int64
		if err := json.Unmarshal(field, &value); err != nil || value < 0 {
			return "", domain.Permanent(fmt.Errorf("speaker quality speech_duration_ms is not a non-negative duration"))
		}
		metrics.SpeechDurationMS = &value
	}
	encoded, err := json.Marshal(metrics)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// stageSpeakerInferenceLocked encrypts the result under a fresh unique
// reference and records the staging row, or reuses an earlier stage of the
// same bytes. It must be called with the biometric state lock held.
//
// Reuse requires the job, profile and digest to match the staged row and the
// ciphertext to still resolve. Old incomplete staging — a different digest,
// or ciphertext lost to a crash or a purge — is repaired from this new valid
// submission before anything canonical is published: the superseded
// reference's deletion obligation is still on the worklist from its own
// stage, so no ciphertext can survive unowned.
func (s *Service) stageSpeakerInferenceLocked(ctx context.Context, repo speakerPrivacyRepository, request domain.InferenceRequest, result domain.InferenceResult, digest, jobID string) (domain.SpeakerEmbeddingStaging, error) {
	staging, err := repo.GetSpeakerEmbeddingStaging(ctx, request.OperationID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return s.stageFreshSpeakerInferenceLocked(ctx, repo, request, result, digest, jobID)
	case err != nil:
		return domain.SpeakerEmbeddingStaging{}, err
	}
	if staging.JobID != jobID || staging.ProfileID != request.Speaker.ProfileID {
		return domain.SpeakerEmbeddingStaging{}, domain.Permanent(errors.New("staged speaker embedding belongs to another job or profile"))
	}
	// Reuse requires the job, profile and digest to match the staged row and
	// the ciphertext to still resolve. A complete stage of different bytes is
	// immutable: a mismatching same-operation result is rejected read-only and
	// mutates nothing. Repair is reserved for staging that is truly
	// incomplete — a legacy row with no recorded digest, or ciphertext that no
	// longer resolves — and only then from a new valid submission; the
	// superseded reference's deletion obligation is still on the worklist from
	// its own stage, so no ciphertext can survive unowned.
	if staging.ResultDigest == digest {
		if _, ok, resolveErr := s.secrets.Resolve(staging.SecretRef); resolveErr == nil && ok {
			return staging, nil
		}
	} else if staging.ResultDigest != "" {
		if _, ok, resolveErr := s.secrets.Resolve(staging.SecretRef); resolveErr == nil && ok {
			return domain.SpeakerEmbeddingStaging{}, domain.Permanent(errors.New("staged speaker digest does not match the submitted result"))
		}
	}
	return s.stageFreshSpeakerInferenceLocked(ctx, repo, request, result, digest, jobID)
}

// stageFreshSpeakerInferenceLocked registers the deletion obligation for a new
// unique reference, encrypts the vector under it and records the staging row
// with its digest identity. The obligation exists before the ciphertext, so a
// crash between the two steps still leaves a purge entry the drain can honor;
// a crash after the ciphertext but before the staging row leaves an orphan the
// same drain deletes.
func (s *Service) stageFreshSpeakerInferenceLocked(ctx context.Context, repo speakerPrivacyRepository, request domain.InferenceRequest, result domain.InferenceResult, digest, jobID string) (domain.SpeakerEmbeddingStaging, error) {
	consent, err := repo.GetSpeakerConsent(ctx)
	if err != nil {
		return domain.SpeakerEmbeddingStaging{}, err
	}
	if !consent.Enabled {
		return domain.SpeakerEmbeddingStaging{}, domain.Permanent(ErrSpeakerConsentRequired)
	}
	profiles, err := repo.ListSpeakerProfiles(ctx)
	if err != nil {
		return domain.SpeakerEmbeddingStaging{}, err
	}
	var profile *domain.SpeakerProfile
	for index := range profiles {
		if profiles[index].ID == request.Speaker.ProfileID {
			profile = &profiles[index]
			break
		}
	}
	if profile == nil || profile.State != "active" {
		return domain.SpeakerEmbeddingStaging{}, domain.Permanent(errors.New("speaker profile is not active"))
	}
	vector := result.Embeddings[0].Vector
	secretRef := speakerBiometricPrefix + request.OperationID + "-" + idgen.New()
	// Register the obligation before the ciphertext exists: a crash after Put
	// but before the metadata row still leaves a purge entry.
	if err := repo.AddPurgeRef(ctx, secretRef); err != nil {
		return domain.SpeakerEmbeddingStaging{}, err
	}
	payload, err := json.Marshal(struct {
		Dtype     string    `json:"dtype"`
		Dimension int       `json:"dimension"`
		Vector    []float32 `json:"vector"`
	}{Dtype: "float32", Dimension: len(vector), Vector: vector})
	if err != nil {
		return domain.SpeakerEmbeddingStaging{}, err
	}
	if err := s.secrets.Put(secretRef, string(payload)); err != nil {
		return domain.SpeakerEmbeddingStaging{}, fmt.Errorf("encrypt speaker embedding: %w", err)
	}
	staging := domain.SpeakerEmbeddingStaging{
		OperationID:       request.OperationID,
		JobID:             jobID,
		ProfileID:         profile.ID,
		ProfileGeneration: profile.Generation,
		ConsentGeneration: consent.Generation,
		SecretRef:         secretRef,
		ResultDigest:      digest,
	}
	if err := repo.PutSpeakerEmbeddingStaging(ctx, staging); err != nil {
		return domain.SpeakerEmbeddingStaging{}, err
	}
	return staging, nil
}
