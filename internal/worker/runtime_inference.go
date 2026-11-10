package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

// InferenceExecutor is the Worker-side contract for local model execution. It
// matches app.InferenceExecutor structurally; the Worker cannot import the app
// package (that would drag Hub configuration into a compute node binary).
type InferenceExecutor interface {
	Capabilities(context.Context) ([]remote.InferenceCapability, error)
	Execute(context.Context, domain.InferenceRequest) (domain.InferenceResult, error)
}

// progressStateReporter is implemented by Client.ProgressState. Older clients
// used for derive-only Workers do not need it.
type progressStateReporter interface {
	ProgressState(context.Context, string, string, string, float64, string, string) (remote.InferenceProgress, error)
}

// inferenceResultSubmitter is implemented by Client.SubmitInferenceResult.
type inferenceResultSubmitter interface {
	SubmitInferenceResult(context.Context, string, string, remote.InferenceResultSubmission) error
}

// WithInferenceExecutor attaches a local runtime. A Worker may run derive
// jobs, inference jobs or both; each capability is advertised independently.
func (r *Runtime) WithInferenceExecutor(exec InferenceExecutor) *Runtime {
	r.executor = exec
	return r
}

// inferenceRenewEvery is the progress/renewal cadence during a model run. The
// Hub grants a ten-minute lease and cancels execution when a renewal is
// rejected, so this interval is the upper bound on how long a lost lease can
// keep a subprocess alive.
const inferenceRenewEvery = 30 * time.Second

func (r *Runtime) processInference(ctx context.Context, job remote.WorkerJob) error {
	if r.executor == nil {
		return fmt.Errorf("worker has no inference executor")
	}
	if job.Inference == nil {
		return fmt.Errorf("inference lease carries no request")
	}
	request := *job.Inference
	if err := request.Validate(); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopRenewal := make(chan struct{})
	if reporter, ok := r.client.(progressStateReporter); ok {
		go r.renewInferenceLease(runCtx, cancel, job.JobID, stopRenewal, reporter)
	}
	defer close(stopRenewal)

	result, err := r.executor.Execute(runCtx, request)
	if err != nil {
		// A cancellation is the expected outcome of a Hub-initiated stop, not a
		// model failure: report the typed code so the job's failure record says
		// "cancelled" instead of blaming the model.
		if runCtx.Err() != nil {
			return &remote.InferenceFailure{Code: remote.FailureCancelled, Message: "inference cancelled by the Hub"}
		}
		return err
	}
	digest, err := remote.DigestResult(result)
	if err != nil {
		return err
	}
	submitter, ok := r.client.(inferenceResultSubmitter)
	if !ok {
		return fmt.Errorf("worker client cannot submit inference results")
	}
	submission := remote.InferenceResultSubmission{
		OperationID: request.OperationID,
		Digest:      digest,
		Result:      result,
	}
	if err := submitter.SubmitInferenceResult(ctx, r.config.Token, job.JobID, submission); err != nil {
		return err
	}
	return remote.ErrResultCommitted
}

// renewInferenceLease refreshes the lease and honours the Hub's cancellation
// answer. A rejected renewal cancels the operation immediately: continuing
// would burn device time on a result that the commit guard must reject.
func (r *Runtime) renewInferenceLease(ctx context.Context, cancel context.CancelFunc, jobID string, stop <-chan struct{}, reporter progressStateReporter) {
	ticker := time.NewTicker(inferenceRenewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			state, err := reporter.ProgressState(ctx, r.config.Token, jobID, "inference", 50, "progress", "模型推理进行中")
			if err != nil || !state.LeaseValid || state.CancelRequested {
				cancel()
				return
			}
		}
	}
}

// isCommittedResult reports whether the job already owns its terminal success.
func isCommittedResult(err error) bool {
	return errors.Is(err, remote.ErrResultCommitted)
}
