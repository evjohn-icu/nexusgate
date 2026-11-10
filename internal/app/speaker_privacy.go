package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
)

// speakerPrivacyRepository is the optional persistence behind the biometric
// boundary. Every method here is asserted on the concrete repository so a
// minimal test service compiles without it.
type speakerPrivacyRepository interface {
	GetSpeakerConsent(ctx context.Context) (domain.SpeakerConsent, error)
	SetSpeakerConsent(ctx context.Context, enabled bool) (domain.SpeakerConsent, error)
	CreateSpeakerProfile(ctx context.Context, profile domain.SpeakerProfile, sources []domain.SpeakerProfileSource) error
	ListSpeakerProfiles(ctx context.Context) ([]domain.SpeakerProfile, error)
	DeleteSpeakerProfile(ctx context.Context, profileID string) ([]string, error)
	AddSpeakerEmbedding(ctx context.Context, record domain.SpeakerEmbedding) error
	ListSpeakerEmbeddings(ctx context.Context, profileID string) ([]domain.SpeakerEmbedding, error)
	SpeakerEmbeddingRefsBySpace(ctx context.Context, spaceID string) ([]domain.SpeakerEmbedding, error)
	UpsertSpeakerAssociation(ctx context.Context, association domain.SpeakerAssociation) error
	ListSpeakerAssociations(ctx context.Context, profileID string) ([]domain.SpeakerAssociation, error)
	SetSpeakerAssociationState(ctx context.Context, id, state string) error
	AddPurgeRef(ctx context.Context, secretRef string) error
	ListPurgeRefs(ctx context.Context, limit int) ([]string, error)
	ClearPurgeRef(ctx context.Context, secretRef string) error
	RecordPurgeFailure(ctx context.Context, secretRef, code string) error
	PutSpeakerEmbeddingStaging(ctx context.Context, staging domain.SpeakerEmbeddingStaging) error
	GetSpeakerEmbeddingStaging(ctx context.Context, operationID string) (domain.SpeakerEmbeddingStaging, error)
	GenerationExists(ctx context.Context, generationID string) (bool, error)
	ListSpeakerTurnsForGeneration(ctx context.Context, generationID string) ([]domain.SpeakerTurnRow, error)
}

func (s *Service) speakerPrivacy() (speakerPrivacyRepository, error) {
	repo, ok := s.repo.(speakerPrivacyRepository)
	if !ok {
		return nil, fmt.Errorf("speaker privacy storage is not available")
	}
	return repo, nil
}

// ErrSpeakerConsentRequired is the default-off answer: biometric extraction and
// cross-recording comparison refuse to run until the operator opts in.
var ErrSpeakerConsentRequired = fmt.Errorf("cross-recording speaker comparison requires explicit consent")

// speakerBiometricPrefix keeps biometric payloads distinguishable from provider
// credentials in the same encrypted store.
const speakerBiometricPrefix = "biometric-"

// GetSpeakerConsent returns the current opt-in state.
func (s *Service) GetSpeakerConsent(ctx context.Context) (domain.SpeakerConsent, error) {
	repo, err := s.speakerPrivacy()
	if err != nil {
		return domain.SpeakerConsent{}, err
	}
	return repo.GetSpeakerConsent(ctx)
}

// SetSpeakerConsent records the operator's decision. Disabling consent purges
// every profile and drains the deletion worklist; the original media,
// transcripts and anonymous local diarization turns stay intact because they
// are not biometric data.
func (s *Service) SetSpeakerConsent(ctx context.Context, enabled bool) (domain.SpeakerConsent, error) {
	repo, err := s.speakerPrivacy()
	if err != nil {
		return domain.SpeakerConsent{}, err
	}
	consent, err := repo.SetSpeakerConsent(ctx, enabled)
	if err != nil {
		return domain.SpeakerConsent{}, err
	}
	if !enabled {
		profiles, err := repo.ListSpeakerProfiles(ctx)
		if err != nil {
			return consent, err
		}
		for _, profile := range profiles {
			refs, err := repo.DeleteSpeakerProfile(ctx, profile.ID)
			if err != nil {
				slog.Warn("speaker profile purge failed", "profile_id", profile.ID, "error", err)
				continue
			}
			for _, ref := range refs {
				if err := s.deleteBiometricRef(ref); err != nil {
					slog.Warn("biometric secret purge failed", "error", err)
				}
			}
		}
		if err := s.DrainBiometricPurge(ctx); err != nil {
			slog.Warn("biometric purge drain incomplete", "error", err)
		}
	}
	return consent, nil
}

func (s *Service) deleteBiometricRef(ref string) error {
	if s.secrets == nil || strings.TrimSpace(ref) == "" {
		return nil
	}
	return s.secrets.Delete(ref)
}

// DrainBiometricPurge deletes every queued secret and clears the entry only
// after the deletion succeeded. A failure keeps the obligation for the next
// run; it is never reported as success.
func (s *Service) DrainBiometricPurge(ctx context.Context) error {
	repo, err := s.speakerPrivacy()
	if err != nil {
		return err
	}
	refs, err := repo.ListPurgeRefs(ctx, 256)
	if err != nil {
		return err
	}
	var firstErr error
	for _, ref := range refs {
		if err := s.deleteBiometricRef(ref); err != nil {
			_ = repo.RecordPurgeFailure(ctx, ref, "secret_delete_failed")
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := repo.ClearPurgeRef(ctx, ref); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// CreateSpeakerProfile creates one asset-local profile and queues an embedding
// extraction for each eligible turn. A profile is not an identity, and the
// operation refuses to run without consent.
func (s *Service) CreateSpeakerProfile(ctx context.Context, assetID, diarizationGenerationID, localSpeakerID string) (domain.SpeakerProfile, error) {
	repo, err := s.speakerPrivacy()
	if err != nil {
		return domain.SpeakerProfile{}, err
	}
	consent, err := repo.GetSpeakerConsent(ctx)
	if err != nil {
		return domain.SpeakerProfile{}, err
	}
	if !consent.Enabled {
		return domain.SpeakerProfile{}, ErrSpeakerConsentRequired
	}
	if strings.TrimSpace(localSpeakerID) == "" {
		return domain.SpeakerProfile{}, fmt.Errorf("local speaker id is required")
	}
	exists, err := repo.GenerationExists(ctx, diarizationGenerationID)
	if err != nil {
		return domain.SpeakerProfile{}, err
	}
	if !exists {
		return domain.SpeakerProfile{}, fmt.Errorf("diarization generation not found")
	}
	profile := domain.SpeakerProfile{
		ID:         idgen.New(),
		Generation: 1,
		State:      "active",
		AssetID:    assetID,
	}
	source := domain.SpeakerProfileSource{
		ProfileID:               profile.ID,
		AssetID:                 assetID,
		DiarizationGenerationID: diarizationGenerationID,
		LocalSpeakerID:          localSpeakerID,
	}
	if err := repo.CreateSpeakerProfile(ctx, profile, []domain.SpeakerProfileSource{source}); err != nil {
		return domain.SpeakerProfile{}, err
	}
	if err := s.queueSpeakerExtraction(ctx, repo, profile, consent, assetID, diarizationGenerationID, localSpeakerID); err != nil {
		slog.Warn("speaker extraction enqueue incomplete", "profile_id", profile.ID, "error", err)
	}
	return profile, nil
}

// queueSpeakerExtraction enqueues one speaker_embedding task per eligible turn.
// Eligibility is measured, not assumed: at least three seconds of speech, no
// overlap, and no clipped region. The quality metrics travel with the request so
// the stored record says why a sample qualified.
func (s *Service) queueSpeakerExtraction(ctx context.Context, repo speakerPrivacyRepository, profile domain.SpeakerProfile, consent domain.SpeakerConsent, assetID, generationID, localSpeakerID string) error {
	if s.inferenceRepo == nil || s.inferenceExec == nil {
		return fmt.Errorf("no inference executor is configured")
	}
	turns, err := repo.ListSpeakerTurnsForGeneration(ctx, generationID)
	if err != nil {
		return err
	}
	model, ok := inferencecatalog.Lookup("speaker-campplus")
	if !ok {
		return fmt.Errorf("speaker model is not in the catalog")
	}
	space, err := buildSpace(domain.OpSpeakerEmbedding, model, 0)
	if err != nil {
		return err
	}
	if err := s.inferenceRepo.UpsertEmbeddingSpace(ctx, space); err != nil {
		return err
	}
	meta, err := s.repo.GetMediaMetadata(ctx, assetID)
	if err != nil || meta == nil {
		return fmt.Errorf("asset duration is unknown")
	}
	enqueued := 0
	for _, turn := range turns {
		if turn.LocalSpeakerID != localSpeakerID || turn.Overlap {
			continue
		}
		if turn.EndMS-turn.StartMS < 3000 {
			continue
		}
		request := domain.InferenceRequest{
			SchemaVersion: domain.InferenceSchemaVersion,
			OperationID:   idgen.New(),
			Operation:     domain.OpSpeakerEmbedding,
			ModelID:       model.ModelID,
			ModelRevision: model.Revision,
			SpaceID:       space.ID,
			InputHash:     fmt.Sprintf("speaker-%s-%d-%d", profile.ID, turn.StartMS, turn.EndMS),
			ConfigHash:    inferenceConfigHash(string(domain.OpSpeakerEmbedding), model.Revision, space.PreprocessingJSON, model.RuntimeDigest, fmt.Sprint(consent.Generation), fmt.Sprint(profile.Generation), domain.InferenceSchemaVersion),
			TimeoutMS:     inferenceTimeoutMS,
			Input: domain.InferenceInput{
				Kind:       domain.InputKindAsset,
				AssetID:    assetID,
				DurationMS: meta.DurationMS,
			},
			Speaker: &domain.SpeakerSettings{
				Interval:  &domain.TimeInterval{StartMS: turn.StartMS, EndMS: turn.EndMS},
				ProfileID: profile.ID,
			},
		}
		if err := request.Validate(); err != nil {
			return err
		}
		key, err := (InferenceTaskKey{
			Operation:         domain.OpSpeakerEmbedding,
			SourceIdentity:    fmt.Sprintf("%s:%s:%d-%d", assetID, profile.ID, turn.StartMS, turn.EndMS),
			ModelID:           model.ModelID,
			ModelRevision:     model.Revision,
			ConfigHash:        request.ConfigHash,
			SpaceID:           space.ID,
			ConsentGeneration: consent.Generation,
			ProfileGeneration: profile.Generation,
		}).Key()
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			return err
		}
		task := domain.InferenceTask{
			AssetID:       assetID,
			TaskKey:       key,
			Operation:     domain.OpSpeakerEmbedding,
			RequestJSON:   string(encoded),
			ConfigHash:    request.ConfigHash,
			ModelRevision: model.Revision,
		}
		if _, err := s.inferenceRepo.EnqueueInferenceTask(ctx, nil, task, 40); err != nil {
			return err
		}
		enqueued++
	}
	if enqueued == 0 {
		return fmt.Errorf("speaker has no eligible clean speech")
	}
	return nil
}

// ListSpeakerProfiles returns active profiles.
func (s *Service) ListSpeakerProfiles(ctx context.Context) ([]domain.SpeakerProfile, error) {
	repo, err := s.speakerPrivacy()
	if err != nil {
		return nil, err
	}
	return repo.ListSpeakerProfiles(ctx)
}

// DeleteSpeakerProfile removes a profile and destroys its ciphertext.
func (s *Service) DeleteSpeakerProfile(ctx context.Context, profileID string) error {
	repo, err := s.speakerPrivacy()
	if err != nil {
		return err
	}
	refs, err := repo.DeleteSpeakerProfile(ctx, profileID)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err := s.deleteBiometricRef(ref); err != nil {
			slog.Warn("biometric secret purge pending", "error", err)
		}
	}
	return s.DrainBiometricPurge(ctx)
}

// SearchSpeakerMatches computes candidate pairings for a profile. Without a
// calibrated threshold the state is "unknown": similarity is reported, a match
// is not asserted, and no automatic grouping happens.
func (s *Service) SearchSpeakerMatches(ctx context.Context, profileID string, limit int) ([]domain.SpeakerAssociation, error) {
	repo, err := s.speakerPrivacy()
	if err != nil {
		return nil, err
	}
	consent, err := repo.GetSpeakerConsent(ctx)
	if err != nil {
		return nil, err
	}
	if !consent.Enabled {
		return nil, ErrSpeakerConsentRequired
	}
	records, err := repo.ListSpeakerEmbeddings(ctx, profileID)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return []domain.SpeakerAssociation{}, nil
	}
	spaceID := records[0].SpaceID
	centroid, err := s.profileCentroid(records, spaceID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	all, err := repo.SpeakerEmbeddingRefsBySpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	byProfile := make(map[string][]domain.SpeakerEmbedding)
	for _, record := range all {
		if record.ProfileID == profileID {
			continue
		}
		byProfile[record.ProfileID] = append(byProfile[record.ProfileID], record)
	}
	associations := make([]domain.SpeakerAssociation, 0, len(byProfile))
	for otherID, otherRecords := range byProfile {
		other, err := s.profileCentroid(otherRecords, spaceID)
		if err != nil {
			continue
		}
		associations = append(associations, domain.SpeakerAssociation{
			LeftProfileID:     profileID,
			RightProfileID:    otherID,
			State:             domain.AssociationUnknown,
			Similarity:        cosineFloat32(centroid, other),
			ConsentGeneration: consent.Generation,
		})
	}
	sort.Slice(associations, func(i, j int) bool {
		if associations[i].Similarity != associations[j].Similarity {
			return associations[i].Similarity > associations[j].Similarity
		}
		return associations[i].RightProfileID < associations[j].RightProfileID
	})
	if len(associations) > limit {
		associations = associations[:limit]
	}
	for _, association := range associations {
		if err := repo.UpsertSpeakerAssociation(ctx, association); err != nil {
			return nil, err
		}
	}
	return repo.ListSpeakerAssociations(ctx, profileID)
}

// profileCentroid loads the profile's encrypted vectors, normalizes each and
// returns the duration-weighted mean, renormalized. It runs only after the
// caller has established consent.
func (s *Service) profileCentroid(records []domain.SpeakerEmbedding, spaceID string) ([]float32, error) {
	if s.secrets == nil {
		return nil, fmt.Errorf("secret store is not configured")
	}
	var weighted []float64
	var totalWeight float64
	for _, record := range records {
		if record.SpaceID != spaceID {
			continue
		}
		raw, ok, err := s.secrets.Resolve(record.SecretRef)
		if err != nil || !ok {
			continue
		}
		var payload struct {
			Dtype     string    `json:"dtype"`
			Dimension int       `json:"dimension"`
			Vector    []float32 `json:"vector"`
		}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			continue
		}
		if payload.Dtype != "float32" || payload.Dimension != len(payload.Vector) || len(payload.Vector) == 0 {
			continue
		}
		var norm float64
		for _, value := range payload.Vector {
			norm += float64(value) * float64(value)
		}
		if norm <= 0 {
			continue
		}
		norm = math.Sqrt(norm)
		if weighted == nil {
			weighted = make([]float64, len(payload.Vector))
		}
		if len(weighted) != len(payload.Vector) {
			continue
		}
		weight := float64(record.SpeechDurationMS)
		if weight <= 0 {
			weight = 1
		}
		for i, value := range payload.Vector {
			weighted[i] += float64(value) / norm * weight
		}
		totalWeight += weight
	}
	if weighted == nil || totalWeight == 0 {
		return nil, fmt.Errorf("profile has no readable embeddings")
	}
	out := make([]float32, len(weighted))
	var norm float64
	for i, value := range weighted {
		value /= totalWeight
		out[i] = float32(value)
		norm += value * value
	}
	if norm <= 0 {
		return nil, fmt.Errorf("profile centroid is zero")
	}
	norm = math.Sqrt(norm)
	for i := range out {
		out[i] = float32(float64(out[i]) / norm)
	}
	return out, nil
}

func cosineFloat32(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// SetSpeakerAssociationState records an operator decision on one pairing.
func (s *Service) SetSpeakerAssociationState(ctx context.Context, id, state string) error {
	repo, err := s.speakerPrivacy()
	if err != nil {
		return err
	}
	consent, err := repo.GetSpeakerConsent(ctx)
	if err != nil {
		return err
	}
	if !consent.Enabled {
		return ErrSpeakerConsentRequired
	}
	return repo.SetSpeakerAssociationState(ctx, id, state)
}

// SplitSpeakerCluster marks every candidate or confirmed pairing that crosses
// the requested partition as rejected, while keeping decisions inside each
// part. A split is the operator's correction of the clustering, not a new
// similarity judgement, so it never creates a confirmed edge.
func (s *Service) SplitSpeakerCluster(ctx context.Context, _ string, profileIDs []string) error {
	repo, err := s.speakerPrivacy()
	if err != nil {
		return err
	}
	consent, err := repo.GetSpeakerConsent(ctx)
	if err != nil {
		return err
	}
	if !consent.Enabled {
		return ErrSpeakerConsentRequired
	}
	if len(profileIDs) < 2 {
		return fmt.Errorf("a split needs at least two profile ids")
	}
	partition := make(map[string]int, len(profileIDs))
	for i, id := range profileIDs {
		partition[id] = i
	}
	seen := make(map[string]bool)
	for _, id := range profileIDs {
		associations, err := repo.ListSpeakerAssociations(ctx, id)
		if err != nil {
			return err
		}
		for _, association := range associations {
			leftPart, leftOK := partition[association.LeftProfileID]
			rightPart, rightOK := partition[association.RightProfileID]
			if !leftOK || !rightOK || leftPart == rightPart {
				continue
			}
			if seen[association.ID] {
				continue
			}
			seen[association.ID] = true
			if err := repo.SetSpeakerAssociationState(ctx, association.ID, domain.AssociationRejected); err != nil {
				return err
			}
		}
	}
	return nil
}
