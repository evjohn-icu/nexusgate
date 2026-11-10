package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// commitSpeakerInferenceResult is the Hub-side half of the biometric
// boundary. The model process returns a plaintext vector; the Hub encrypts it
// before any SQLite write, registers the deletion obligation first, and only
// then publishes metadata through CommitSpeakerEmbedding. The vector never
// enters staged_json, model_runs, logs or an API response.
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

	staging, err := repo.GetSpeakerEmbeddingStaging(ctx, request.OperationID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		consent, err := repo.GetSpeakerConsent(ctx)
		if err != nil {
			return err
		}
		if !consent.Enabled {
			return domain.Permanent(ErrSpeakerConsentRequired)
		}
		profiles, err := repo.ListSpeakerProfiles(ctx)
		if err != nil {
			return err
		}
		var profile *domain.SpeakerProfile
		for index := range profiles {
			if profiles[index].ID == request.Speaker.ProfileID {
				profile = &profiles[index]
				break
			}
		}
		if profile == nil || profile.State != "active" {
			return domain.Permanent(errors.New("speaker profile is not active"))
		}
		secretRef := speakerBiometricPrefix + request.OperationID
		// Register the obligation before the ciphertext exists: a crash after
		// Put but before the metadata row still leaves a purge entry.
		if err := repo.AddPurgeRef(ctx, secretRef); err != nil {
			return err
		}
		payload, err := json.Marshal(struct {
			Dtype     string    `json:"dtype"`
			Dimension int       `json:"dimension"`
			Vector    []float32 `json:"vector"`
		}{Dtype: "float32", Dimension: len(record.Vector), Vector: record.Vector})
		if err != nil {
			return err
		}
		if err := s.secrets.Put(secretRef, string(payload)); err != nil {
			return fmt.Errorf("encrypt speaker embedding: %w", err)
		}
		staging = domain.SpeakerEmbeddingStaging{
			OperationID:       request.OperationID,
			JobID:             jobID,
			ProfileID:         request.Speaker.ProfileID,
			ProfileGeneration: profile.Generation,
			ConsentGeneration: consent.Generation,
			SecretRef:         secretRef,
		}
		if err := repo.PutSpeakerEmbeddingStaging(ctx, staging); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if staging.JobID != jobID || staging.ProfileID != request.Speaker.ProfileID {
			return domain.Permanent(errors.New("staged speaker embedding belongs to another job or profile"))
		}
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
		QualityJSON:       `{}`,
		SecretRef:         staging.SecretRef,
		ConsentGeneration: staging.ConsentGeneration,
		ProfileGeneration: staging.ProfileGeneration,
	}}
	return repo.CommitSpeakerEmbedding(ctx, jobID, owner, staging.ConsentGeneration, staging.ProfileGeneration, records)
}
