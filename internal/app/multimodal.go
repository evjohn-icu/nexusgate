package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/providers/shotdetect"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

// Inference lease duration. Inference is far slower than a derive stage, and a
// Worker renews every 30 seconds, so the initial window only has to survive the
// gap between two renewals, not a whole model run.
const (
	InferenceLease       = 10 * time.Minute
	inferenceRenewEvery  = 30 * time.Second
	inferenceTimeoutMS   = 10 * 60 * 1000
	inferenceMaxAttempts = 3
)

// WithInference attaches the executor and repository this pipeline uses for
// JobInference. Both are optional: a Hub with no local model still runs the
// derive/analyse chain, and every inference job in its queue stays leasable by
// a paired Worker.
func (p *Pipeline) WithInference(exec InferenceExecutor, repo InferenceRepository) *Pipeline {
	p.inferenceExec = exec
	p.inferenceRepo = repo
	return p
}

// inferenceConfigHash names the exact configuration that produced a
// generation. Thresholds, preprocessing and runtime identity are part of it,
// so changing a detector threshold produces a new generation instead of
// silently relabelling old rows.
func inferenceConfigHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return "cfg-" + hex.EncodeToString(sum[:])
}

// ShotSetHash is the identity of a canonical shot set. Visual vectors are
// rejected when the asset's shots changed since the request was built.
func ShotSetHash(shots []domain.AssetShot) string {
	ordered := append([]domain.AssetShot(nil), shots...)
	sortShots(ordered)
	var b strings.Builder
	for _, shot := range ordered {
		fmt.Fprintf(&b, "%s:%d:%d\n", shot.ID, shot.StartMS, shot.EndMS)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "shots-" + hex.EncodeToString(sum[:])
}

func sortShots(shots []domain.AssetShot) {
	for i := 1; i < len(shots); i++ {
		for j := i; j > 0 && (shots[j].StartMS < shots[j-1].StartMS ||
			(shots[j].StartMS == shots[j-1].StartMS && (shots[j].EndMS < shots[j-1].EndMS ||
				(shots[j].EndMS == shots[j-1].EndMS && shots[j].ID < shots[j-1].ID)))); j-- {
			shots[j], shots[j-1] = shots[j-1], shots[j]
		}
	}
}

// buildSpace constructs the space identity for a model/operation pair and
// returns it with its derived id.
func buildSpace(op domain.InferenceOperation, model inferencecatalog.Model, truncateTo int) (domain.EmbeddingSpace, error) {
	dimension := model.Dimension
	normalization := model.Normalization
	preprocessing := model.PreprocessingJSON
	if truncateTo > 0 {
		allowed := false
		for _, d := range model.Matryoshka {
			if d == truncateTo {
				allowed = true
				break
			}
		}
		if !allowed {
			return domain.EmbeddingSpace{}, fmt.Errorf("model %q does not declare a %d-dimensional truncation", model.CatalogID, truncateTo)
		}
		dimension = truncateTo
		preprocessing = fmt.Sprintf("%s;truncate_to=%d", preprocessing, truncateTo)
	}
	modality := domain.ModalityVisual
	switch op {
	case domain.OpAudioEmbedding:
		modality = domain.ModalityAudio
	case domain.OpSpeakerEmbedding:
		modality = domain.ModalitySpeaker
	}
	space := domain.EmbeddingSpace{
		Modality:          modality,
		Provider:          model.Provider,
		ModelID:           model.ModelID,
		ModelRevision:     model.Revision,
		Dimension:         dimension,
		Normalization:     normalization,
		PreprocessingJSON: preprocessing,
		RuntimeDigest:     model.RuntimeDigest,
	}
	id, err := CanonicalSpaceID(space)
	if err != nil {
		return domain.EmbeddingSpace{}, err
	}
	space.ID = id
	return space, nil
}

// modalitySource describes what an asset can currently offer one operation.
type modalitySource struct {
	assetID     string
	inputHash   string
	rootID      string
	relPath     string
	modifiedNS  int64
	durationMS  int64
	hasAudio    bool
	shots       []domain.AssetShot
	audioExists bool
}

// assetModalitySource loads the facts a request needs. Missing audio is not an
// error: it means the audio-driven operations are simply not enqueued for this
// asset.
func (s *Service) assetModalitySource(ctx context.Context, assetID, inputHash string) (modalitySource, error) {
	src := modalitySource{assetID: assetID, inputHash: inputHash}
	loc, err := s.repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		return src, err
	}
	src.rootID = loc.RootID
	src.relPath = loc.RelativePath
	src.modifiedNS = loc.ModifiedNS
	meta, err := s.repo.GetMediaMetadata(ctx, assetID)
	if err != nil {
		return src, err
	}
	if meta != nil {
		src.durationMS = meta.DurationMS
		src.hasAudio = meta.HasAudio
	}
	shots, err := s.repo.ListAssetShots(ctx, assetID)
	if err != nil {
		return src, err
	}
	src.shots = shots
	if artifact, err := s.repo.GetArtifact(ctx, assetID, "audio"); err == nil && artifact != nil && artifact.LocalPath != "" {
		src.audioExists = true
	}
	return src, nil
}

// PlanAssetModalities returns the inference tasks the enabled settings imply
// for one asset. It never enqueues; the caller decides whether the executor is
// local or a Worker.
func (s *Service) PlanAssetModalities(ctx context.Context, assetID, inputHash string) ([]domain.InferenceTask, error) {
	settings, err := s.inferenceRepo.GetMultimodalSettings(ctx)
	if err != nil {
		return nil, err
	}
	enabled := settings.EnabledOperations()
	if len(enabled) == 0 {
		return nil, nil
	}
	src, err := s.assetModalitySource(ctx, assetID, inputHash)
	if err != nil {
		return nil, err
	}
	var tasks []domain.InferenceTask
	for _, op := range enabled {
		task, ok := s.planAssetOperation(src, settings, op, false)
		if ok {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

// PlanAssetOperation builds one operation's task for an asset. force bypasses
// the "settings says this operation is enabled" check, which is what an admin
// rebuild means: the operator asked for this operation on this asset now,
// without turning the operation on for the whole library.
func (s *Service) PlanAssetOperation(ctx context.Context, assetID, inputHash string, op domain.InferenceOperation, force bool) (domain.InferenceTask, bool, error) {
	settings, err := s.inferenceRepo.GetMultimodalSettings(ctx)
	if err != nil {
		return domain.InferenceTask{}, false, err
	}
	src, err := s.assetModalitySource(ctx, assetID, inputHash)
	if err != nil {
		return domain.InferenceTask{}, false, err
	}
	task, ok := s.planAssetOperation(src, settings, op, force)
	return task, ok, nil
}

func (s *Service) planAssetOperation(src modalitySource, settings domain.MultimodalSettings, op domain.InferenceOperation, force bool) (domain.InferenceTask, bool) {
	if !force && !settings.Enabled[op] {
		return domain.InferenceTask{}, false
	}
	model, ok := s.modelForOperation(settings, op)
	if !ok {
		return domain.InferenceTask{}, false
	}
	var task domain.InferenceTask
	var err error
	switch op {
	case domain.OpVisualEmbedding:
		if len(src.shots) == 0 || src.durationMS <= 0 {
			return domain.InferenceTask{}, false
		}
		task, err = s.visualTask(src, model)
	case domain.OpSoundEvents:
		if !src.hasAudio || !src.audioExists {
			return domain.InferenceTask{}, false
		}
		task, err = s.assetTask(src, model, domain.OpSoundEvents)
	case domain.OpAudioEmbedding:
		if !src.hasAudio || !src.audioExists {
			return domain.InferenceTask{}, false
		}
		task, err = s.assetTask(src, model, domain.OpAudioEmbedding)
	case domain.OpDiarization:
		if !src.hasAudio || !src.audioExists {
			return domain.InferenceTask{}, false
		}
		task, err = s.assetTask(src, model, domain.OpDiarization)
	default:
		// Speaker embedding is never automatic: it is created by an explicit
		// profile action after consent, and only for eligible turns.
		return domain.InferenceTask{}, false
	}
	if err != nil {
		slog.Warn("inference task not built", "asset_id", src.assetID, "operation", op, "error", err)
		return domain.InferenceTask{}, false
	}
	return task, true
}

// modelForOperation resolves the configured profile, falling back to the
// catalog default. Unknown catalogue ids are ignored rather than guessed.
func (s *Service) modelForOperation(settings domain.MultimodalSettings, op domain.InferenceOperation) (inferencecatalog.Model, bool) {
	if id := strings.TrimSpace(settings.Profiles[string(op)]); id != "" {
		if model, ok := inferencecatalog.Lookup(id); ok {
			return model, true
		}
		return inferencecatalog.Model{}, false
	}
	return inferencecatalog.DefaultFor(op)
}

func (s *Service) visualTask(src modalitySource, model inferencecatalog.Model) (domain.InferenceTask, error) {
	space, err := buildSpace(domain.OpVisualEmbedding, model, 0)
	if err != nil {
		return domain.InferenceTask{}, err
	}
	if err := s.inferenceRepo.UpsertEmbeddingSpace(context.Background(), space); err != nil {
		return domain.InferenceTask{}, err
	}
	shots := make([]domain.VisualShot, 0, len(src.shots))
	for _, shot := range src.shots {
		points := media.PlanShotFrames(shot.StartMS, shot.EndMS, "boundary")
		timestamps := make([]int64, 0, len(points))
		for _, point := range points {
			ts := point.TimestampMS
			if ts >= shot.EndMS {
				ts = shot.EndMS - 1
			}
			timestamps = append(timestamps, ts)
		}
		shots = append(shots, domain.VisualShot{
			ShotID: shot.ID, StartMS: shot.StartMS, EndMS: shot.EndMS, TimestampsMS: timestamps,
		})
	}
	req := domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion,
		OperationID:   idgen.New(),
		Operation:     domain.OpVisualEmbedding,
		ModelID:       model.ModelID,
		ModelRevision: model.Revision,
		SpaceID:       space.ID,
		InputHash:     src.inputHash,
		ConfigHash:    inferenceConfigHash(string(domain.OpVisualEmbedding), model.Revision, space.PreprocessingJSON, model.RuntimeDigest, domain.InferenceSchemaVersion),
		TimeoutMS:     inferenceTimeoutMS,
		Input: domain.InferenceInput{
			Kind:         domain.InputKindAsset,
			AssetID:      src.assetID,
			RootID:       src.rootID,
			RelativePath: src.relPath,
			DurationMS:   src.durationMS,
		},
		Visual: &domain.VisualSettings{
			Strategy:    "shot_frame_pooled",
			ShotSetHash: ShotSetHash(src.shots),
			Shots:       shots,
		},
	}
	if err := req.Validate(); err != nil {
		return domain.InferenceTask{}, err
	}
	return s.inferenceTask(src, req, model)
}

func (s *Service) assetTask(src modalitySource, model inferencecatalog.Model, op domain.InferenceOperation) (domain.InferenceTask, error) {
	spaceID := ""
	spaceJSON := ""
	dimension := 0
	if op.NeedsSpace() {
		space, err := buildSpace(op, model, 0)
		if err != nil {
			return domain.InferenceTask{}, err
		}
		if err := s.inferenceRepo.UpsertEmbeddingSpace(context.Background(), space); err != nil {
			return domain.InferenceTask{}, err
		}
		spaceID = space.ID
		spaceJSON = space.PreprocessingJSON
		dimension = space.Dimension
	}
	_ = dimension
	req := domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion,
		OperationID:   idgen.New(),
		Operation:     op,
		ModelID:       model.ModelID,
		ModelRevision: model.Revision,
		SpaceID:       spaceID,
		InputHash:     src.inputHash,
		ConfigHash:    inferenceConfigHash(string(op), model.Revision, spaceJSON, model.PreprocessingJSON, model.RuntimeDigest, domain.InferenceSchemaVersion),
		TimeoutMS:     inferenceTimeoutMS,
		Input: domain.InferenceInput{
			Kind:         domain.InputKindAsset,
			AssetID:      src.assetID,
			RootID:       src.rootID,
			RelativePath: src.relPath,
			DurationMS:   src.durationMS,
		},
	}
	switch op {
	case domain.OpSoundEvents:
		req.SoundEvent = &domain.SoundEventSettings{}
	case domain.OpAudioEmbedding:
		req.Audio = &domain.AudioSettings{WindowMS: 10000, HopMS: 5000}
	case domain.OpDiarization:
		req.Diarization = &domain.DiarizationSettings{}
	default:
		return domain.InferenceTask{}, fmt.Errorf("asset task does not support operation %q", op)
	}
	if err := req.Validate(); err != nil {
		return domain.InferenceTask{}, err
	}
	return s.inferenceTask(src, req, model)
}

func (s *Service) inferenceTask(src modalitySource, req domain.InferenceRequest, model inferencecatalog.Model) (domain.InferenceTask, error) {
	key, err := (InferenceTaskKey{
		Operation:      req.Operation,
		SourceIdentity: src.inputHash + ":" + src.assetID,
		ModelID:        model.ModelID,
		ModelRevision:  model.Revision,
		ConfigHash:     req.ConfigHash,
		SpaceID:        req.SpaceID,
	}).Key()
	if err != nil {
		return domain.InferenceTask{}, err
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return domain.InferenceTask{}, err
	}
	return domain.InferenceTask{
		AssetID:       src.assetID,
		TaskKey:       key,
		Operation:     req.Operation,
		RequestJSON:   string(encoded),
		ConfigHash:    req.ConfigHash,
		ModelRevision: model.Revision,
	}, nil
}

// EnqueuePlannedModalities enqueues the planned tasks and the deterministic
// shot-detection job when visual indexing is enabled for an asset that has no
// canonical shots yet. Existing identical tasks are deduplicated by task_key.
func (s *Service) EnqueuePlannedModalities(ctx context.Context, assetID, inputHash string) {
	if s.pipeline == nil || s.pipeline.inferenceRepo == nil {
		return
	}
	settings, err := s.inferenceRepo.GetMultimodalSettings(ctx)
	if err != nil {
		slog.Warn("multimodal settings unreadable; skipping inference enqueue", "error", err)
		return
	}
	if settings.Enabled[domain.OpVisualEmbedding] {
		if shots, err := s.repo.ListAssetShots(ctx, assetID); err == nil && len(shots) == 0 {
			if err := s.repo.EnqueueJob(ctx, assetID, domain.JobDetectShots, hashStrings(inputHash, "detect-shots-v1"), 70); err != nil && !isDuplicateEnqueue(err) {
				slog.Warn("detect_shots enqueue failed", "asset_id", assetID, "error", err)
			}
		}
	}
	tasks, err := s.PlanAssetModalities(ctx, assetID, inputHash)
	if err != nil {
		slog.Warn("modality planning failed", "asset_id", assetID, "error", err)
		return
	}
	for _, task := range tasks {
		priority := 60
		if task.Operation == domain.OpDiarization {
			priority = 55
		}
		if _, err := s.pipeline.inferenceRepo.EnqueueInferenceTask(ctx, nil, task, priority); err != nil {
			slog.Warn("inference enqueue failed", "asset_id", assetID, "operation", task.Operation, "error", err)
		}
	}
}

// isDuplicateEnqueue reports whether an enqueue lost the deduplication race.
// The enqueue path itself is idempotent for inference tasks; for ordinary jobs
// the existing UNIQUE constraint is the same answer, not a failure.
func isDuplicateEnqueue(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}

// executeInference runs one inference task end to end. The canonical commit
// owns the successful terminal transition, so success returns
// ErrJobCommittedEarly and the pipeline must not complete the job again.
func (p *Pipeline) executeInference(ctx context.Context, j domain.Job, owner string) error {
	if p.inferenceExec == nil || p.inferenceRepo == nil {
		return domain.Permanent(fmt.Errorf("no inference executor is configured on this Hub"))
	}
	task, err := p.inferenceRepo.GetInferenceTask(ctx, j.ID)
	if err != nil {
		return err
	}
	if task.CancelRequested {
		return domain.Permanent(&remote.InferenceFailure{Code: remote.FailureCancelled, Message: "cancelled before execution"})
	}
	var req domain.InferenceRequest
	if err := json.Unmarshal([]byte(task.RequestJSON), &req); err != nil {
		return domain.Permanent(fmt.Errorf("inference task request is unreadable: %w", err))
	}
	if err := req.Validate(); err != nil {
		return domain.Permanent(err)
	}
	caps, err := p.inferenceExec.Capabilities(ctx)
	if err != nil {
		return err
	}
	if _, ok := CapabilityFor(caps, req); !ok {
		return domain.Permanent(&remote.InferenceFailure{
			Code:    remote.FailureModelUnavailable,
			Message: fmt.Sprintf("no available model for %s at revision %s", req.ModelID, req.ModelRevision),
		})
	}
	result, err := p.inferenceExec.Execute(ctx, req)
	if err != nil {
		return classifyInferenceError(err)
	}
	dimension := 0
	if req.Operation.NeedsSpace() && req.SpaceID != "" {
		if space, err := p.inferenceRepo.GetEmbeddingSpace(ctx, req.SpaceID); err == nil {
			dimension = space.Dimension
		}
	}
	if err := result.ValidateFor(req, req.Input.DurationMS, dimension); err != nil {
		return domain.Permanent(&remote.InferenceFailure{Code: remote.FailureInvalidOutput, Message: err.Error()})
	}
	if err := p.inferenceRepo.StageInferenceTask(ctx, j.ID, owner, result); err != nil {
		return err
	}
	if err := p.inferenceRepo.CommitInferenceTask(ctx, j.ID, owner); err != nil {
		return err
	}
	return ErrJobCommittedEarly
}

// classifyInferenceError maps a typed failure onto the job layer's retry
// policy. Only codes that can clear on their own stay retryable; a missing
// model or a malformed output would fail identically on every attempt.
func classifyInferenceError(err error) error {
	var failure *remote.InferenceFailure
	if errors.As(err, &failure) {
		if failure.Code.Retryable() {
			return err
		}
		return domain.Permanent(err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return err
}

// executeDetectShots produces bare canonical shots for visual indexing when no
// VLM analysis created them. It writes only timing and ordinal: no semantic
// claim is invented, and an existing canonical set is never overwritten.
func (p *Pipeline) executeDetectShots(ctx context.Context, j domain.Job, sourcePath string, owner string) error {
	existing, err := p.repo.ListAssetShots(ctx, j.AssetID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil
	}
	if p.shotDetector == nil {
		return domain.Permanent(fmt.Errorf("no shot detector is configured"))
	}
	meta, err := p.repo.GetMediaMetadata(ctx, j.AssetID)
	if err != nil {
		return err
	}
	if meta == nil || meta.DurationMS <= 0 {
		return domain.Permanent(fmt.Errorf("asset duration is unknown; cannot detect shots"))
	}
	bounds, err := p.shotDetector.Detect(ctx, sourcePath, meta.DurationMS)
	if err != nil {
		return err
	}
	normalized, err := shotdetect.Normalize(bounds, meta.DurationMS)
	if err != nil {
		return err
	}
	shots := make([]domain.AssetShot, 0, len(normalized))
	for i, bound := range normalized {
		shots = append(shots, domain.AssetShot{
			AssetID: j.AssetID,
			Ordinal: i,
			StartMS: bound.StartMS,
			EndMS:   bound.EndMS,
		})
	}
	if err := p.repo.ReplaceAssetShots(ctx, j.AssetID, "", shots, j.ID, owner); err != nil {
		return err
	}
	p.afterShotsCommitted(ctx, j.AssetID)
	return nil
}

// enqueueVisualModalities is the canonical-shot trigger. Visual indexing only
// needs the shot set, so it is independent of whether the VLM produced the
// shots or the deterministic detector did.
func (s *Service) enqueueVisualModalities(ctx context.Context, assetID string) {
	if s.inferenceRepo == nil {
		return
	}
	settings, err := s.inferenceRepo.GetMultimodalSettings(ctx)
	if err != nil || !settings.Enabled[domain.OpVisualEmbedding] {
		return
	}
	model, ok := s.modelForOperation(settings, domain.OpVisualEmbedding)
	if !ok {
		return
	}
	shots, err := s.repo.ListAssetShots(ctx, assetID)
	if err != nil || len(shots) == 0 {
		return
	}
	src, err := s.assetModalitySource(ctx, assetID, "v-"+ShotSetHash(shots))
	if err != nil {
		return
	}
	task, err := s.visualTask(src, model)
	if err != nil {
		slog.Warn("visual task not built", "asset_id", assetID, "error", err)
		return
	}
	if _, err := s.inferenceRepo.EnqueueInferenceTask(ctx, nil, task, 60); err != nil {
		slog.Warn("visual inference enqueue failed", "asset_id", assetID, "error", err)
	}
}

// enqueueAudioModalities is the derive trigger for sound events, audio
// vectors and diarization. Speaker embedding stays manual: it requires consent
// and an eligible profile, neither of which exists automatically.
func (s *Service) enqueueAudioModalities(ctx context.Context, assetID, inputHash string) {
	if s.inferenceRepo == nil {
		return
	}
	tasks, err := s.PlanAssetModalities(ctx, assetID, inputHash)
	if err != nil {
		slog.Warn("audio modality planning failed", "asset_id", assetID, "error", err)
		return
	}
	for _, task := range tasks {
		switch task.Operation {
		case domain.OpSoundEvents, domain.OpAudioEmbedding, domain.OpDiarization:
		default:
			continue
		}
		if _, err := s.inferenceRepo.EnqueueInferenceTask(ctx, nil, task, 55); err != nil {
			slog.Warn("audio inference enqueue failed", "asset_id", assetID, "operation", task.Operation, "error", err)
		}
	}
}
