package app

import (
	"context"
	"fmt"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// ModelManager executes one queued install/probe unit for a catalog entry.
// Installation moves bytes and verifies hashes; probing asks the runtime what
// it can actually load. Neither runs model inference.
type ModelManager interface {
	Install(ctx context.Context, task ModelManagementTask) error
	Probe(ctx context.Context, task ModelManagementTask) error
}

// ModelManagementTask is the app-level view of a maintenance unit; the remote
// package mirrors it on the wire.
type ModelManagementTask struct {
	JobID           string
	TargetWorkerID  string
	CatalogID       string
	ManifestDigest  string
	Action          string
	LicenseAccepted bool
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
	unit := ModelManagementTask{
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
	if err := p.repo.CompleteJob(ctx, j.ID, owner, domain.JobSucceeded, ""); err != nil {
		return err
	}
	return ErrJobCommittedEarly
}

// ModelManagementRepository is the durable side of maintenance jobs. It is a
// separate interface so a minimal repository used by tests does not have to
// implement the installer.
type ModelManagementRepository interface {
	GetModelManagementTask(ctx context.Context, jobID string) (ModelManagementTaskRow, error)
}

// ModelManagementTaskRow is the stored form.
type ModelManagementTaskRow struct {
	JobID           string
	TargetWorkerID  string
	CatalogID       string
	ManifestDigest  string
	Action          string
	LicenseAccepted bool
}
