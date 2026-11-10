package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

// ErrInferenceAlreadyCommitted answers a repeated result submission whose first
// response was lost. It is a success for the Worker: the bytes it is resending
// are already canonical, and writing them twice would be worse than dropping
// the retry.
var ErrInferenceAlreadyCommitted = errors.New("inference result already committed")

// SubmitWorkerInferenceResult validates and commits a Worker-produced result.
//
// The Hub is authoritative: it rechecks the request identity, the result shape,
// the digest the Worker computed, and (through Stage/Commit) the live lease.
// A Worker cannot publish by calling the ordinary complete endpoint, and a
// result that arrives after a lost lease writes nothing.
func (s *Service) SubmitWorkerInferenceResult(ctx context.Context, workerID, jobID string, submission remote.InferenceResultSubmission) error {
	if s.inferenceRepo == nil {
		return fmt.Errorf("multimodal storage is not available")
	}
	task, err := s.inferenceRepo.GetInferenceTask(ctx, jobID)
	if err != nil {
		return err
	}
	if task.StagedJSON != "" {
		// Already staged by an earlier attempt of this same submission. Compare
		// digests rather than overwriting: a different result under one
		// operation id is a protocol violation, not a retry.
		var staged domain.InferenceResult
		if err := json.Unmarshal([]byte(task.StagedJSON), &staged); err == nil {
			if existing, err := remote.DigestResult(staged); err == nil && existing == submission.Digest {
				return s.inferenceRepo.CommitInferenceTask(ctx, jobID, workerID)
			}
		}
		return fmt.Errorf("inference result already staged with different bytes")
	}
	var request domain.InferenceRequest
	if err := json.Unmarshal([]byte(task.RequestJSON), &request); err != nil {
		return fmt.Errorf("stored inference request is unreadable: %w", err)
	}
	if submission.OperationID != request.OperationID || submission.Result.OperationID != request.OperationID {
		return fmt.Errorf("inference result does not belong to this operation")
	}
	digest, err := remote.DigestResult(submission.Result)
	if err != nil {
		return err
	}
	if submission.Digest != "" && submission.Digest != digest {
		return fmt.Errorf("inference result digest mismatch")
	}
	dimension := 0
	if request.Operation.NeedsSpace() && request.SpaceID != "" {
		if space, err := s.inferenceRepo.GetEmbeddingSpace(ctx, request.SpaceID); err == nil {
			dimension = space.Dimension
		}
	}
	if err := submission.Result.ValidateFor(request, request.Input.DurationMS, dimension); err != nil {
		return fmt.Errorf("inference result is invalid: %w", err)
	}
	if err := s.inferenceRepo.StageInferenceTask(ctx, jobID, workerID, submission.Result); err != nil {
		if errors.Is(err, domain.ErrJobLeaseLost) {
			return err
		}
		return err
	}
	return s.inferenceRepo.CommitInferenceTask(ctx, jobID, workerID)
}

// WorkerInferenceProgress records a progress renewal and reports whether the
// lease is still owned plus whether cancellation was requested. The boolean
// result says whether jobID is an inference job at all; a derive job keeps the
// legacy 204 response.
func (s *Service) WorkerInferenceProgress(ctx context.Context, workerID, jobID, stage string, progress float64, event, message string) (remote.InferenceProgress, bool, error) {
	if s.inferenceRepo == nil {
		return remote.InferenceProgress{}, false, nil
	}
	task, err := s.inferenceRepo.GetInferenceTask(ctx, jobID)
	if err != nil {
		// Not an inference task: let the legacy handler answer 204/error.
		return remote.InferenceProgress{}, false, nil
	}
	if err := s.repo.RecordWorkerJobProgress(ctx, jobID, workerID, stage, progress, event, message); err != nil {
		if errors.Is(err, domain.ErrJobLeaseLost) {
			return remote.InferenceProgress{LeaseValid: false}, true, nil
		}
		return remote.InferenceProgress{}, true, err
	}
	return remote.InferenceProgress{
		CancelRequested: task.CancelRequested,
		LeaseExpiresAt:  time.Now().Add(InferenceLease).UTC().Format(time.RFC3339Nano),
		LeaseValid:      true,
	}, true, nil
}

// inferenceLeaseRepository is the optional repository capability a paired
// Worker needs. It is asserted rather than embedded so test repositories that
// only implement the derive contract keep compiling.
type inferenceLeaseRepository interface {
	LeaseNextWorkerInference(ctx context.Context, worker string, lease time.Duration, roots []string, ops []string, maxAssetBytes int64, now time.Time) (*remote.WorkerJob, error)
	LeaseNextModelManagement(ctx context.Context, worker string, lease time.Duration, now time.Time) (*remote.WorkerJob, error)
}

// LeaseNextWorkerInference leases the next inference or maintenance unit this
// Worker is authorized and able to execute. An empty result means there is
// nothing to do, not that the caller should fail.
func (s *Service) LeaseNextWorkerInference(ctx context.Context, worker remote.Worker, lease time.Duration, filter domain.LeaseFilter) (*remote.WorkerJob, error) {
	repo, ok := s.repo.(inferenceLeaseRepository)
	if !ok {
		return nil, nil
	}
	ops := authorizedInferenceOperations(worker.Capabilities)
	if len(ops) == 0 {
		return nil, nil
	}
	now := time.Now().UTC()
	job, err := repo.LeaseNextWorkerInference(ctx, worker.ID, lease, worker.Capabilities.LibraryRoots, ops, filter.MaxAssetBytes, now)
	if err != nil || job != nil {
		return job, err
	}
	if len(worker.Capabilities.ModelManagementOperations) == 0 {
		return nil, nil
	}
	return repo.LeaseNextModelManagement(ctx, worker.ID, lease, now)
}

// authorizedInferenceOperations intersects what the Worker claims with what its
// enrollment granted. A heartbeat can narrow availability but can never add an
// operation, so a compromised or misconfigured Worker cannot widen its own
// access by editing its heartbeat body.
func authorizedInferenceOperations(caps remote.WorkerCapabilities) []string {
	granted := make(map[string]bool, len(caps.InferenceOperations))
	for _, op := range caps.InferenceOperations {
		granted[op] = true
	}
	seen := make(map[string]bool)
	out := make([]string, 0, len(caps.Inference))
	for _, capability := range caps.Inference {
		operation := string(capability.Operation)
		if !granted[operation] || seen[operation] {
			continue
		}
		seen[operation] = true
		out = append(out, operation)
	}
	return out
}

// WorkerInferenceInput resolves the exact query-image bytes bound to a
// Worker's inference lease. The Worker never receives a path or URL: the Hub
// looks up the reference recorded in the task and streams the managed bytes.
func (s *Service) WorkerInferenceInput(ctx context.Context, workerID, jobID string) (remote.InferenceInputDescriptor, string, error) {
	if s.inferenceRepo == nil {
		return remote.InferenceInputDescriptor{}, "", fmt.Errorf("multimodal storage is not available")
	}
	status, err := s.repo.GetWorkerJobStatus(ctx, jobID)
	if err != nil {
		return remote.InferenceInputDescriptor{}, "", err
	}
	if status.State != domain.JobRunning || status.LeaseOwner != workerID {
		return remote.InferenceInputDescriptor{}, "", domain.ErrJobLeaseLost
	}
	task, err := s.inferenceRepo.GetInferenceTask(ctx, jobID)
	if err != nil {
		return remote.InferenceInputDescriptor{}, "", err
	}
	var request domain.InferenceRequest
	if err := json.Unmarshal([]byte(task.RequestJSON), &request); err != nil {
		return remote.InferenceInputDescriptor{}, "", err
	}
	if request.Input.Kind != domain.InputKindImage {
		return remote.InferenceInputDescriptor{}, "", fmt.Errorf("job has no image input")
	}
	ref, path, err := s.GetSearchReference(ctx, request.Input.ReferenceID)
	if err != nil {
		return remote.InferenceInputDescriptor{}, "", err
	}
	var size int64
	if info, statErr := os.Stat(path); statErr == nil {
		size = info.Size()
	}
	return remote.InferenceInputDescriptor{
		Kind:      domain.InputKindImage,
		MediaType: ref.MediaType,
		Digest:    ref.ContentHash,
		SizeBytes: size,
	}, path, nil
}
