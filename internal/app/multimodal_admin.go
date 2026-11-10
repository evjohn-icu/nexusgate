package app

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
)

// GetMultimodalSettings returns the singleton operator configuration, with
// every operation disabled when no row exists yet.
func (s *Service) GetMultimodalSettings(ctx context.Context) (domain.MultimodalSettings, error) {
	if s.inferenceRepo == nil {
		return domain.DefaultMultimodalSettings(), nil
	}
	return s.inferenceRepo.GetMultimodalSettings(ctx)
}

// SaveMultimodalSettings validates and persists the operator configuration,
// then queues missing work for assets that already exist. Turning an operation
// on is the operator saying "index the shelf I have", not "index only what is
// imported from now on", so the backfill is part of the same request. It is
// deduplicated by task_key, so pressing save twice costs nothing.
func (s *Service) SaveMultimodalSettings(ctx context.Context, settings domain.MultimodalSettings) (domain.MultimodalSettings, error) {
	if s.inferenceRepo == nil {
		return domain.MultimodalSettings{}, fmt.Errorf("multimodal storage is not available")
	}
	normalized, err := normalizeMultimodalSettings(settings)
	if err != nil {
		return domain.MultimodalSettings{}, err
	}
	if err := s.inferenceRepo.PutMultimodalSettings(ctx, normalized); err != nil {
		return domain.MultimodalSettings{}, err
	}
	if len(normalized.EnabledOperations()) > 0 {
		if err := s.enqueueMissingModalities(ctx); err != nil {
			// The settings write already succeeded; a backfill failure is
			// reported but must not roll the operator's choice back.
			slog.Warn("multimodal backfill failed after settings save", "error", err)
		}
	}
	return s.inferenceRepo.GetMultimodalSettings(ctx)
}

func normalizeMultimodalSettings(settings domain.MultimodalSettings) (domain.MultimodalSettings, error) {
	out := domain.DefaultMultimodalSettings()
	for op, enabled := range settings.Enabled {
		if !op.Valid() {
			return domain.MultimodalSettings{}, fmt.Errorf("unknown operation %q", op)
		}
		out.Enabled[op] = enabled
	}
	for op, catalogID := range settings.Profiles {
		operation := domain.InferenceOperation(op)
		if !operation.Valid() {
			return domain.MultimodalSettings{}, fmt.Errorf("unknown operation %q in profile selection", op)
		}
		catalogID = strings.TrimSpace(catalogID)
		if catalogID == "" {
			continue
		}
		model, ok := inferencecatalog.Lookup(catalogID)
		if !ok {
			return domain.MultimodalSettings{}, fmt.Errorf("unknown model %q", catalogID)
		}
		capable := false
		for _, candidate := range model.Operations {
			if candidate == operation {
				capable = true
				break
			}
		}
		if !capable {
			return domain.MultimodalSettings{}, fmt.Errorf("model %q cannot execute %q", catalogID, op)
		}
		out.Profiles[string(operation)] = catalogID
	}
	return out, nil
}

// enqueueMissingModalities walks the library and queues every planned task the
// current settings imply. It is deliberately sequential and bounded by the
// existing task_key dedupe rather than by a "already indexed" heuristic: a
// revision change must re-index, and only the key knows that.
func (s *Service) enqueueMissingModalities(ctx context.Context) error {
	assets, err := s.repo.ListAssets(ctx, 1<<31-1, 0)
	if err != nil {
		return err
	}
	sorted := append([]domain.Asset(nil), assets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, asset := range sorted {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.EnqueuePlannedModalities(ctx, asset.ID, "backfill-v1")
	}
	return nil
}

// RebuildAssetModality queues one operation for one asset regardless of the
// library-wide switch. Existing valid data for other operations is untouched:
// the rebuild writes a new generation for exactly one (asset, operation).
func (s *Service) RebuildAssetModality(ctx context.Context, assetID string, op domain.InferenceOperation) (string, error) {
	if s.inferenceRepo == nil {
		return "", fmt.Errorf("multimodal storage is not available")
	}
	if !op.Valid() {
		return "", fmt.Errorf("unknown operation %q", op)
	}
	if op == domain.OpSpeakerEmbedding {
		return "", fmt.Errorf("speaker embeddings are created by a profile action, not a rebuild")
	}
	if op == domain.OpVisualEmbedding {
		// A rebuild must not pay for a VLM pass: reuse canonical shots, or ask
		// the deterministic detector to create bare ones.
		shots, err := s.repo.ListAssetShots(ctx, assetID)
		if err != nil {
			return "", err
		}
		if len(shots) == 0 {
			if err := s.repo.EnqueueJob(ctx, assetID, domain.JobDetectShots, hashStrings("rebuild-visual", assetID), 70); err != nil {
				return "", err
			}
			return "", nil
		}
	}
	inputHash := "rebuild-" + string(op) + "-v1"
	task, ok, err := s.PlanAssetOperation(ctx, assetID, inputHash, op, true)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("asset is not eligible for %q", op)
	}
	return s.inferenceRepo.EnqueueInferenceTask(ctx, nil, task, 75)
}

// CancelInferenceTask requests cancellation of a queued or running inference
// job. A running executor observes the flag through its progress renewals; a
// pending job is failed immediately by the repository.
func (s *Service) CancelInferenceTask(ctx context.Context, jobID string) error {
	if s.inferenceRepo == nil {
		return fmt.Errorf("multimodal storage is not available")
	}
	return s.inferenceRepo.CancelInferenceTask(ctx, jobID)
}
