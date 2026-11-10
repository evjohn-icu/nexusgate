package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

type fakeInferenceClient struct {
	mu            sync.Mutex
	heartbeats    int
	lease         *remote.WorkerJob
	completed     []domain.JobState
	submissions   []remote.InferenceResultSubmission
	progressCalls int
	progressState remote.InferenceProgress
	progressErr   error
	submitErr     error
}

func (f *fakeInferenceClient) Heartbeat(context.Context, string, string, remote.WorkerCapabilities) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats++
	return nil
}

func (f *fakeInferenceClient) Lease(context.Context, string) (*remote.WorkerJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job := f.lease
	f.lease = nil
	return job, nil
}

func (f *fakeInferenceClient) UploadArtifact(context.Context, string, string, ArtifactUpload) error {
	return nil
}

func (f *fakeInferenceClient) Complete(_ context.Context, _, _ string, state domain.JobState, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, state)
	return nil
}

func (f *fakeInferenceClient) ProgressState(context.Context, string, string, string, float64, string, string) (remote.InferenceProgress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.progressCalls++
	return f.progressState, f.progressErr
}

func (f *fakeInferenceClient) SubmitInferenceResult(_ context.Context, _, _ string, submission remote.InferenceResultSubmission) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submissions = append(f.submissions, submission)
	return f.submitErr
}

type fakeExecutor struct {
	result domain.InferenceResult
	err    error
	run    func(ctx context.Context, req domain.InferenceRequest) (domain.InferenceResult, error)
}

func (f *fakeExecutor) Capabilities(context.Context) ([]remote.InferenceCapability, error) {
	return []remote.InferenceCapability{{
		Operation:     domain.OpSoundEvents,
		SchemaVersion: domain.InferenceSchemaVersion,
		ModelID:       "google/yamnet",
		ModelRevision: "rev-1",
		ConfigHash:    "cfg-1",
		Backend:       "cpu",
		Available:     true,
	}}, nil
}

func (f *fakeExecutor) Execute(ctx context.Context, req domain.InferenceRequest) (domain.InferenceResult, error) {
	if f.run != nil {
		return f.run(ctx, req)
	}
	return f.result, f.err
}

func inferenceJob() remote.WorkerJob {
	request := domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion,
		OperationID:   "op-1",
		Operation:     domain.OpSoundEvents,
		ModelID:       "google/yamnet",
		ModelRevision: "rev-1",
		InputHash:     "i",
		ConfigHash:    "cfg-1",
		TimeoutMS:     1000,
		Input:         domain.InferenceInput{Kind: domain.InputKindAsset, AssetID: "a", DurationMS: 40000},
		SoundEvent:    &domain.SoundEventSettings{},
	}
	return remote.WorkerJob{JobID: "job-1", AssetID: "a", JobType: domain.JobInference, Inference: &request}
}

func validSoundResult() domain.InferenceResult {
	return domain.InferenceResult{
		SchemaVersion: domain.InferenceSchemaVersion,
		OperationID:   "op-1",
		Operation:     domain.OpSoundEvents,
		ModelID:       "google/yamnet",
		ModelRevision: "rev-1",
		InputHash:     "i",
		ConfigHash:    "cfg-1",
		Backend:       "cpu",
		Events: []domain.SoundEvent{
			{Label: "Rain", StartMS: 1000, EndMS: 2000, RawScore: 0.7, EvidenceStatus: "possible"},
		},
	}
}

func TestProcessInferenceSubmitsResultAndSkipsOrdinaryComplete(t *testing.T) {
	client := &fakeInferenceClient{progressState: remote.InferenceProgress{LeaseValid: true}}
	executor := &fakeExecutor{result: validSoundResult()}
	runtime := NewRuntime(client, Config{Token: "t"}, nil, remote.WorkerCapabilities{}).WithInferenceExecutor(executor)
	job := inferenceJob()
	if err := runtime.processAndComplete(context.Background(), job); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(client.submissions) != 1 {
		t.Fatalf("want one submission, got %d", len(client.submissions))
	}
	if client.submissions[0].Digest == "" {
		t.Fatal("submission must carry a digest")
	}
	if len(client.completed) != 0 {
		t.Fatalf("the ordinary complete endpoint must not be called: %+v", client.completed)
	}
}

func TestProcessInferenceFailureUsesOrdinaryFailureComplete(t *testing.T) {
	client := &fakeInferenceClient{progressState: remote.InferenceProgress{LeaseValid: true}}
	executor := &fakeExecutor{err: errors.New("model exploded")}
	runtime := NewRuntime(client, Config{Token: "t"}, nil, remote.WorkerCapabilities{}).WithInferenceExecutor(executor)
	if err := runtime.processAndComplete(context.Background(), inferenceJob()); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(client.completed) != 1 || client.completed[0] != domain.JobFailed {
		t.Fatalf("want one failed completion, got %+v", client.completed)
	}
	if len(client.submissions) != 0 {
		t.Fatal("a failed execution must not submit a result")
	}
}

func TestProcessInferenceCancelsOnHubRequest(t *testing.T) {
	client := &fakeInferenceClient{progressState: remote.InferenceProgress{LeaseValid: true, CancelRequested: true}}
	cancelled := make(chan struct{})
	executor := &fakeExecutor{run: func(ctx context.Context, _ domain.InferenceRequest) (domain.InferenceResult, error) {
		select {
		case <-ctx.Done():
			close(cancelled)
			return domain.InferenceResult{}, ctx.Err()
		case <-time.After(5 * time.Second):
			return validSoundResult(), nil
		}
	}}
	runtime := NewRuntime(client, Config{Token: "t"}, nil, remote.WorkerCapabilities{}).WithInferenceExecutor(executor).WithInferenceRenewInterval(10 * time.Millisecond)
	err := runtime.processAndComplete(context.Background(), inferenceJob())
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("the executor context was not cancelled after the Hub asked for cancellation")
	}
	if len(client.submissions) != 0 {
		t.Fatal("a cancelled execution must not submit a result")
	}
}

func TestProcessInferenceCancelsOnLostLease(t *testing.T) {
	client := &fakeInferenceClient{progressState: remote.InferenceProgress{LeaseValid: false}}
	cancelSeen := make(chan struct{})
	executor := &fakeExecutor{run: func(ctx context.Context, _ domain.InferenceRequest) (domain.InferenceResult, error) {
		<-ctx.Done()
		close(cancelSeen)
		return domain.InferenceResult{}, ctx.Err()
	}}
	runtime := NewRuntime(client, Config{Token: "t"}, nil, remote.WorkerCapabilities{}).WithInferenceExecutor(executor).WithInferenceRenewInterval(10 * time.Millisecond)
	if err := runtime.processAndComplete(context.Background(), inferenceJob()); err != nil {
		t.Fatalf("process: %v", err)
	}
	select {
	case <-cancelSeen:
	default:
		t.Fatal("losing the lease must cancel the operation")
	}
}

func TestProcessRejectsAnInferenceJobWithoutRequest(t *testing.T) {
	runtime := NewRuntime(&fakeInferenceClient{}, Config{Token: "t"}, nil, remote.WorkerCapabilities{}).WithInferenceExecutor(&fakeExecutor{})
	job := remote.WorkerJob{JobID: "job-1", JobType: domain.JobInference}
	if err := runtime.process(context.Background(), job); err == nil {
		t.Fatal("an inference lease without a request must be rejected")
	}
}

func TestRunOnceAllowsInferenceExecutorWithoutDeriver(t *testing.T) {
	client := &fakeInferenceClient{
		progressState: remote.InferenceProgress{LeaseValid: true},
		lease:         func() *remote.WorkerJob { job := inferenceJob(); return &job }(),
	}
	runtime := NewRuntime(client, Config{Token: "t"}, nil, remote.WorkerCapabilities{}).WithInferenceExecutor(&fakeExecutor{result: validSoundResult()})
	worked, err := runtime.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if !worked {
		t.Fatal("the job was not processed")
	}
	if client.heartbeats != 1 {
		t.Fatalf("want one heartbeat, got %d", client.heartbeats)
	}
}
