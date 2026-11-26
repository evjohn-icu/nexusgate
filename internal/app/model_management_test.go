package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
	"github.com/evjohn-icu/nexusgate/internal/remote"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

// queueModelManagementService builds a real SQLite-backed service whose
// pipeline carries a manager that records every action it is asked to run.
type recordingManager struct {
	installed []domain.ModelManagementTask
	probed    []domain.ModelManagementTask
}

func (m *recordingManager) Install(ctx context.Context, task domain.ModelManagementTask) error {
	m.installed = append(m.installed, task)
	return nil
}

func (m *recordingManager) Probe(ctx context.Context, task domain.ModelManagementTask) error {
	m.probed = append(m.probed, task)
	return nil
}

func newModelManagementService(t *testing.T) (*Service, *sqlite.Repository) {
	t.Helper()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "model-management.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, config.Config{DataDir: dir, CacheDir: filepath.Join(dir, "cache")})
	if err != nil {
		t.Fatal(err)
	}
	return service, repo
}

func TestQueueModelManagementValidatesWorkerTargets(t *testing.T) {
	service, repo := newModelManagementService(t)
	ctx := context.Background()
	model, ok := inferencecatalog.Lookup("sound-yamnet")
	if !ok {
		t.Fatal("sound model missing from catalog")
	}

	t.Run("unknown worker is rejected at queue time", func(t *testing.T) {
		_, err := service.QueueModelManagement(ctx, "install", model.CatalogID, "worker-ghost", model.ManifestDigest(), true)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("unknown target error=%v", err)
		}
	})

	t.Run("action-ineligible worker is rejected", func(t *testing.T) {
		capabilities, err := json.Marshal(remote.WorkerCapabilities{ModelManagementOperations: []string{"probe"}})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := repo.DB().ExecContext(ctx, `INSERT INTO workers(id,name,platform,token_hash,status,capabilities_json,last_seen_at,created_at) VALUES('worker-probe','probe','linux','tok','online',?,?,?)`, string(capabilities), now, now); err != nil {
			t.Fatal(err)
		}
		_, err = service.QueueModelManagement(ctx, "install", model.CatalogID, "worker-probe", model.ManifestDigest(), true)
		if err == nil || !strings.Contains(err.Error(), "not authorized") {
			t.Fatalf("probe-only target install error=%v", err)
		}
		if got := modelTaskCount(t, repo); got != 0 {
			t.Fatalf("rejected target queued %d tasks", got)
		}
		job, err := service.QueueModelManagement(ctx, "probe", model.CatalogID, "worker-probe", model.ManifestDigest(), false)
		if err != nil {
			t.Fatalf("probe target probe error=%v", err)
		}
		task, err := repo.GetModelManagementTask(ctx, job)
		if err != nil || task.TargetWorkerID != "worker-probe" || task.Action != "probe" {
			t.Fatalf("probe task=%+v err=%v", task, err)
		}
	})

	t.Run("revoked worker is rejected", func(t *testing.T) {
		capabilities, err := json.Marshal(remote.WorkerCapabilities{ModelManagementOperations: []string{"install"}})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := repo.DB().ExecContext(ctx, `INSERT INTO workers(id,name,platform,token_hash,status,capabilities_json,last_seen_at,created_at) VALUES('worker-revoked','gone','linux','tok2','revoked',?,?,?)`, string(capabilities), now, now); err != nil {
			t.Fatal(err)
		}
		_, err = service.QueueModelManagement(ctx, "install", model.CatalogID, "worker-revoked", model.ManifestDigest(), true)
		if err == nil || !strings.Contains(err.Error(), "revoked") {
			t.Fatalf("revoked target error=%v", err)
		}
	})

	t.Run("empty and explicit local targets queue the stable local target", func(t *testing.T) {
		job, err := service.QueueModelManagement(ctx, "install", model.CatalogID, "", model.ManifestDigest(), true)
		if err != nil {
			t.Fatal(err)
		}
		task, err := repo.GetModelManagementTask(ctx, job)
		if err != nil || task.TargetWorkerID != localModelTarget {
			t.Fatalf("local task target=%q err=%v", task.TargetWorkerID, err)
		}
		job, err = service.QueueModelManagement(ctx, "probe", model.CatalogID, localModelTarget, model.ManifestDigest(), false)
		if err != nil {
			t.Fatal(err)
		}
		task, err = repo.GetModelManagementTask(ctx, job)
		if err != nil || task.TargetWorkerID != localModelTarget {
			t.Fatalf("explicit local task target=%q err=%v", task.TargetWorkerID, err)
		}
	})
}

func TestExecuteModelManagementAttestsLocalTarget(t *testing.T) {
	service, repo := newModelManagementService(t)
	ctx := context.Background()
	manager := &recordingManager{}
	service.AttachModelManager(manager)
	model, ok := inferencecatalog.Lookup("sound-yamnet")
	if !ok {
		t.Fatal("sound model missing from catalog")
	}

	// Queue a Hub-local install before any process owner exists: the target
	// must be the stable literal, not whatever owner happens to lease it.
	jobID, err := service.QueueModelManagement(ctx, "install", model.CatalogID, "", model.ManifestDigest(), true)
	if err != nil {
		t.Fatal(err)
	}
	owner := "local-test"
	job, err := service.pipeline.leaseLocalDeviceJob(ctx, owner, 0, time.Now().UTC())
	if err != nil || job == nil {
		t.Fatalf("local maintenance lease=%+v err=%v", job, err)
	}
	if job.ID != jobID || (job.Type != domain.JobModelInstall && job.Type != domain.JobModelProbe) {
		t.Fatalf("unexpected local lease: %+v", job)
	}
	if err := service.pipeline.executeModelManagement(ctx, *job, owner); !errors.Is(err, ErrJobCommittedEarly) {
		t.Fatalf("execution error=%v, want ErrJobCommittedEarly", err)
	}
	if len(manager.installed) != 1 || manager.installed[0].CatalogID != model.CatalogID || !manager.installed[0].LicenseAccepted {
		t.Fatalf("install calls=%+v", manager.installed)
	}
	var state string
	if err := repo.DB().QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=?`, jobID).Scan(&state); err != nil || state != string(domain.JobSucceeded) {
		t.Fatalf("job state=%q err=%v, want succeeded", state, err)
	}
	var workerID string
	if err := repo.DB().QueryRowContext(ctx, `SELECT worker_id FROM model_attestations WHERE catalog_id=? AND action='install'`, model.CatalogID).Scan(&workerID); err != nil {
		t.Fatal(err)
	}
	if workerID != localModelTarget {
		t.Fatalf("local attestation keyed %q, want %q", workerID, localModelTarget)
	}

	// A restart leases nothing: the job is terminal and the attestation is the
	// only record.
	second, err := service.pipeline.leaseLocalDeviceJob(ctx, "local-restart", 0, time.Now().UTC())
	if err != nil || second != nil {
		t.Fatalf("post-restart lease=%+v err=%v", second, err)
	}
}

func TestExecuteModelManagementWithoutManagerFailsLocally(t *testing.T) {
	service, repo := newModelManagementService(t)
	ctx := context.Background()
	model, ok := inferencecatalog.Lookup("sound-yamnet")
	if !ok {
		t.Fatal("sound model missing from catalog")
	}
	// No manager attached: the Hub must refuse to pretend a local install
	// happened. The pipeline never leases local maintenance without one, so
	// the queued row waits for a restart that re-attaches the runtime (or a
	// Worker to which the operator re-targets it).
	jobID, err := service.QueueModelManagement(ctx, "install", model.CatalogID, "", model.ManifestDigest(), true)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.pipeline.leaseLocalDeviceJob(ctx, "local-test", 0, time.Now().UTC())
	if err != nil || job != nil {
		t.Fatalf("lease without manager=%+v err=%v, want nothing", job, err)
	}
	var state string
	if err := repo.DB().QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=?`, jobID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state == string(domain.JobSucceeded) {
		t.Fatal("unconfigured Hub recorded a successful local install")
	}
	var attestations int
	if err := repo.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM model_attestations`).Scan(&attestations); err != nil {
		t.Fatal(err)
	}
	if attestations != 0 {
		t.Fatal("unconfigured Hub attested an install")
	}
}

func modelTaskCount(t *testing.T, repo *sqlite.Repository) int {
	t.Helper()
	var count int
	if err := repo.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM model_management_tasks`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
