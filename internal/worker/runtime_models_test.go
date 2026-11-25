package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

type fakeModelManager struct {
	mu         sync.Mutex
	installed  []domain.ModelManagementTask
	probed     []domain.ModelManagementTask
	installErr error
	probeErr   error
	// observe, when set, runs inside Install/Probe with the execution context
	// so tests can assert cancellation reached the work.
	observe func(ctx context.Context)
}

func (m *fakeModelManager) Install(ctx context.Context, task domain.ModelManagementTask) error {
	m.mu.Lock()
	m.installed = append(m.installed, task)
	m.mu.Unlock()
	if m.observe != nil {
		m.observe(ctx)
	}
	// Aborted work is a failure, not a success: a cancelled context must not
	// look like a completed install.
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.installErr
}

func (m *fakeModelManager) Probe(ctx context.Context, task domain.ModelManagementTask) error {
	m.mu.Lock()
	m.probed = append(m.probed, task)
	m.mu.Unlock()
	if m.observe != nil {
		m.observe(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.probeErr
}

func modelManagementTask(action string, licenseAccepted bool) *remote.ModelManagementTask {
	model, ok := inferencecatalog.Lookup("sound-yamnet")
	if !ok {
		panic("sound model missing from catalog")
	}
	return &remote.ModelManagementTask{
		JobID:           "job-1",
		TargetWorkerID:  "worker-1",
		CatalogID:       model.CatalogID,
		ManifestDigest:  model.ManifestDigest(),
		Action:          action,
		LicenseAccepted: licenseAccepted,
	}
}

func maintenanceJob(task *remote.ModelManagementTask) remote.WorkerJob {
	jobType := domain.JobModelInstall
	if task.Action == "probe" {
		jobType = domain.JobModelProbe
	}
	return remote.WorkerJob{JobID: task.JobID, JobType: jobType, ModelManagement: task}
}

func TestRunModelManagementInstallsThroughOrdinaryComplete(t *testing.T) {
	client := &fakeInferenceClient{progressState: remote.InferenceProgress{LeaseValid: true}}
	manager := &fakeModelManager{}
	runtime := NewRuntime(client, Config{Token: "t"}, nil, remote.WorkerCapabilities{}).WithModelManager(manager)
	job := maintenanceJob(modelManagementTask("install", true))
	if err := runtime.processAndComplete(context.Background(), job); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(manager.installed) != 1 {
		t.Fatalf("install calls=%+v", manager.installed)
	}
	if task := manager.installed[0]; task.CatalogID != "sound-yamnet" || !task.LicenseAccepted || task.JobID != job.JobID {
		t.Fatalf("install task=%+v", task)
	}
	if len(client.completed) != 1 || client.completed[0] != domain.JobSucceeded {
		t.Fatalf("completions=%+v, want one ordinary succeeded complete", client.completed)
	}
	if len(client.submissions) != 0 {
		t.Fatal("maintenance must not submit an inference result")
	}
}

func TestRunModelManagementDispatchesProbe(t *testing.T) {
	client := &fakeInferenceClient{progressState: remote.InferenceProgress{LeaseValid: true}}
	manager := &fakeModelManager{}
	runtime := NewRuntime(client, Config{Token: "t"}, nil, remote.WorkerCapabilities{}).WithModelManager(manager)
	job := maintenanceJob(modelManagementTask("probe", false))
	if err := runtime.processAndComplete(context.Background(), job); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(manager.probed) != 1 || len(manager.installed) != 0 {
		t.Fatalf("probe calls=%+v install calls=%+v", manager.probed, manager.installed)
	}
	if len(client.completed) != 1 || client.completed[0] != domain.JobSucceeded {
		t.Fatalf("completions=%+v", client.completed)
	}
}

func TestRunModelManagementValidatesLeaseTask(t *testing.T) {
	cases := []struct {
		name string
		job  remote.WorkerJob
		want string
	}{
		{"missing task", remote.WorkerJob{JobID: "job-1", JobType: domain.JobModelInstall}, "carries no task"},
		{"task and lease id disagree", func() remote.WorkerJob {
			task := modelManagementTask("install", true)
			task.JobID = "other-job"
			job := maintenanceJob(task)
			job.JobID = "job-1"
			return job
		}(), "does not match lease"},
		{"unknown action", func() remote.WorkerJob {
			task := modelManagementTask("install", true)
			task.Action = "purge"
			return maintenanceJob(task)
		}(), "unknown model management action"},
		{"type and action disagree", func() remote.WorkerJob {
			task := modelManagementTask("install", true)
			job := maintenanceJob(task)
			job.JobType = domain.JobModelProbe
			return job
		}(), "does not match action"},
		{"missing license acceptance", func() remote.WorkerJob {
			return maintenanceJob(modelManagementTask("install", false))
		}(), "explicit licence acceptance"},
		{"missing catalog identity", func() remote.WorkerJob {
			task := modelManagementTask("install", true)
			task.CatalogID = ""
			return maintenanceJob(task)
		}(), "catalog identity"},
		{"missing manifest digest", func() remote.WorkerJob {
			task := modelManagementTask("install", true)
			task.ManifestDigest = ""
			return maintenanceJob(task)
		}(), "manifest digest"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeInferenceClient{progressState: remote.InferenceProgress{LeaseValid: true}}
			manager := &fakeModelManager{}
			runtime := NewRuntime(client, Config{Token: "t"}, nil, remote.WorkerCapabilities{}).WithModelManager(manager)
			err := runtime.process(context.Background(), test.job)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
			if len(manager.installed)+len(manager.probed) != 0 {
				t.Fatal("a rejected lease must not reach the manager")
			}
			if len(client.completed) != 0 {
				t.Fatal("process must not complete; the caller reports the failure")
			}
		})
	}
}

func TestRunModelManagementWithoutManagerFails(t *testing.T) {
	client := &fakeInferenceClient{progressState: remote.InferenceProgress{LeaseValid: true}}
	runtime := NewRuntime(client, Config{Token: "t"}, nil, remote.WorkerCapabilities{})
	job := maintenanceJob(modelManagementTask("install", true))
	err := runtime.process(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "no model manager") {
		t.Fatalf("error=%v, want a missing-manager failure", err)
	}
	// The ordinary failure completion records it; nothing succeeds.
	if err := runtime.processAndComplete(context.Background(), job); err != nil {
		t.Fatalf("processAndComplete: %v", err)
	}
	if len(client.completed) != 1 || client.completed[0] != domain.JobFailed {
		t.Fatalf("completions=%+v, want the reported failure", client.completed)
	}
}

func TestRunModelManagementCancelsOnLostLease(t *testing.T) {
	client := &fakeInferenceClient{progressState: remote.InferenceProgress{LeaseValid: false}}
	cancelSeen := make(chan struct{})
	var once sync.Once
	manager := &fakeModelManager{observe: func(ctx context.Context) {
		<-ctx.Done()
		once.Do(func() { close(cancelSeen) })
	}}
	runtime := NewRuntime(client, Config{Token: "t"}, nil, remote.WorkerCapabilities{}).WithModelManager(manager).WithInferenceRenewInterval(10 * time.Millisecond)
	if err := runtime.processAndComplete(context.Background(), maintenanceJob(modelManagementTask("install", true))); err != nil {
		t.Fatalf("process: %v", err)
	}
	select {
	case <-cancelSeen:
	default:
		t.Fatal("the manager context was not cancelled after the Hub rejected the renewal")
	}
	if len(client.completed) != 1 || client.completed[0] != domain.JobFailed {
		t.Fatalf("completions=%+v, want one failed completion", client.completed)
	}
	if len(manager.installed) != 1 {
		t.Fatalf("install calls=%+v", manager.installed)
	}
}

func TestRunOnceExecutesMaintenanceJob(t *testing.T) {
	client := &fakeInferenceClient{lease: func() *remote.WorkerJob { job := maintenanceJob(modelManagementTask("install", true)); return &job }(), progressState: remote.InferenceProgress{LeaseValid: true}}
	manager := &fakeModelManager{}
	runtime := NewRuntime(client, Config{Token: "t"}, nil, remote.WorkerCapabilities{}).WithModelManager(manager)
	ran, err := runtime.RunOnce(context.Background())
	if err != nil || !ran {
		t.Fatalf("RunOnce ran=%v err=%v", ran, err)
	}
	if len(manager.installed) != 1 || manager.installed[0].Action != "install" {
		t.Fatalf("install calls=%+v", manager.installed)
	}
	if len(client.completed) != 1 || client.completed[0] != domain.JobSucceeded {
		t.Fatalf("completions=%+v, want one succeeded completion", client.completed)
	}
}

func TestRunModelManagementReportsManagerFailure(t *testing.T) {
	client := &fakeInferenceClient{progressState: remote.InferenceProgress{LeaseValid: true}}
	manager := &fakeModelManager{installErr: errors.New("artifact digest mismatch")}
	runtime := NewRuntime(client, Config{Token: "t"}, nil, remote.WorkerCapabilities{}).WithModelManager(manager)
	if err := runtime.processAndComplete(context.Background(), maintenanceJob(modelManagementTask("install", true))); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(client.completed) != 1 || client.completed[0] != domain.JobFailed {
		t.Fatalf("completions=%+v, want one failed completion", client.completed)
	}
}

func TestRunModelManagementRenewsLeaseWhileWorking(t *testing.T) {
	client := &fakeInferenceClient{progressState: remote.InferenceProgress{LeaseValid: true}}
	working := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	manager := &fakeModelManager{observe: func(ctx context.Context) {
		once.Do(func() { close(working) })
		<-release
	}}
	runtime := NewRuntime(client, Config{Token: "t"}, nil, remote.WorkerCapabilities{}).WithModelManager(manager).WithInferenceRenewInterval(10 * time.Millisecond)
	done := make(chan error, 1)
	go func() {
		done <- runtime.processAndComplete(context.Background(), maintenanceJob(modelManagementTask("install", true)))
	}()
	select {
	case <-working:
	case <-time.After(5 * time.Second):
		t.Fatal("the manager never started working")
	}
	// The renewal loop runs on its own cadence; poll rather than race the
	// first tick.
	deadline := time.Now().Add(5 * time.Second)
	for {
		client.mu.Lock()
		calls := client.progressCalls
		client.mu.Unlock()
		if calls > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no progress renewal was recorded while maintenance was working")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("process: %v", err)
	}
}
