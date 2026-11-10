package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
	"github.com/evjohn-icu/nexusgate/internal/search"
)

// queryVectorRepository is the optional cache the encoder reads.
type queryVectorRepository interface {
	QueryReferenceVector(ctx context.Context, referenceID, spaceID string) ([]float32, bool, error)
	QueryReferenceJob(ctx context.Context, referenceID string) (string, bool, error)
}

// modalityQueryEncoder answers search requests with vectors that were already
// produced by an authorised inference task. It never runs a model on the read
// path: an unencoded text query is a pending authorization, not a reason to
// spend device time for whoever asks.
type modalityQueryEncoder struct {
	service *Service
}

var _ search.ModalityQueryEncoder = (*modalityQueryEncoder)(nil)

func (e *modalityQueryEncoder) Encode(ctx context.Context, modality, text, referenceID string) (string, []float32, error) {
	if e == nil || e.service == nil {
		return "", nil, fmt.Errorf("%w: search is not wired to an inference runtime", search.ErrModalitiesUnavailable)
	}
	var operation domain.InferenceOperation
	switch modality {
	case search.ModalityVisual:
		operation = domain.OpVisualEmbedding
	case search.ModalityAudio:
		operation = domain.OpAudioEmbedding
	default:
		return "", nil, fmt.Errorf("%w: %s has no text/image encoder", search.ErrModalitiesUnavailable, modality)
	}
	model, ok := inferencecatalog.DefaultFor(operation)
	if !ok {
		return "", nil, fmt.Errorf("%w: no locked model for %s", search.ErrModalitiesUnavailable, modality)
	}
	space, err := buildSpace(operation, model, 0)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(referenceID) == "" {
		return "", nil, fmt.Errorf("%w: text query embeddings must be requested by a Hub administrator; an unencoded free-text query is not executed on the read path", search.ErrModalitiesUnavailable)
	}
	repo, ok := e.service.repo.(queryVectorRepository)
	if !ok {
		return "", nil, fmt.Errorf("%w: query vectors are not stored by this Hub", search.ErrModalitiesUnavailable)
	}
	vector, ready, err := repo.QueryReferenceVector(ctx, referenceID, space.ID)
	if err != nil {
		return "", nil, err
	}
	if ready && len(vector) > 0 {
		return space.ID, vector, nil
	}
	jobID, pending, _ := repo.QueryReferenceJob(ctx, referenceID)
	if pending {
		return "", nil, fmt.Errorf("%w: query image is still being encoded (job %s)", search.ErrModalitiesUnavailable, jobID)
	}
	return "", nil, fmt.Errorf("%w: query image has not been submitted for encoding", search.ErrModalitiesUnavailable)
}

// enqueueReferenceQuery queues the assetless visual task for a query image.
// It runs at reference creation, where the caller is already an authenticated
// administrator, so no read-path request can cause model work.
func (s *Service) enqueueReferenceQuery(ctx context.Context, ref domain.SearchReference) {
	if s.inferenceRepo == nil || s.inferenceExec == nil {
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
	space, err := buildSpace(domain.OpVisualEmbedding, model, 0)
	if err != nil {
		return
	}
	if err := s.inferenceRepo.UpsertEmbeddingSpace(ctx, space); err != nil {
		return
	}
	request := domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion,
		OperationID:   ref.ID,
		Operation:     domain.OpVisualEmbedding,
		ModelID:       model.ModelID,
		ModelRevision: model.Revision,
		SpaceID:       space.ID,
		InputHash:     ref.ContentHash,
		ConfigHash:    inferenceConfigHash(string(domain.OpVisualEmbedding), model.Revision, space.PreprocessingJSON, model.RuntimeDigest, domain.InferenceSchemaVersion),
		TimeoutMS:     inferenceTimeoutMS,
		Input: domain.InferenceInput{
			Kind:        domain.InputKindImage,
			ReferenceID: ref.ID,
			Digest:      ref.ContentHash,
		},
		Visual: &domain.VisualSettings{Strategy: "shot_frame_pooled"},
	}
	if err := request.Validate(); err != nil {
		return
	}
	key, err := (InferenceTaskKey{
		Operation:      domain.OpVisualEmbedding,
		SourceIdentity: "image:" + ref.ContentHash,
		ModelID:        model.ModelID,
		ModelRevision:  model.Revision,
		ConfigHash:     request.ConfigHash,
		SpaceID:        space.ID,
	}).Key()
	if err != nil {
		return
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return
	}
	task := domain.InferenceTask{
		TaskKey:          key,
		Operation:        domain.OpVisualEmbedding,
		RequestJSON:      string(encoded),
		ConfigHash:       request.ConfigHash,
		ModelRevision:    model.Revision,
		QueryReferenceID: ref.ID,
	}
	// Enqueue failure is not fatal to creating the reference: the search will
	// report the image as pending, which is the truth.
	_, _ = s.inferenceRepo.EnqueueInferenceTask(ctx, nil, task, 30)
}
