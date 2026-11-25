package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

func queryImageDigest(t *testing.T, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func queryImageJob(t *testing.T, jobID string, body []byte) remote.WorkerJob {
	request := domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion,
		OperationID:   "op-img-1",
		Operation:     domain.OpVisualEmbedding,
		ModelID:       "google/siglip",
		ModelRevision: "rev-1",
		InputHash:     "i",
		ConfigHash:    "cfg-1",
		SpaceID:       "space-1",
		TimeoutMS:     1000,
		Input: domain.InferenceInput{
			Kind:        domain.InputKindImage,
			ReferenceID: "ref-1",
			Digest:      queryImageDigest(t, body),
		},
		Visual: &domain.VisualSettings{},
	}
	return remote.WorkerJob{JobID: jobID, AssetID: "asset-1", JobType: domain.JobInference, Inference: &request}
}

func visualResult() domain.InferenceResult {
	return domain.InferenceResult{
		SchemaVersion: domain.InferenceSchemaVersion,
		OperationID:   "op-img-1",
		Operation:     domain.OpVisualEmbedding,
		ModelID:       "google/siglip",
		ModelRevision: "rev-1",
		InputHash:     "i",
		ConfigHash:    "cfg-1",
		Backend:       "cpu",
		Embeddings: []domain.EmbeddingRecord{
			{SampleIndex: 0, Vector: []float32{1, 2, 3}},
		},
	}
}

// imageJobClient serves one image lease and records everything the runtime
// does with it.
type imageJobClient struct {
	mu          sync.Mutex
	lease       *remote.WorkerJob
	body        []byte
	fetches     int
	completed   []domain.JobState
	failMessage string
	submissions []remote.InferenceResultSubmission
}

func (c *imageJobClient) Heartbeat(context.Context, string, string, remote.WorkerCapabilities) error {
	return nil
}

func (c *imageJobClient) Lease(context.Context, string) (*remote.WorkerJob, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	job := c.lease
	c.lease = nil
	return job, nil
}

func (c *imageJobClient) UploadArtifact(context.Context, string, string, ArtifactUpload) error {
	return nil
}

func (c *imageJobClient) Complete(_ context.Context, _, _ string, state domain.JobState, message string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.completed = append(c.completed, state)
	if state == domain.JobFailed {
		c.failMessage = message
	}
	return nil
}

func (c *imageJobClient) ProgressState(context.Context, string, string, string, float64, string, string) (remote.InferenceProgress, error) {
	return remote.InferenceProgress{LeaseValid: true}, nil
}

func (c *imageJobClient) SubmitInferenceResult(_ context.Context, _, _ string, submission remote.InferenceResultSubmission) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.submissions = append(c.submissions, submission)
	return nil
}

func (c *imageJobClient) FetchInferenceInput(context.Context, string, string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetches++
	return c.body, nil
}

// resolvingExecutor resolves the image through the context it receives, the
// only path the real localinference executor uses after the CLI wires
// ResolveInferenceImage into its config.
type resolvingExecutor struct {
	mu         sync.Mutex
	resolved   []byte
	outsideErr string
	resolveErr string
}

func (e *resolvingExecutor) Capabilities(context.Context) ([]remote.InferenceCapability, error) {
	return nil, nil
}

func (e *resolvingExecutor) Execute(ctx context.Context, req domain.InferenceRequest) (domain.InferenceResult, error) {
	path, err := ResolveInferenceImage(ctx, req.Input.ReferenceID, req.Input.Digest)
	if err != nil {
		e.mu.Lock()
		e.resolveErr = err.Error()
		e.mu.Unlock()
		return domain.InferenceResult{}, err
	}
	// Outside the job context the same lookup must fail: the staged bytes are
	// scoped to the derived execution context, not the process.
	if _, outsideErr := ResolveInferenceImage(context.Background(), req.Input.ReferenceID, req.Input.Digest); outsideErr == nil {
		e.mu.Lock()
		e.outsideErr = "resolved outside the job context"
		e.mu.Unlock()
		return domain.InferenceResult{}, outsideErr
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.InferenceResult{}, err
	}
	e.mu.Lock()
	e.resolved = data
	e.mu.Unlock()
	return visualResult(), nil
}

// A query-image job stages the lease-bound bytes, publishes them to the
// executor only through the derived job context, and removes them with the
// job. The executor sees the exact fetched bytes and cannot resolve the same
// reference from an unrelated context.
func TestQueryImageStagesThroughJobContext(t *testing.T) {
	dir := t.TempDir()
	body := []byte("query-image-bytes")
	job := queryImageJob(t, "job-img-1", body)
	client := &imageJobClient{lease: &job, body: body}
	executor := &resolvingExecutor{}
	runtime := NewRuntime(client, Config{Token: "token", CacheDir: filepath.Join(dir, "cache")}, nil, remote.WorkerCapabilities{}).
		WithInferenceExecutor(executor)

	worked, err := runtime.RunOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("RunOnce worked=%t err=%v", worked, err)
	}
	if executor.resolveErr != "" {
		t.Fatalf("in-job resolution failed: %s", executor.resolveErr)
	}
	if executor.outsideErr != "" {
		t.Fatalf("executor report: %s", executor.outsideErr)
	}
	if string(executor.resolved) != string(body) {
		t.Fatalf("executor saw %q, want the fetched bytes %q", executor.resolved, body)
	}
	if client.fetches != 1 {
		t.Fatalf("lease-input fetches = %d, want 1", client.fetches)
	}
	if len(client.submissions) != 1 || client.submissions[0].OperationID != "op-img-1" {
		t.Fatalf("submissions = %+v, want one committed result", client.submissions)
	}
	inputs := filepath.Join(dir, "cache", "inference", "inputs")
	if entries, err := os.ReadDir(inputs); err != nil || len(entries) != 0 {
		t.Fatalf("staged query input survived the job: entries=%v err=%v", entries, err)
	}
	if len(client.completed) != 0 {
		t.Fatalf("ordinary complete must not run for a committed inference job, got %+v", client.completed)
	}
}

// A mismatching fetch can never reach the executor: the digest is verified
// before anything is staged or executed, and the job fails without a result.
func TestQueryImageRejectsDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	job := queryImageJob(t, "job-img-2", []byte("expected-bytes"))
	client := &imageJobClient{lease: &job, body: []byte("different-bytes")}
	executor := &resolvingExecutor{}
	runtime := NewRuntime(client, Config{Token: "token", CacheDir: filepath.Join(dir, "cache")}, nil, remote.WorkerCapabilities{}).
		WithInferenceExecutor(executor)

	worked, err := runtime.RunOnce(context.Background())
	if !worked || err == nil {
		t.Fatalf("RunOnce worked=%t err=%v, want a reported failure", worked, err)
	}
	if !strings.Contains(strings.ToLower(client.failMessage), "digest") {
		t.Fatalf("failure record %q must name the digest mismatch", client.failMessage)
	}
	if len(client.submissions) != 0 {
		t.Fatalf("submissions = %+v, want none", client.submissions)
	}
	inputs := filepath.Join(dir, "cache", "inference", "inputs")
	if _, statErr := os.Stat(inputs); !os.IsNotExist(statErr) {
		t.Fatalf("nothing may be staged for a rejected fetch: %v", statErr)
	}
}

// A client without the lease-input fetch capability rejects image inputs as
// unsupported_input before executing anything, exactly like a missing local
// path resolver would.
func TestQueryImageWithoutFetcherIsUnsupportedInput(t *testing.T) {
	dir := t.TempDir()
	job := queryImageJob(t, "job-img-3", []byte("query-image-bytes"))
	plain := &fakeInferenceClient{lease: &job}
	executor := &resolvingExecutor{}
	runtime := NewRuntime(plain, Config{Token: "token", CacheDir: filepath.Join(dir, "cache")}, nil, remote.WorkerCapabilities{}).
		WithInferenceExecutor(executor)

	_, err := runtime.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce must fail for an image job without a lease-input fetcher")
	}
	var failure *remote.InferenceFailure
	if !errors.As(err, &failure) || failure.Code != remote.FailureUnsupportedInput {
		t.Fatalf("error %v must be a typed unsupported_input failure", err)
	}
	if plain.submissions != nil {
		t.Fatalf("submissions = %+v, want none", plain.submissions)
	}
}

// The package-level resolver refuses every lookup that did not come from the
// executing job's derived context: unknown job, wrong reference, wrong digest.
func TestResolveInferenceImageScopedToJobContext(t *testing.T) {
	dir := t.TempDir()
	if _, err := ResolveInferenceImage(context.Background(), "ref-1", "sha256:00"); err == nil {
		t.Fatal("a context without staged images must not resolve")
	}
	staged := &stagedQueryImages{
		dir: dir,
		entries: map[string]stagedQueryImage{
			"ref-1": {digest: "sha256:11", path: filepath.Join(dir, "query-image")},
		},
	}
	inJob := withStagedQueryImages(context.Background(), staged)
	if path, err := ResolveInferenceImage(inJob, "ref-1", "sha256:11"); err != nil || path != filepath.Join(dir, "query-image") {
		t.Fatalf("in-job lookup path=%q err=%v", path, err)
	}
	if _, err := ResolveInferenceImage(inJob, "ref-other", "sha256:11"); err == nil {
		t.Fatal("an unknown reference must not resolve")
	}
	if _, err := ResolveInferenceImage(inJob, "ref-1", "sha256:22"); err == nil {
		t.Fatal("a digest that does not match the staged bytes must not resolve")
	}
	if _, err := ResolveInferenceImage(inJob, "ref-1", ""); err == nil {
		t.Fatal("a missing digest must not resolve")
	}
	staged.release()
	if _, err := ResolveInferenceImage(inJob, "ref-1", "sha256:11"); err == nil {
		t.Fatal("after release the job context must not resolve")
	}
}

// Job IDs become a single scratch path component: nothing outside [A-Za-z0-9_-]
// survives, and the result can never escape the inference inputs root.
func TestSanitizePathSegment(t *testing.T) {
	cases := map[string]string{
		"job-1":    "job-1",
		"a/b\\c":   "a_b_c",
		"..\\..":   "_____",
		"":         "unknown-job",
		"sp ace":   "sp_ace",
		"üñî":      "___",
		"Job_9-ok": "Job_9-ok",
	}
	for in, want := range cases {
		if got := sanitizePathSegment(in); got != want {
			t.Errorf("sanitizePathSegment(%q) = %q, want %q", in, got, want)
		}
	}
}
