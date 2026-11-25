package worker

import (
	"context"
	"fmt"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

// ModelManager executes one queued install/probe unit on this device. It is
// the Worker-side contract, structurally satisfied by the shared app manager
// (app.NewLocalModelManager); a Worker must never import the app package, so
// the interface travels instead of the implementation. Installation moves and
// verifies bytes; probing asks the runtime what it can actually load. Neither
// runs model inference.
type ModelManager interface {
	Install(context.Context, domain.ModelManagementTask) error
	Probe(context.Context, domain.ModelManagementTask) error
}

// WithModelManager attaches the installer/prober. A Worker without one can
// still derive and run inference; the Hub only leases maintenance units to it
// when the enrollment grant says it may, and the lease cannot be executed
// without this.
func (r *Runtime) WithModelManager(manager ModelManager) *Runtime {
	if r == nil {
		return r
	}
	r.modelManager = manager
	return r
}

// runModelManagement executes one leased maintenance unit. The lease carries
// the typed task; the Worker validates everything the Hub also checks on
// completion -- lease/task identity, action pairing, catalog/manifest
// identity and licence acceptance -- so a task queued under one lock can
// never be installed against a different one. The renewal goroutine keeps the
// lease alive through the ordinary progress endpoint and cancels the work the
// moment the Hub rejects a renewal.
//
// Success returns nil: the ordinary authenticated Complete(Succeeded) in
// processAndComplete/RunOnce records the terminal transition, and the Hub
// writes the attestation from the task row it queued. This hook must not call
// Complete itself, and it never supplies attestation fields.
func (r *Runtime) runModelManagement(ctx context.Context, job remote.WorkerJob) error {
	if r == nil || r.modelManager == nil {
		return fmt.Errorf("worker has no model manager")
	}
	if job.ModelManagement == nil {
		return fmt.Errorf("model management lease carries no task")
	}
	task := *job.ModelManagement
	if task.JobID != job.JobID {
		return domain.Permanent(fmt.Errorf("model management task %q does not match lease %q", task.JobID, job.JobID))
	}
	switch task.Action {
	case "install", "probe":
	default:
		return domain.Permanent(fmt.Errorf("unknown model management action %q", task.Action))
	}
	if (job.JobType == domain.JobModelInstall) != (task.Action == "install") {
		return domain.Permanent(fmt.Errorf("model management lease type %q does not match action %q", job.JobType, task.Action))
	}
	if task.CatalogID == "" || task.ManifestDigest == "" {
		return domain.Permanent(fmt.Errorf("model management task lacks a catalog identity or manifest digest"))
	}
	if task.Action == "install" && !task.LicenseAccepted {
		return domain.Permanent(fmt.Errorf("model %q requires explicit licence acceptance", task.CatalogID))
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if reporter, ok := r.client.(progressStateReporter); ok {
		stop, joined := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(joined)
			r.keepLease(runCtx, cancel, job.JobID, stop, reporter, leaseKeeperConfig{stage: "model_management", progress: 10, message: "模型维护进行中"})
		}()
		defer func() {
			close(stop)
			cancel()
			<-joined
		}()
	}

	unit := domain.ModelManagementTask{
		JobID:           task.JobID,
		TargetWorkerID:  task.TargetWorkerID,
		CatalogID:       task.CatalogID,
		ManifestDigest:  task.ManifestDigest,
		Action:          task.Action,
		LicenseAccepted: task.LicenseAccepted,
	}
	var err error
	if task.Action == "install" {
		err = r.modelManager.Install(runCtx, unit)
	} else {
		err = r.modelManager.Probe(runCtx, unit)
	}
	if runCtx.Err() != nil {
		return &remote.InferenceFailure{Code: remote.FailureCancelled, Message: "model management cancelled by the Hub"}
	}
	return err
}
