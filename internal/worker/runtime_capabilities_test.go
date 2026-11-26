package worker

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

// capsClient records the capabilities of every heartbeat so the tests can
// prove that the executor's live inventory — not the enroll-time snapshot —
// is what reaches the Hub.
type capsClient struct {
	mu         sync.Mutex
	heartbeats []remote.WorkerCapabilities
	lease      *remote.WorkerJob
	completed  []domain.JobState
}

func (c *capsClient) Heartbeat(_ context.Context, _, _ string, capabilities remote.WorkerCapabilities) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heartbeats = append(c.heartbeats, capabilities)
	return nil
}

func (c *capsClient) Lease(context.Context, string) (*remote.WorkerJob, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	job := c.lease
	c.lease = nil
	return job, nil
}

func (c *capsClient) UploadArtifact(context.Context, string, string, ArtifactUpload) error {
	return nil
}

func (c *capsClient) Complete(_ context.Context, _, _ string, state domain.JobState, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.completed = append(c.completed, state)
	return nil
}

// inventoryExecutor answers Capabilities with successive inventories so the
// tests can tell which call a given heartbeat picked up.
type inventoryExecutor struct {
	mu          sync.Mutex
	inventories [][]remote.InferenceCapability
	err         error
	calls       int
}

func (e *inventoryExecutor) Capabilities(context.Context) ([]remote.InferenceCapability, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	index := e.calls
	e.calls++
	if e.err != nil {
		return nil, e.err
	}
	if index >= len(e.inventories) {
		index = len(e.inventories) - 1
	}
	if index < 0 {
		return nil, nil
	}
	return e.inventories[index], nil
}

func (e *inventoryExecutor) Execute(context.Context, domain.InferenceRequest) (domain.InferenceResult, error) {
	return domain.InferenceResult{}, errors.New("inventory executor does not execute")
}

func soundEventsCapability(available bool) []remote.InferenceCapability {
	return []remote.InferenceCapability{{
		Operation:     domain.OpSoundEvents,
		SchemaVersion: domain.InferenceSchemaVersion,
		ModelID:       "google/yamnet",
		ModelRevision: "rev-1",
		ConfigHash:    "cfg-1",
		Backend:       "cpu",
		Available:     available,
	}}
}

func declaredCapabilities() remote.WorkerCapabilities {
	return remote.WorkerCapabilities{
		Proxy:                     true,
		LibraryRoots:              []string{"root-1"},
		ProviderOperations:        []string{"video_analysis"},
		InferenceOperations:       []string{"sound_events"},
		ModelManagementOperations: []string{"install", "probe"},
	}
}

// A heartbeat must advertise what the executor can serve right now. Before
// the first heartbeat and before every normal heartbeat the runtime refreshes
// the executor's inventory into the outgoing copy; the declared fields an
// executor knows nothing about pass through unchanged.
func TestHeartbeatPublishesFreshExecutorInventory(t *testing.T) {
	declared := declaredCapabilities()
	declared.Inference = soundEventsCapability(false) // stale enroll-time entry

	executor := &inventoryExecutor{inventories: [][]remote.InferenceCapability{
		soundEventsCapability(true),
		soundEventsCapability(false),
	}}
	client := &capsClient{lease: nil}
	runtime := NewRuntime(client, Config{Token: "token"}, nil, declared).
		WithInferenceExecutor(executor)

	if _, err := runtime.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(client.heartbeats) != 1 {
		t.Fatalf("heartbeats = %d, want 1", len(client.heartbeats))
	}
	sent := client.heartbeats[0]
	if !reflect.DeepEqual(sent.Inference, soundEventsCapability(true)) {
		t.Fatalf("first heartbeat must carry the fresh inventory, got %+v", sent.Inference)
	}
	if !reflect.DeepEqual(sent.InferenceOperations, declared.InferenceOperations) ||
		!reflect.DeepEqual(sent.ModelManagementOperations, declared.ModelManagementOperations) ||
		!reflect.DeepEqual(sent.ProviderOperations, declared.ProviderOperations) {
		t.Fatalf("declared grant fields must pass through unchanged, got %+v", sent)
	}
	// The declared struct itself must not be mutated by the refresh.
	if !reflect.DeepEqual(declared.Inference, soundEventsCapability(false)) {
		t.Fatalf("heartbeat refresh must work on a copy, declared = %+v", declared.Inference)
	}
}

// An executor or probe error must publish no available models rather than the
// previous list: a stale heartbeat would keep the Hub leasing jobs a Worker
// cannot execute.
func TestHeartbeatPublishesNoModelsOnExecutorError(t *testing.T) {
	declared := declaredCapabilities()
	executor := &inventoryExecutor{err: errors.New("probe failed")}
	client := &capsClient{}
	runtime := NewRuntime(client, Config{Token: "token"}, nil, declared).
		WithInferenceExecutor(executor)

	if _, err := runtime.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(client.heartbeats) != 1 {
		t.Fatalf("heartbeats = %d, want 1", len(client.heartbeats))
	}
	if sent := client.heartbeats[0]; len(sent.Inference) != 0 {
		t.Fatalf("a failed probe must publish no models, got %+v", sent.Inference)
	}
	if sent := client.heartbeats[0]; !reflect.DeepEqual(sent.InferenceOperations, declared.InferenceOperations) {
		t.Fatalf("the enrollment grant set must survive a probe failure, got %+v", sent.InferenceOperations)
	}
}

// The long-running loop refreshes on the first heartbeat and on every normal
// heartbeat, so a model installed or probed between beats is advertised at
// the next beat without waiting for a restart.
func TestRunRefreshesInventoryEveryHeartbeat(t *testing.T) {
	declared := declaredCapabilities()
	empty := []remote.InferenceCapability{}
	executor := &inventoryExecutor{inventories: [][]remote.InferenceCapability{
		empty,
		soundEventsCapability(true),
		soundEventsCapability(true),
	}}
	client := &capsClient{}
	runtime := NewRuntime(client, Config{Token: "token"}, nil, declared).
		WithInferenceExecutor(executor)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if err := runtime.Run(ctx, RunOptions{HeartbeatInterval: 20 * time.Millisecond, PollInterval: 5 * time.Millisecond}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.heartbeats) < 2 {
		t.Fatalf("heartbeats = %d, want at least the first and one normal beat", len(client.heartbeats))
	}
	if got := client.heartbeats[0].Inference; len(got) != 0 {
		t.Fatalf("first heartbeat must see the empty inventory, got %+v", got)
	}
	for _, beat := range client.heartbeats[1:] {
		if !reflect.DeepEqual(beat.Inference, soundEventsCapability(true)) {
			t.Fatalf("later heartbeats must see the installed model, got %+v", beat.Inference)
		}
	}
}

// A derive-only Worker keeps sending its declared capabilities unchanged: a
// heartbeat without an executor publishes no inventory field of its own.
func TestDeriveOnlyHeartbeatKeepsDeclaredCapabilities(t *testing.T) {
	declared := declaredCapabilities()
	client := &capsClient{}
	runtime := NewRuntime(client, Config{Token: "token"}, &FFmpegDeriver{}, declared)

	if _, err := runtime.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(client.heartbeats) != 1 {
		t.Fatalf("heartbeats = %d, want 1", len(client.heartbeats))
	}
	if sent := client.heartbeats[0]; sent.Inference != nil {
		t.Fatalf("a derive-only heartbeat must not invent an inventory, got %+v", sent.Inference)
	}
}
