package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

// ErrJobCommittedEarly reports that an operation already published its
// canonical rows and moved its own job to succeeded inside the same
// transaction. The pipeline must not issue a second completion: doing so would
// either fail the compare-and-swap against a job that is no longer running, or
// — worse — overwrite a committed success with a generic failure.
//
// It is a sentinel rather than a bool because every existing execution path
// returns an error, and a lease-lost error must stay distinguishable from a
// committed success.
var ErrJobCommittedEarly = errors.New("job committed its own terminal success")

// InferenceExecutor is the consuming boundary for model execution. Both the
// Hub-local pipeline and a paired Worker's runtime call exactly this interface;
// the transport (subprocess, remote lease) is not visible here.
type InferenceExecutor interface {
	// Capabilities returns what this executor can run right now. An unavailable
	// model is reported as a capability with Available=false and a reason, not
	// as an error: "the model is missing" is a routing fact, not a failure.
	Capabilities(ctx context.Context) ([]remote.InferenceCapability, error)
	// Execute performs one validated operation. Implementations must reject
	// results that do not validate against the request before returning.
	Execute(ctx context.Context, req domain.InferenceRequest) (domain.InferenceResult, error)
}

// InferenceRepository owns the durable side of the inference boundary.
//
// Stage and Commit re-check the live lease (job id + owner + running state +
// unexpired lease) and the request identity against the stored task row, so a
// late result from a lost lease can never publish. Commit writes every typed
// row, swaps the modality head and marks the job succeeded in one transaction.
type InferenceRepository interface {
	// EnqueueInferenceTask inserts the job and its task row atomically. A nil
	// jobID means "allocate one"; a non-nil value is used by maintenance jobs
	// that already created their job. The task_key unique constraint makes a
	// repeated enqueue of the same identity return the existing job instead of
	// creating a second one.
	EnqueueInferenceTask(ctx context.Context, jobID *string, task domain.InferenceTask, priority int) (string, error)
	GetInferenceTask(ctx context.Context, jobID string) (domain.InferenceTask, error)
	StageInferenceTask(ctx context.Context, jobID, owner string, result domain.InferenceResult) error
	CommitInferenceTask(ctx context.Context, jobID, owner string) error
	CancelInferenceTask(ctx context.Context, jobID string) error

	GetMultimodalSettings(ctx context.Context) (domain.MultimodalSettings, error)
	PutMultimodalSettings(ctx context.Context, settings domain.MultimodalSettings) error

	UpsertEmbeddingSpace(ctx context.Context, space domain.EmbeddingSpace) error
	GetEmbeddingSpace(ctx context.Context, id string) (domain.EmbeddingSpace, error)

	// ActiveGeneration returns the committed generation for the
	// (asset, operation, config) triple, or domain.ErrNotFound.
	ActiveGeneration(ctx context.Context, assetID string, op domain.InferenceOperation, configHash string) (domain.ModalityGeneration, error)
}

// InferenceTaskKey is the deduplication identity of one inference unit.
//
// It hashes every input that changes the result: the operation, the exact
// source identity (asset content hash or query bytes digest), the immutable
// model revision, the preprocessing/runtime/dimension identity and, where it
// applies, the consent/profile generation. Two different answers under one key
// would be indistinguishable in the canonical store, so nothing that changes
// the answer may be omitted here.
type InferenceTaskKey struct {
	Operation      domain.InferenceOperation
	SourceIdentity string
	ModelID        string
	ModelRevision  string
	ConfigHash     string
	SpaceID        string
	// ConsentGeneration and ProfileGeneration are zero for every
	// non-biometric operation.
	ConsentGeneration int64
	ProfileGeneration int64
}

// Key returns the canonical SHA-256 of the identity, prefixed so a stored key
// cannot be confused with an asset hash.
func (k InferenceTaskKey) Key() (string, error) {
	if !k.Operation.Valid() {
		return "", fmt.Errorf("task key: unsupported operation %q", k.Operation)
	}
	if strings.TrimSpace(k.SourceIdentity) == "" {
		return "", fmt.Errorf("task key: source identity is required")
	}
	if strings.TrimSpace(k.ModelRevision) == "" || strings.TrimSpace(k.ConfigHash) == "" {
		return "", fmt.Errorf("task key: model revision and config hash are required")
	}
	payload := struct {
		Operation         domain.InferenceOperation `json:"operation"`
		SourceIdentity    string                    `json:"source_identity"`
		ModelID           string                    `json:"model_id"`
		ModelRevision     string                    `json:"model_revision"`
		ConfigHash        string                    `json:"config_hash"`
		SpaceID           string                    `json:"space_id,omitempty"`
		ConsentGeneration int64                     `json:"consent_generation,omitempty"`
		ProfileGeneration int64                     `json:"profile_generation,omitempty"`
	}{
		Operation:         k.Operation,
		SourceIdentity:    k.SourceIdentity,
		ModelID:           k.ModelID,
		ModelRevision:     k.ModelRevision,
		ConfigHash:        k.ConfigHash,
		SpaceID:           k.SpaceID,
		ConsentGeneration: k.ConsentGeneration,
		ProfileGeneration: k.ProfileGeneration,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("task key: encode identity: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "inf-" + hex.EncodeToString(sum[:]), nil
}

// CanonicalSpaceID is the SHA-256 of the canonical JSON identity of a space.
// The ID is derived, never assigned, so two installations that agree on every
// field agree on the space and can exchange vectors.
func CanonicalSpaceID(space domain.EmbeddingSpace) (string, error) {
	if !space.Modality.Valid() {
		return "", fmt.Errorf("space: unknown modality %q", space.Modality)
	}
	if space.Dimension <= 0 {
		return "", fmt.Errorf("space: dimension must be positive")
	}
	if strings.TrimSpace(space.ModelRevision) == "" {
		return "", fmt.Errorf("space: immutable model revision is required")
	}
	payload := struct {
		Modality          domain.Modality `json:"modality"`
		Provider          string          `json:"provider"`
		ModelID           string          `json:"model_id"`
		ModelRevision     string          `json:"model_revision"`
		Dimension         int             `json:"dimension"`
		Normalization     string          `json:"normalization"`
		PreprocessingJSON string          `json:"preprocessing_json"`
		RuntimeDigest     string          `json:"runtime_digest"`
	}{
		Modality:          space.Modality,
		Provider:          space.Provider,
		ModelID:           space.ModelID,
		ModelRevision:     space.ModelRevision,
		Dimension:         space.Dimension,
		Normalization:     space.Normalization,
		PreprocessingJSON: space.PreprocessingJSON,
		RuntimeDigest:     space.RuntimeDigest,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("space: encode identity: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "space-" + hex.EncodeToString(sum[:]), nil
}

// SortedCapabilities returns capabilities in a stable order so route decisions
// and tests do not depend on map iteration.
func SortedCapabilities(caps []remote.InferenceCapability) []remote.InferenceCapability {
	out := append([]remote.InferenceCapability(nil), caps...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Operation != out[j].Operation {
			return out[i].Operation < out[j].Operation
		}
		return out[i].ConfigHash < out[j].ConfigHash
	})
	return out
}

// CapabilityFor returns the available capability that can execute req.
func CapabilityFor(caps []remote.InferenceCapability, req domain.InferenceRequest) (remote.InferenceCapability, bool) {
	for _, cap := range caps {
		if cap.Matches(req) {
			return cap, true
		}
	}
	return remote.InferenceCapability{}, false
}

// AttachInferenceExecutor binds a local runtime to the service and its
// pipeline. Called by the serve command after the runtime has been probed; a
// Hub that never calls it keeps inference work leasable by paired Workers.
func (s *Service) AttachInferenceExecutor(exec InferenceExecutor) {
	if s == nil {
		return
	}
	s.inferenceExec = exec
	if s.pipeline != nil {
		s.pipeline.WithInference(exec, s.inferenceRepo)
	}
}

// InferenceExecutorStatus reports whether this Hub can execute inference
// locally. It is safe on a nil service so callers do not need a nil check.
func (s *Service) InferenceExecutorStatus() bool {
	return s != nil && s.inferenceExec != nil
}

// AttachModelManager binds the local installer/prober to the pipeline. A Hub
// without one still queues maintenance work for a Worker that has the runtime.
func (s *Service) AttachModelManager(manager ModelManager) {
	if s == nil || s.pipeline == nil {
		return
	}
	s.pipeline.WithModelManager(manager)
}
