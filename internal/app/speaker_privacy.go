package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/evjohn-icu/nexusgate/internal/cachecoord"
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
	CreateSpeakerProfileWithTasks(ctx context.Context, profile domain.SpeakerProfile, sources []domain.SpeakerProfileSource, tasks []domain.InferenceTask, priority int, consentGeneration int64) ([]string, error)
	DiarizationGenerationIdentity(ctx context.Context, generationID string) (string, domain.InferenceOperation, error)
	ListSpeakerProfiles(ctx context.Context) ([]domain.SpeakerProfile, error)
	DeleteSpeakerProfile(ctx context.Context, profileID string) ([]string, error)
	AddSpeakerEmbedding(ctx context.Context, record domain.SpeakerEmbedding) error
	ListSpeakerEmbeddings(ctx context.Context, profileID string) ([]domain.SpeakerEmbedding, error)
	SpeakerEmbeddingRefsBySpace(ctx context.Context, spaceID string) ([]domain.SpeakerEmbedding, error)
	UpsertSpeakerAssociation(ctx context.Context, association domain.SpeakerAssociation) error
	ListSpeakerAssociations(ctx context.Context, profileID string) ([]domain.SpeakerAssociation, error)
	SetSpeakerAssociationState(ctx context.Context, id, state string) error
	AddPurgeRef(ctx context.Context, secretRef string) error
	ListPurgeRefs(ctx context.Context, after string, limit int) ([]string, error)
	ClearPurgeRef(ctx context.Context, secretRef string) error
	RecordPurgeFailure(ctx context.Context, secretRef, code string) error
	ClassifyBiometricPurgeRefs(ctx context.Context, refs []string) (map[string]domain.BiometricPurgeRefDisposition, error)
	ClearStagingBySecretRef(ctx context.Context, secretRef string) error
	PutSpeakerEmbeddingStaging(ctx context.Context, staging domain.SpeakerEmbeddingStaging) error
	GetSpeakerEmbeddingStaging(ctx context.Context, operationID string) (domain.SpeakerEmbeddingStaging, error)
	SpeakerResultCommitState(ctx context.Context, jobID, owner string) (domain.SpeakerResultCommitState, error)
	CommitSpeakerEmbedding(ctx context.Context, jobID, owner string, consentGeneration, profileGeneration int64, digest string, records []domain.SpeakerEmbedding) error
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

const (
	// speakerEligibleTurnMS is the minimum clean, non-overlapping speech a
	// diarization turn must carry to become an extraction sample.
	speakerEligibleTurnMS = 3000
	// speakerExtractionPriority matches the other inference task priorities.
	speakerExtractionPriority = 40
	// biometricPurgeBatchSize bounds every read of the deletion worklist.
	biometricPurgeBatchSize = 256
)

// biometricStateLockName is the fixed, private coordination file inside
// DataDir. Staging, commit, consent changes, profile deletion and the purge
// drain all take it before they touch the secret store or the canonical rows,
// so two Hub processes (or the pipeline and the API) can never interleave a
// stage with a purge.
const biometricStateLockName = ".biometric-state.lock"

// biometricStateMu serializes biometric mutations for services whose DataDir is
// empty (in-process test services with no filesystem to coordinate). Real
// services coordinate through the biometricStateLockName file instead; a
// package-level mutex is safe here because the locked helpers below never
// re-acquire and no production service runs with an empty DataDir.
var biometricStateMu sync.Mutex

// lockBiometricState takes the biometric state lock. The returned release
// function must always run, and every internal helper named ...Locked assumes
// the caller already holds it: the lock order is biometric state first, then
// the secret store's own path lock, so a locked helper must not call the
// exported, self-locking entry points again.
func (s *Service) lockBiometricState() (func(), error) {
	if strings.TrimSpace(s.cfg.DataDir) == "" {
		biometricStateMu.Lock()
		return biometricStateMu.Unlock, nil
	}
	lock, err := cachecoord.AcquirePath(filepath.Join(s.cfg.DataDir, biometricStateLockName), true)
	if err != nil {
		return nil, fmt.Errorf("acquire biometric state lock: %w", err)
	}
	return func() {
		if err := lock.Release(); err != nil {
			// Releasing the advisory lock failed while closing the file; the
			// process is still holding a path it cannot give up cleanly, so
			// surface it rather than silently continuing.
			slog.Warn("release biometric state lock failed", "error", err)
		}
	}, nil
}

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
//
// The whole destructive path runs under the biometric state lock, and the
// purge deletion only happens through the drain: deleting a profile moves its
// ciphertext obligations to the worklist inside one transaction, so the
// worklist, not this call, is the single source of truth about what must be
// destroyed.
func (s *Service) SetSpeakerConsent(ctx context.Context, enabled bool) (domain.SpeakerConsent, error) {
	repo, err := s.speakerPrivacy()
	if err != nil {
		return domain.SpeakerConsent{}, err
	}
	release, err := s.lockBiometricState()
	if err != nil {
		return domain.SpeakerConsent{}, err
	}
	defer release()
	consent, err := repo.SetSpeakerConsent(ctx, enabled)
	if err != nil {
		return domain.SpeakerConsent{}, err
	}
	if enabled {
		return consent, nil
	}
	profiles, err := repo.ListSpeakerProfiles(ctx)
	if err != nil {
		return consent, err
	}
	var failures []error
	for _, profile := range profiles {
		if _, err := repo.DeleteSpeakerProfile(ctx, profile.ID); err != nil {
			// The profile may have been deleted concurrently. Do not turn
			// that into a silent success: the caller must know some
			// biometric data could still be live.
			failures = append(failures, fmt.Errorf("delete speaker profile %s: %w", profile.ID, err))
			continue
		}
	}
	if err := s.drainBiometricPurgeLocked(ctx, repo); err != nil {
		failures = append(failures, fmt.Errorf("drain biometric purge: %w", err))
	}
	if len(failures) != 0 {
		return consent, errors.Join(failures...)
	}
	return consent, nil
}

func (s *Service) deleteBiometricRef(ref string) error {
	if s.secrets == nil || strings.TrimSpace(ref) == "" {
		return nil
	}
	if biometricDeleteHook != nil {
		return biometricDeleteHook(s, ref)
	}
	return s.secrets.Delete(ref)
}

// biometricDeleteHook is nil in production. Fault-injection tests set it to
// make one ciphertext deletion fail without standing up a broken store; it is
// only consulted through deleteBiometricRef and every implementation must fall
// back to the real store for references it does not recognize.
var biometricDeleteHook func(svc *Service, ref string) error

// DrainBiometricPurge deletes every queued secret and clears the entry only
// after the deletion succeeded. A failure keeps the obligation for the next
// run; it is never reported as success. It runs at queue-serving startup and
// on the serving maintenance ticker, and after every explicit consent or
// profile deletion — never implicitly from NewService.
func (s *Service) DrainBiometricPurge(ctx context.Context) error {
	repo, err := s.speakerPrivacy()
	if err != nil {
		return err
	}
	release, err := s.lockBiometricState()
	if err != nil {
		return err
	}
	defer release()
	return s.drainBiometricPurgeLocked(ctx, repo)
}

// drainBiometricPurgeLocked walks the deletion worklist in stable
// secret_ref-ordered batches of 256 until it is exhausted. The keyset advances
// across failures, so one stuck deletion cannot stall the references after
// it. Canonical references are never deleted while a committed embedding row
// names them: the erroneous obligation is cleared and the ciphertext kept. A
// staged extraction with unrevoked consent and an active profile retains its
// reference for its commit; obsolete staging metadata for cancelled or
// terminal jobs is removed together with the obligation.
func (s *Service) drainBiometricPurgeLocked(ctx context.Context, repo speakerPrivacyRepository) error {
	after := ""
	var failures []error
	for {
		refs, err := repo.ListPurgeRefs(ctx, after, biometricPurgeBatchSize)
		if err != nil {
			failures = append(failures, err)
			break
		}
		if len(refs) == 0 {
			break
		}
		dispositions, err := repo.ClassifyBiometricPurgeRefs(ctx, refs)
		if err != nil {
			failures = append(failures, err)
			break
		}
		for _, ref := range refs {
			after = ref
			disposition := dispositions[ref]
			switch {
			case disposition.Canonical:
				if err := repo.ClearPurgeRef(ctx, ref); err != nil {
					failures = append(failures, fmt.Errorf("clear erroneous purge obligation %s: %w", ref, err))
				}
				continue
			case disposition.LiveStaging:
				continue
			}
			if err := s.deleteBiometricRef(ref); err != nil {
				_ = repo.RecordPurgeFailure(ctx, ref, "secret_delete_failed")
				failures = append(failures, fmt.Errorf("delete biometric secret %s: %w", ref, err))
				continue
			}
			if disposition.Staged {
				// The staging row's job is cancelled or terminal: obsolete
				// staging metadata goes away with the ciphertext.
				if err := repo.ClearStagingBySecretRef(ctx, ref); err != nil {
					failures = append(failures, fmt.Errorf("clear obsolete biometric staging %s: %w", ref, err))
					continue
				}
			}
			if err := repo.ClearPurgeRef(ctx, ref); err != nil {
				failures = append(failures, fmt.Errorf("clear biometric purge obligation %s: %w", ref, err))
			}
		}
		if len(refs) < biometricPurgeBatchSize {
			break
		}
	}
	return errors.Join(failures...)
}

// CreateSpeakerProfile creates one asset-local profile and queues an embedding
// extraction for each eligible turn. A profile is not an identity, and the
// operation refuses to run without consent.
//
// Every eligibility check runs before any row exists: a generation that does
// not exist or is not committed diarization, a generation that belongs to
// another asset, a speaker with no turns in that generation, or a speaker with
// no clean eligible speech is an explicit invalid request that leaves no
// profile behind. The profile, its sources and all extraction jobs/tasks are
// then persisted in one transaction gated by a consent-generation CAS, so a
// consent change that commits first cannot adopt a profile the operator no
// longer authorized. Queueing needs no local inference executor: a Hub without
// one keeps the tasks pending for a paired Worker.
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
	assetID = strings.TrimSpace(assetID)
	generationID := strings.TrimSpace(diarizationGenerationID)
	genAssetID, genOperation, err := repo.DiarizationGenerationIdentity(ctx, generationID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SpeakerProfile{}, fmt.Errorf("diarization generation %q does not exist or is not committed", generationID)
	}
	if err != nil {
		return domain.SpeakerProfile{}, err
	}
	if genOperation != domain.OpDiarization {
		return domain.SpeakerProfile{}, fmt.Errorf("generation %q records operation %q, not diarization", generationID, genOperation)
	}
	if genAssetID != assetID {
		return domain.SpeakerProfile{}, fmt.Errorf("diarization generation %q belongs to asset %q, not %q", generationID, genAssetID, assetID)
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
		DiarizationGenerationID: generationID,
		LocalSpeakerID:          localSpeakerID,
	}
	tasks, err := s.speakerExtractionTasks(ctx, repo, profile, consent, assetID, generationID, localSpeakerID)
	if err != nil {
		return domain.SpeakerProfile{}, err
	}
	if _, err := repo.CreateSpeakerProfileWithTasks(ctx, profile, []domain.SpeakerProfileSource{source}, tasks, speakerExtractionPriority, consent.Generation); err != nil {
		return domain.SpeakerProfile{}, err
	}
	return profile, nil
}

// speakerExtractionTasks builds one speaker_embedding task per eligible turn.
// Eligibility is measured, not assumed: the turn must belong to this asset and
// speaker, carry at least three seconds of speech, and not overlap another
// turn. Building is side-effect free for the biometric tables — the only
// write is the embedding-space identity row, which is idempotent
// configuration — so a validation failure cannot leave a half-created profile.
func (s *Service) speakerExtractionTasks(ctx context.Context, repo speakerPrivacyRepository, profile domain.SpeakerProfile, consent domain.SpeakerConsent, assetID, generationID, localSpeakerID string) ([]domain.InferenceTask, error) {
	if s.inferenceRepo == nil {
		return nil, fmt.Errorf("multimodal storage is not available")
	}
	turns, err := repo.ListSpeakerTurnsForGeneration(ctx, generationID)
	if err != nil {
		return nil, err
	}
	model, ok := inferencecatalog.Lookup("speaker-campplus")
	if !ok {
		return nil, fmt.Errorf("speaker model is not in the catalog")
	}
	space, err := buildSpace(domain.OpSpeakerEmbedding, model, 0)
	if err != nil {
		return nil, err
	}
	if err := s.inferenceRepo.UpsertEmbeddingSpace(ctx, space); err != nil {
		return nil, err
	}
	meta, err := s.repo.GetMediaMetadata(ctx, assetID)
	if err != nil || meta == nil {
		return nil, fmt.Errorf("asset duration is unknown")
	}
	var tasks []domain.InferenceTask
	for _, turn := range turns {
		if turn.AssetID != assetID || turn.LocalSpeakerID != localSpeakerID || turn.Overlap {
			continue
		}
		if turn.EndMS-turn.StartMS < speakerEligibleTurnMS {
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
				Interval:                &domain.TimeInterval{StartMS: turn.StartMS, EndMS: turn.EndMS},
				ProfileID:               profile.ID,
				DiarizationGenerationID: generationID,
			},
		}
		if err := request.Validate(); err != nil {
			return nil, err
		}
		key, err := (InferenceTaskKey{
			Operation:         domain.OpSpeakerEmbedding,
			SourceIdentity:    fmt.Sprintf("%s:%s:%s:%d-%d", assetID, profile.ID, generationID, turn.StartMS, turn.EndMS),
			ModelID:           model.ModelID,
			ModelRevision:     model.Revision,
			ConfigHash:        request.ConfigHash,
			SpaceID:           space.ID,
			ConsentGeneration: consent.Generation,
			ProfileGeneration: profile.Generation,
		}).Key()
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, domain.InferenceTask{
			AssetID:       assetID,
			TaskKey:       key,
			Operation:     domain.OpSpeakerEmbedding,
			RequestJSON:   string(encoded),
			ConfigHash:    request.ConfigHash,
			ModelRevision: model.Revision,
		})
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("speaker %q has no eligible clean speech of at least %d ms in generation %q", localSpeakerID, speakerEligibleTurnMS, generationID)
	}
	return tasks, nil
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
//
// The repository tombstones the profile, terminalizes its extraction jobs with
// the shared cancellation contract, and moves every ciphertext obligation to
// the purge worklist in one transaction; the drain under the biometric state
// lock then deletes the ciphertexts and clears their obligations. A deletion
// that fails keeps its obligation for the next run and is reported, never
// papered over.
func (s *Service) DeleteSpeakerProfile(ctx context.Context, profileID string) error {
	repo, err := s.speakerPrivacy()
	if err != nil {
		return err
	}
	release, err := s.lockBiometricState()
	if err != nil {
		return err
	}
	defer release()
	if _, err := repo.DeleteSpeakerProfile(ctx, profileID); err != nil {
		return err
	}
	return s.drainBiometricPurgeLocked(ctx, repo)
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
