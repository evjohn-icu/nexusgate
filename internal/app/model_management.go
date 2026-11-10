package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
)

// ModelManager executes one queued install/probe unit for a catalog entry.
// Installation moves bytes and verifies hashes; probing asks the runtime what
// it can actually load. Neither runs model inference.
type ModelManager interface {
	Install(ctx context.Context, task domain.ModelManagementTask) error
	Probe(ctx context.Context, task domain.ModelManagementTask) error
}

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
// probe succeeded at its job of finding out.
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
	// An attestation is the Hub's record that a locked profile was verified on
	// this Worker. Routing reads it; the Worker's own claim is never enough.
	if err := repo.RecordModelAttestation(ctx, unit.TargetWorkerID, unit.CatalogID, unit.ManifestDigest, unit.Action, "{}"); err != nil {
		return err
	}
	if err := p.repo.CompleteJob(ctx, j.ID, owner, domain.JobSucceeded, ""); err != nil {
		return err
	}
	return ErrJobCommittedEarly
}

// ModelManagementRepository is the durable side of maintenance jobs. It is a
// separate interface so a minimal repository used by tests does not have to
// implement the installer.
type ModelManagementRepository interface {
	GetModelManagementTask(ctx context.Context, jobID string) (domain.ModelManagementTask, error)
	RecordModelAttestation(ctx context.Context, workerID, catalogID, manifestDigest, action, detailJSON string) error
}

// modelManagementRepository is the optional persistence behind install/probe
// queueing. Asserted so a minimal service still compiles.
type modelManagementRepository interface {
	CreateModelManagementTask(ctx context.Context, task domain.ModelManagementTask, priority int) (string, error)
	GetModelManagementTask(ctx context.Context, jobID string) (domain.ModelManagementTask, error)
}

// ModelManifestDigest is the identity of one catalog entry as the installer
// sees it: the catalog id plus the immutable revision. A caller must present the
// same value it read from the catalog route, so a stale page cannot install a
// revision the operator did not see.
func ModelManifestDigest(entry inferencecatalog.Model) string {
	sum := sha256.Sum256([]byte(entry.CatalogID + "\x1f" + entry.ModelID + "\x1f" + entry.Revision + "\x1fmodels.lock.v1"))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// QueueModelManagement validates and enqueues one install or probe unit.
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
	if strings.TrimSpace(workerID) == "" {
		workerID = "local"
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
