package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// stagedQueryImageContextKey is the unexported context key under which the
// executing inference job publishes its verified staged query images. Because
// the key is unexported and the value only enters the context that is handed
// to executor.Execute, a resolver call outside that job context — another
// job, another goroutine, or the Hub-side executor — can never see this
// Worker's staged bytes.
type stagedQueryImageContextKey struct{}

// stagedQueryImage is one verified lease-bound query image.
type stagedQueryImage struct {
	digest string
	path   string
}

// stagedQueryImages is the per-job (reference ID, digest) map the executing
// job publishes into its execution context. The lease grants access to
// exactly one image; the map shape keeps the resolver API general without
// widening what a job may publish.
type stagedQueryImages struct {
	// dir is the per-job scratch directory under the cache inference root;
	// release removes it and everything staged inside.
	dir      string
	entries  map[string]stagedQueryImage
	released atomic.Bool
}

// release removes the staged scratch. It must run on every job outcome —
// success, failure and cancellation alike — so a cancelled or failed run
// never leaves query pixels on disk.
func (s *stagedQueryImages) release() {
	if s != nil && !s.released.Swap(true) {
		_ = os.RemoveAll(s.dir)
	}
}

// withStagedQueryImages publishes the job's staged image map under the
// unexported job key. Callers derive exactly one execution context per job
// and pass it only to executor.Execute.
func withStagedQueryImages(ctx context.Context, staged *stagedQueryImages) context.Context {
	return context.WithValue(ctx, stagedQueryImageContextKey{}, staged)
}

// ResolveInferenceImage maps a lease-bound query image (reference ID, digest)
// pair to the verified staged path published by the executing job's context.
// The CLI installs it directly as the local inference executor's
// contextual ResolveImagePath: the executor receives the derived context, so
// resolution is impossible without the staging this runtime performed for the
// very same lease.
func ResolveInferenceImage(ctx context.Context, referenceID, digest string) (string, error) {
	staged, ok := ctx.Value(stagedQueryImageContextKey{}).(*stagedQueryImages)
	if !ok || staged == nil || staged.released.Load() || ctx.Err() != nil {
		return "", fmt.Errorf("query image %q is not staged in this job context", referenceID)
	}
	entry, ok := staged.entries[referenceID]
	if !ok {
		return "", fmt.Errorf("query image %q is not staged in this job context", referenceID)
	}
	if entry.digest != digest {
		return "", fmt.Errorf("query image %q digest does not match the staged bytes", referenceID)
	}
	return entry.path, nil
}

// imageInputFetcher is implemented by Client.FetchInferenceInput. Older
// clients predating the lease-input endpoint cannot serve image inputs.
type imageInputFetcher interface {
	FetchInferenceInput(context.Context, string, string) ([]byte, error)
}

// maxInferenceInputBytes matches the bound the Hub's lease-input endpoint
// serves: 8 MiB of image plus framing headroom, the same ceiling the query
// reference upload enforces. The fetch is rejected before anything is written
// if the response is larger or empty.
const maxInferenceInputBytes = 9 << 20

// stageQueryImage fetches the query image bound to this job's lease through
// the authenticated lease-input endpoint, verifies its content digest against
// the request before anything else happens, and writes the verified bytes
// under the cache inference scratch for this job. The returned value is
// published to the executor only via the job's execution context and removed
// by release on every outcome.
func (r *Runtime) stageQueryImage(ctx context.Context, jobID string, input domain.InferenceInput) (*stagedQueryImages, error) {
	fetcher, ok := r.client.(imageInputFetcher)
	if !ok {
		return nil, &remote.InferenceFailure{Code: remote.FailureUnsupportedInput, Message: "worker client cannot fetch lease-bound inference inputs"}
	}
	body, err := fetcher.FetchInferenceInput(ctx, r.config.Token, jobID)
	if err != nil {
		return nil, fmt.Errorf("fetch inference input: %w", err)
	}
	if len(body) == 0 {
		return nil, &remote.InferenceFailure{Code: remote.FailureUnsupportedInput, Message: "fetched inference input is empty"}
	}
	if int64(len(body)) > maxInferenceInputBytes {
		return nil, &remote.InferenceFailure{Code: remote.FailureUnsupportedInput, Message: "fetched inference input exceeds the lease-input bound"}
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if digest != input.Digest {
		return nil, &remote.InferenceFailure{Code: remote.FailureUnsupportedInput, Message: "fetched query image does not match the request digest"}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := filepath.Join(r.config.CachePath(), "inference", "inputs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create inference input root: %w", err)
	}
	dir, err := os.MkdirTemp(root, sanitizePathSegment(jobID)+"-")
	if err != nil {
		return nil, fmt.Errorf("create inference input scratch: %w", err)
	}
	path := filepath.Join(dir, "query-image")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("write staged query image: %w", err)
	}
	return &stagedQueryImages{
		dir: dir,
		entries: map[string]stagedQueryImage{
			input.ReferenceID: {digest: digest, path: path},
		},
	}, nil
}

// sanitizePathSegment reduces a job ID to a single safe path component so the
// scratch directory name can never escape the inference inputs root.
func sanitizePathSegment(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "unknown-job"
	}
	return builder.String()
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

// WithInferenceRenewInterval overrides the progress/renewal cadence. Tests use
// a short interval; production keeps the 30-second default, which bounds how
// long a lost lease can keep a subprocess alive.
func (r *Runtime) WithInferenceRenewInterval(interval time.Duration) *Runtime {
	if interval > 0 {
		r.inferenceRenew = interval
	}
	return r
}

func (r *Runtime) renewInterval() time.Duration {
	if r.inferenceRenew > 0 {
		return r.inferenceRenew
	}
	return inferenceRenewDefault
}

// inferenceRenewDefault is the progress/renewal cadence during a model run. The
// Hub grants a ten-minute lease and cancels execution when a renewal is
// rejected, so this interval is the upper bound on how long a lost lease can
// keep a subprocess alive.
const inferenceRenewDefault = 30 * time.Second

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

	// A lease-bound query image is fetched and digest-verified before the
	// renewal loop starts, so a staging failure aborts the job without
	// leaving a renewal goroutine behind. The verified path is published only
	// through the derived execution context handed to executor.Execute: the
	// executor resolves it via ResolveInferenceImage, and any resolver call
	// outside this job's context fails instead of exposing Hub paths or
	// another job's pixels. Release runs on every outcome, including
	// cancellation and a failed submission.
	var staged *stagedQueryImages
	if request.Input.Kind == domain.InputKindImage {
		images, stageErr := r.stageQueryImage(ctx, job.JobID, request.Input)
		if stageErr != nil {
			return stageErr
		}
		staged = images
		defer staged.release()
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if reporter, ok := r.client.(progressStateReporter); ok {
		stopRenewal, joined := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(joined)
			r.renewInferenceLease(runCtx, cancel, job.JobID, stopRenewal, reporter)
		}()
		defer func() {
			close(stopRenewal)
			cancel()
			<-joined
		}()
	}

	execCtx := runCtx
	if staged != nil {
		execCtx = withStagedQueryImages(runCtx, staged)
	}

	result, err := r.executor.Execute(execCtx, request)
	if runCtx.Err() != nil {
		return &remote.InferenceFailure{Code: remote.FailureCancelled, Message: "inference cancelled by the Hub"}
	}
	if err != nil {
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
	if err := submitter.SubmitInferenceResult(runCtx, r.config.Token, job.JobID, submission); err != nil {
		return err
	}
	return remote.ErrResultCommitted
}

// leaseKeeperConfig parameterizes the shared lease-renewal loop: the stage
// label, progress and message each renewal reports on the existing progress
// endpoint. The Hub's typed answer governs cancellation regardless of labels.
type leaseKeeperConfig struct {
	stage    string
	progress float64
	message  string
}

// keepLease refreshes a long-running lease on the existing progress endpoint
// and honours the Hub's cancellation answer. A rejected renewal cancels the
// operation immediately: continuing would burn device time on work whose
// result the commit guard must reject. Inference execution and model
// maintenance (runtime_models.go) share this loop; only the labels differ.
func (r *Runtime) keepLease(ctx context.Context, cancel context.CancelFunc, jobID string, stop <-chan struct{}, reporter progressStateReporter, cfg leaseKeeperConfig) {
	ticker := time.NewTicker(r.renewInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			state, err := reporter.ProgressState(ctx, r.config.Token, jobID, cfg.stage, cfg.progress, "progress", cfg.message)
			if err != nil || !state.LeaseValid || state.CancelRequested {
				cancel()
				return
			}
		}
	}
}

// renewInferenceLease refreshes the inference lease with the inference stage
// labels.
func (r *Runtime) renewInferenceLease(ctx context.Context, cancel context.CancelFunc, jobID string, stop <-chan struct{}, reporter progressStateReporter) {
	r.keepLease(ctx, cancel, jobID, stop, reporter, leaseKeeperConfig{stage: "inference", progress: 50, message: "模型推理进行中"})
}

// isCommittedResult reports whether the job already owns its terminal success.
func isCommittedResult(err error) bool {
	return errors.Is(err, remote.ErrResultCommitted)
}
