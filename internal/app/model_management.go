package app

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

// ModelManager executes one queued install/probe unit for a catalog entry.
// Installation moves bytes and verifies hashes; probing asks the runtime what
// it can actually load. Neither runs model inference.
type ModelManager interface {
	Install(ctx context.Context, task domain.ModelManagementTask) error
	Probe(ctx context.Context, task domain.ModelManagementTask) error
}

// localModelTarget is the stable maintenance target for this Hub's own device.
// It is deliberately independent of the pipeline lease owner: a queued local
// install must survive a process restart, and the owner the pipeline leases
// under is minted fresh every start. The repository and the pipeline lease
// path both use this value.
const localModelTarget = "local"

// WithModelManager attaches the installer/prober. A Hub without one still
// queues maintenance jobs for Workers that have the runtime; it refuses to
// pretend a local install happened.
func (p *Pipeline) WithModelManager(manager ModelManager) *Pipeline {
	p.modelManager = manager
	return p
}

// executeModelManagement dispatches one maintenance job. The job's terminal
// transition is owned here, not by the generic completion path, because a
// probe may succeed while deliberately reporting an unavailable model: the
// probe succeeded at its job of finding out. The attestation is written by the
// repository's CompleteModelManagement from the task row it queued -- never
// from a Worker claim and never from the manager's in-memory state.
func (p *Pipeline) executeModelManagement(ctx context.Context, j domain.Job, owner string) error {
	if p.modelManager == nil {
		return domain.Permanent(fmt.Errorf("model management is not configured on this Hub"))
	}
	repo, ok := p.repo.(ModelManagementRepository)
	if !ok {
		return domain.Permanent(fmt.Errorf("repository cannot persist model management tasks"))
	}
	task, err := repo.GetModelManagementTask(ctx, j.ID)
	if err != nil {
		return err
	}
	unit := domain.ModelManagementTask{
		JobID:           task.JobID,
		TargetWorkerID:  task.TargetWorkerID,
		CatalogID:       task.CatalogID,
		ManifestDigest:  task.ManifestDigest,
		Action:          task.Action,
		LicenseAccepted: task.LicenseAccepted,
	}
	switch unit.Action {
	case "install":
		err = p.modelManager.Install(ctx, unit)
	case "probe":
		err = p.modelManager.Probe(ctx, unit)
	default:
		return domain.Permanent(fmt.Errorf("unknown model management action %q", unit.Action))
	}
	if err != nil {
		return err
	}
	if err := repo.CompleteModelManagement(ctx, j.ID, owner); err != nil {
		return err
	}
	return ErrJobCommittedEarly
}

// ModelManagementRepository is the durable side of maintenance jobs. It is a
// separate interface so a minimal repository used by tests does not have to
// implement the installer.
type ModelManagementRepository interface {
	GetModelManagementTask(ctx context.Context, jobID string) (domain.ModelManagementTask, error)
	// CompleteModelManagement validates the executing lease and writes the
	// success state and attestation atomically. owner is the current lease
	// owner: the Hub process's own local- identity for local targets, or the
	// executing Worker for Worker targets.
	CompleteModelManagement(ctx context.Context, jobID, owner string) error
}

// modelManagementRepository is the optional persistence behind install/probe
// queueing. Asserted so a minimal service still compiles.
type modelManagementRepository interface {
	CreateModelManagementTask(ctx context.Context, task domain.ModelManagementTask, priority int) (string, error)
	GetModelManagementTask(ctx context.Context, jobID string) (domain.ModelManagementTask, error)
	GetWorker(ctx context.Context, workerID string) (remote.Worker, error)
}

// ModelManifestDigest is the identity of one catalog entry as the installer
// sees it: the catalog id plus the immutable revision. A caller must present the
// same value it read from the catalog route, so a stale page cannot install a
// revision the operator did not see.
func ModelManifestDigest(entry inferencecatalog.Model) string {
	return entry.ManifestDigest()
}

// QueueModelManagement validates and enqueues one install or probe unit.
// workerID names the target device: a registered Worker granted the action,
// or empty/the stable local target for this Hub's own runtime.
func (s *Service) QueueModelManagement(ctx context.Context, action, catalogID, workerID, manifestDigest string, licenseAccepted bool) (string, error) {
	if action != "install" && action != "probe" {
		return "", fmt.Errorf("unknown model management action %q", action)
	}
	repo, ok := s.repo.(modelManagementRepository)
	if !ok {
		return "", fmt.Errorf("model management is not available")
	}
	entry, ok := inferencecatalog.Lookup(catalogID)
	if !ok {
		return "", fmt.Errorf("unknown model %q", catalogID)
	}
	expected := ModelManifestDigest(entry)
	if strings.TrimSpace(manifestDigest) != "" && manifestDigest != expected {
		return "", fmt.Errorf("manifest digest does not match the locked catalog entry")
	}
	if action == "install" && !licenseAccepted {
		return "", fmt.Errorf("install requires explicit licence acceptance")
	}
	workerID = strings.TrimSpace(workerID)
	if workerID != "" && workerID != localModelTarget {
		// An explicit Worker target is authorized at queue time: unknown,
		// revoked or action-ineligible targets are rejected before anything is
		// queued, so a task can never wait forever on a device that may
		// legitimately never execute it.
		worker, err := repo.GetWorker(ctx, workerID)
		if err != nil {
			return "", err
		}
		if worker.Status == remote.WorkerRevoked {
			return "", fmt.Errorf("worker %q is revoked", workerID)
		}
		if !slices.Contains(worker.Capabilities.ModelManagementOperations, action) {
			return "", fmt.Errorf("worker %q is not authorized for model management action %q", workerID, action)
		}
	} else {
		workerID = localModelTarget
	}
	task := domain.ModelManagementTask{
		TargetWorkerID:  workerID,
		CatalogID:       entry.CatalogID,
		ManifestDigest:  expected,
		Action:          action,
		LicenseAccepted: licenseAccepted,
	}
	return repo.CreateModelManagementTask(ctx, task, 50)
}

// ModelManagementStatus returns one maintenance unit and its job state.
func (s *Service) ModelManagementStatus(ctx context.Context, jobID string) (map[string]any, error) {
	repo, ok := s.repo.(modelManagementRepository)
	if !ok {
		return nil, fmt.Errorf("model management is not available")
	}
	task, err := repo.GetModelManagementTask(ctx, jobID)
	if err != nil {
		return nil, err
	}
	status, err := s.repo.GetWorkerJobStatus(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"task": task, "job": status}, nil
}
