package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

// insertMaintenanceTask seeds one model management unit the way
// CreateModelManagementTask would, with explicit ids so tests can target rows
// directly.
func insertMaintenanceTask(t *testing.T, repo *Repository, jobID, target, catalogID, manifestDigest, action string, now time.Time) {
	t.Helper()
	if _, err := repo.db.Exec(`INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at) VALUES(?,NULL,?,'pending',10,0,3,?,?,?,?)`,
		jobID, maintenanceJobType(action), formatTime(now), "management-"+jobID, formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`INSERT INTO model_management_tasks(job_id,target_worker_id,catalog_id,manifest_digest,action,license_accepted_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		jobID, target, catalogID, manifestDigest, action, licenseAcceptedColumn(action, now), formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
}

func maintenanceJobType(action string) string {
	if action == "install" {
		return string(domain.JobModelInstall)
	}
	return string(domain.JobModelProbe)
}

func licenseAcceptedColumn(action string, now time.Time) any {
	if action == "install" {
		return formatTime(now)
	}
	return nil
}

// insertMaintenanceWorker registers one Worker with the given maintenance grant so
// lease/complete authorization has a row to read.
func insertMaintenanceWorker(t *testing.T, repo *Repository, workerID string, modelManagementOperations []string, status string) {
	t.Helper()
	capabilities, err := json.Marshal(remote.WorkerCapabilities{ModelManagementOperations: modelManagementOperations})
	if err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.Exec(`INSERT INTO workers(id,name,platform,token_hash,status,capabilities_json,last_seen_at,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		workerID, workerID, "linux", "token-"+workerID, status, string(capabilities), now, now); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseNextWorkerInferenceMatchesConfigHashAndGrant(t *testing.T) {
	repo, _ := newInferenceRepository(t)
	model, ok := inferencecatalog.Lookup("sound-yamnet")
	if !ok {
		t.Fatal("sound model missing from catalog")
	}
	ctx := context.Background()
	request := inferenceTestRequest(domain.OpSoundEvents, "asset-inference", "", "config-hash")
	request.ModelID = model.ModelID
	request.ModelRevision = model.Revision
	request.ConfigHash = "config-hash-a"
	jobID := enqueueInferenceTestTask(t, repo, request, "task-key-config-hash")
	// Capture the lease clock after the task is queued: run_after must already
	// be due when the lease predicate runs.
	now := time.Now().UTC()
	granted, err := json.Marshal(remote.WorkerCapabilities{InferenceOperations: []string{string(domain.OpSoundEvents)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`INSERT INTO workers(id,name,platform,token_hash,status,capabilities_json,last_seen_at,created_at) VALUES('worker-hash','w','linux','token','online',?,?,?)`, string(granted), formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordModelAttestation(ctx, "worker-hash", model.CatalogID, model.ManifestDigest(), "install", "{}"); err != nil {
		t.Fatal(err)
	}
	matching := remote.InferenceCapability{
		Operation: domain.OpSoundEvents, ModelID: model.ModelID, ModelRevision: model.Revision,
		ConfigHash: "config-hash-a", Available: true,
	}
	mismatched := matching
	mismatched.ConfigHash = "config-hash-b"

	// A configured runtime pins the config hash it was built with; a task
	// staged under another configuration must not lease to it.
	job, err := repo.LeaseNextWorkerInference(ctx, "worker-hash", time.Minute, []string{"root-inference"}, []remote.InferenceCapability{mismatched}, 0, now)
	if err != nil || job != nil {
		t.Fatalf("mismatched config hash leased job=%+v err=%v", job, err)
	}
	job, err = repo.LeaseNextWorkerInference(ctx, "worker-hash", time.Minute, []string{"root-inference"}, []remote.InferenceCapability{matching}, 0, now)
	if err != nil || job == nil || job.JobID != jobID {
		t.Fatalf("matching config hash lease=%+v err=%v", job, err)
	}
}

func TestLeaseNextWorkerInferenceFailsClosedWithoutPersistedGrant(t *testing.T) {
	repo, _ := newInferenceRepository(t)
	model, ok := inferencecatalog.Lookup("sound-yamnet")
	if !ok {
		t.Fatal("sound model missing from catalog")
	}
	ctx := context.Background()
	request := inferenceTestRequest(domain.OpSoundEvents, "asset-inference", "", "no-grant")
	request.ModelID = model.ModelID
	request.ModelRevision = model.Revision
	jobID := enqueueInferenceTestTask(t, repo, request, "task-key-no-grant")
	now := time.Now().UTC()
	if err := repo.RecordModelAttestation(ctx, "worker-unguarded", model.CatalogID, model.ManifestDigest(), "install", "{}"); err != nil {
		t.Fatal(err)
	}
	// The Worker announces the operation, an attestation exists, but the
	// persisted grant never covered it: a direct repository caller must not
	// lease past the authorization the enrollment recorded.
	if _, err := repo.db.Exec(`INSERT INTO workers(id,name,platform,token_hash,status,capabilities_json,last_seen_at,created_at) VALUES('worker-unguarded','w','linux','token','online','{}',?,?)`, formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	caps := []remote.InferenceCapability{{
		Operation: domain.OpSoundEvents, ModelID: model.ModelID, ModelRevision: model.Revision,
		ConfigHash: request.ConfigHash, Available: true,
	}}
	job, err := repo.LeaseNextWorkerInference(ctx, "worker-unguarded", time.Minute, []string{"root-inference"}, caps, 0, now)
	if err != nil || job != nil {
		t.Fatalf("ungranted capability leased job=%+v err=%v", job, err)
	}
	// Granting the operation makes the same lease succeed.
	granted, err := json.Marshal(remote.WorkerCapabilities{InferenceOperations: []string{string(domain.OpSoundEvents)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`UPDATE workers SET capabilities_json=? WHERE id='worker-unguarded'`, string(granted)); err != nil {
		t.Fatal(err)
	}
	job, err = repo.LeaseNextWorkerInference(ctx, "worker-unguarded", time.Minute, []string{"root-inference"}, caps, 0, now)
	if err != nil || job == nil || job.JobID != jobID {
		t.Fatalf("granted lease=%+v err=%v", job, err)
	}
}

func TestMigration0042RewritesLegacyLocalTargets(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "legacy-model-targets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	applyMigrationsThrough(t, ctx, repo, "0041_worker_pairing_grants.sql")
	now := formatTime(time.Now().UTC())
	// Simulate the pre-0042 state: Hub-local tasks targeted the queuing
	// process's lease owner, and one task targets a Worker whose id happens to
	// share the legacy shape (the guard must leave it alone).
	for _, target := range []string{"local-legacy-a", "local-legacy-b", "local-worker"} {
		jobID := "legacy-" + target
		if _, err := repo.db.Exec(`INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at) VALUES(?,NULL,'model_install','pending',10,0,3,?,?,?,?)`, jobID, now, "legacy-"+target, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.Exec(`INSERT INTO model_management_tasks(job_id,target_worker_id,catalog_id,manifest_digest,action,license_accepted_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			jobID, target, "sound-yamnet", "sha256:manifest", "install", now, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.db.Exec(`INSERT INTO workers(id,name,platform,token_hash,status,capabilities_json,last_seen_at,created_at) VALUES('local-worker','legacy','linux','token','online','{}',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("apply 0042: %v", err)
	}
	rows := map[string]string{}
	query, err := repo.db.Query(`SELECT job_id,target_worker_id FROM model_management_tasks WHERE job_id LIKE 'legacy-%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer query.Close()
	for query.Next() {
		var jobID, target string
		if err := query.Scan(&jobID, &target); err != nil {
			t.Fatal(err)
		}
		rows[jobID] = target
	}
	if err := query.Err(); err != nil {
		t.Fatal(err)
	}
	if rows["legacy-local-legacy-a"] != "local" || rows["legacy-local-legacy-b"] != "local" {
		t.Fatalf("legacy process targets were not rewritten: %v", rows)
	}
	if rows["legacy-local-worker"] != "local-worker" {
		t.Fatalf("a persisted Worker id must keep its own target, got %q", rows["legacy-local-worker"])
	}
}

func TestLeaseNextModelManagementMatchesTargetAndPermission(t *testing.T) {
	repo, _ := newInferenceRepository(t)
	now := time.Now().UTC().Add(-time.Second)
	insertMaintenanceTask(t, repo, "job-install", "worker-a", "sound-yamnet", "sha256:manifest", "install", now)
	insertMaintenanceWorker(t, repo, "worker-a", []string{"install", "probe"}, "online")
	ctx := context.Background()

	t.Run("operations gate both select and update", func(t *testing.T) {
		// A probe-only grant may not receive the install task, in the select
		// or the compare-and-swap that actually takes the lease.
		job, err := repo.LeaseNextModelManagement(ctx, "worker-a", "worker-a", []string{"probe"}, time.Minute, now)
		if err != nil || job != nil {
			t.Fatalf("probe-only grant leased=%+v err=%v", job, err)
		}
		leased, err := repo.LeaseNextModelManagement(ctx, "worker-a", "worker-a", []string{"install"}, time.Minute, now)
		if err != nil || leased == nil {
			t.Fatalf("install grant lease=%+v err=%v", leased, err)
		}
		if leased.JobID != "job-install" || leased.JobType != domain.JobModelInstall {
			t.Fatalf("unexpected lease: %+v", leased)
		}
		if leased.ModelManagement == nil || leased.ModelManagement.TargetWorkerID != "worker-a" ||
			leased.ModelManagement.CatalogID != "sound-yamnet" || !leased.ModelManagement.LicenseAccepted {
			t.Fatalf("task payload: %+v", leased.ModelManagement)
		}
		var state, owner string
		if err := repo.db.QueryRow(`SELECT state,lease_owner FROM jobs WHERE id='job-install'`).Scan(&state, &owner); err != nil || state != "running" || owner != "worker-a" {
			t.Fatalf("leased job state=%q owner=%q err=%v", state, owner, err)
		}
	})

	t.Run("empty operations mean no work", func(t *testing.T) {
		job, err := repo.LeaseNextModelManagement(ctx, "worker-a", "worker-a", nil, time.Minute, now)
		if err != nil || job != nil {
			t.Fatalf("empty operations leased=%+v err=%v", job, err)
		}
	})

	t.Run("another target cannot lease", func(t *testing.T) {
		job, err := repo.LeaseNextModelManagement(ctx, "worker-b", "worker-b", []string{"install", "probe"}, time.Minute, now)
		if err != nil || job != nil {
			t.Fatalf("foreign target leased=%+v err=%v", job, err)
		}
	})

	t.Run("cancelled task is not leased", func(t *testing.T) {
		if _, err := repo.db.Exec(`UPDATE model_management_tasks SET cancel_requested=1 WHERE job_id='job-install'`); err != nil {
			t.Fatal(err)
		}
		job, err := repo.LeaseNextModelManagement(ctx, "worker-a", "worker-a", []string{"install", "probe"}, time.Minute, now)
		if err != nil || job != nil {
			t.Fatalf("cancelled task leased=%+v err=%v", job, err)
		}
	})
}

func TestLeaseNextModelManagementLocalTargetSurvivesRestart(t *testing.T) {
	repo, _ := newInferenceRepository(t)
	now := time.Now().UTC().Add(-time.Second)
	insertMaintenanceTask(t, repo, "job-local", "local", "sound-yamnet", "sha256:manifest", "install", now)
	ctx := context.Background()
	if _, err := repo.LeaseNextModelManagement(ctx, "local", "local-first", []string{"install"}, time.Minute, now); err != nil {
		t.Fatal(err)
	}
	// A restart mints a new process owner. The expired lease must hand the
	// same task to the new owner because the target, not the old process
	// identity, is what the task is queued against.
	expired := now.Add(-time.Minute)
	if _, err := repo.db.Exec(`UPDATE jobs SET lease_expires_at=? WHERE id='job-local'`, formatTime(expired)); err != nil {
		t.Fatal(err)
	}
	leased, err := repo.LeaseNextModelManagement(ctx, "local", "local-second", []string{"install", "probe"}, time.Minute, now)
	if err != nil || leased == nil {
		t.Fatalf("post-restart lease=%+v err=%v", leased, err)
	}
	if leased.JobID != "job-local" || leased.LeaseDeadline.IsZero() {
		t.Fatalf("unexpected lease: %+v", leased)
	}
	var owner string
	if err := repo.db.QueryRow(`SELECT lease_owner FROM jobs WHERE id='job-local'`).Scan(&owner); err != nil || owner != "local-second" {
		t.Fatalf("lease owner=%q err=%v, want the new process owner", owner, err)
	}
}

func TestCompleteModelManagementAttestsWorkerAndRepeats(t *testing.T) {
	repo, _ := newInferenceRepository(t)
	now := time.Now().UTC().Add(-time.Second)
	model, ok := inferencecatalog.Lookup("sound-yamnet")
	if !ok {
		t.Fatal("sound model missing from catalog")
	}
	insertMaintenanceTask(t, repo, "job-complete", "worker-a", model.CatalogID, model.ManifestDigest(), "install", now)
	insertMaintenanceWorker(t, repo, "worker-a", []string{"install", "probe"}, "online")
	ctx := context.Background()
	deadline := time.Now().UTC().Add(time.Minute)
	if _, err := repo.db.Exec(`UPDATE jobs SET state='running',attempt_count=1,lease_owner='worker-a',lease_expires_at=? WHERE id='job-complete'`, formatTime(deadline)); err != nil {
		t.Fatal(err)
	}

	// Another identity cannot record the completion, expired or not.
	if err := repo.CompleteModelManagement(ctx, "job-complete", "worker-other"); err == nil {
		t.Fatal("another worker completed the task")
	}
	if got := inferenceTestCount(t, repo, "model_attestations", ""); got != 0 {
		t.Fatalf("foreign completion wrote %d attestations", got)
	}
	if err := repo.CompleteModelManagement(ctx, "job-complete", "worker-a"); err != nil {
		t.Fatalf("authorized completion: %v", err)
	}
	var state string
	if err := repo.db.QueryRow(`SELECT state FROM jobs WHERE id='job-complete'`).Scan(&state); err != nil || state != string(domain.JobSucceeded) {
		t.Fatalf("job state=%q err=%v, want succeeded", state, err)
	}
	var verifiedAt string
	if err := repo.db.QueryRow(`SELECT verified_at FROM model_attestations WHERE worker_id='worker-a' AND catalog_id=? AND manifest_digest=? AND action='install'`, model.CatalogID, model.ManifestDigest()).Scan(&verifiedAt); err != nil {
		t.Fatalf("attestation row: %v", err)
	}

	// A Worker retrying after a lost HTTP response repeats cleanly and keeps
	// the same persisted attestation.
	if err := repo.CompleteModelManagement(ctx, "job-complete", "worker-a"); err != nil {
		t.Fatalf("idempotent completion: %v", err)
	}
	var verifiedAtAgain string
	if err := repo.db.QueryRow(`SELECT verified_at FROM model_attestations WHERE worker_id='worker-a' AND catalog_id=? AND manifest_digest=? AND action='install'`, model.CatalogID, model.ManifestDigest()).Scan(&verifiedAtAgain); err != nil {
		t.Fatal(err)
	}
	if verifiedAtAgain != verifiedAt {
		t.Fatalf("idempotent completion rewrote the attestation: %q != %q", verifiedAtAgain, verifiedAt)
	}

	// An expired lease cannot complete, and cannot resurrect the job.
	repo2, _ := newInferenceRepository(t)
	insertMaintenanceTask(t, repo2, "job-expired", "worker-a", model.CatalogID, model.ManifestDigest(), "probe", now)
	insertMaintenanceWorker(t, repo2, "worker-a", []string{"probe"}, "online")
	if _, err := repo2.db.Exec(`UPDATE jobs SET state='running',attempt_count=1,lease_owner='worker-a',lease_expires_at=? WHERE id='job-expired'`, formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if err := repo2.CompleteModelManagement(ctx, "job-expired", "worker-a"); err == nil {
		t.Fatal("expired lease completed the task")
	}
	var state2 string
	if err := repo2.db.QueryRow(`SELECT state FROM jobs WHERE id='job-expired'`).Scan(&state2); err != nil || state2 != string(domain.JobRunning) {
		t.Fatalf("expired completion left state=%q err=%v", state2, err)
	}
}

func TestCompleteModelManagementRejectsRevokedGrantAndStaleDigest(t *testing.T) {
	model, ok := inferencecatalog.Lookup("sound-yamnet")
	if !ok {
		t.Fatal("sound model missing from catalog")
	}
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Second)
	deadline := time.Now().UTC().Add(time.Minute)

	t.Run("grant withdrawn before completion", func(t *testing.T) {
		repo, _ := newInferenceRepository(t)
		insertMaintenanceTask(t, repo, "job-grant", "worker-a", model.CatalogID, model.ManifestDigest(), "install", now)
		insertMaintenanceWorker(t, repo, "worker-a", []string{"install"}, "online")
		if _, err := repo.db.Exec(`UPDATE jobs SET state='running',attempt_count=1,lease_owner='worker-a',lease_expires_at=? WHERE id='job-grant'`, formatTime(deadline)); err != nil {
			t.Fatal(err)
		}
		narrowed, err := json.Marshal(remote.WorkerCapabilities{ModelManagementOperations: []string{"probe"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.Exec(`UPDATE workers SET capabilities_json=? WHERE id='worker-a'`, string(narrowed)); err != nil {
			t.Fatal(err)
		}
		if err := repo.CompleteModelManagement(ctx, "job-grant", "worker-a"); err == nil {
			t.Fatal("completion succeeded after the grant was narrowed")
		}
		var state string
		if err := repo.db.QueryRow(`SELECT state FROM jobs WHERE id='job-grant'`).Scan(&state); err != nil || state != string(domain.JobRunning) {
			t.Fatalf("job state=%q err=%v, want still running", state, err)
		}
	})

	t.Run("revoked worker cannot complete", func(t *testing.T) {
		repo, _ := newInferenceRepository(t)
		insertMaintenanceTask(t, repo, "job-revoked", "worker-a", model.CatalogID, model.ManifestDigest(), "install", now)
		insertMaintenanceWorker(t, repo, "worker-a", []string{"install"}, "online")
		if _, err := repo.db.Exec(`UPDATE jobs SET state='running',attempt_count=1,lease_owner='worker-a',lease_expires_at=? WHERE id='job-revoked'`, formatTime(deadline)); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.Exec(`UPDATE workers SET status='revoked' WHERE id='worker-a'`); err != nil {
			t.Fatal(err)
		}
		if err := repo.CompleteModelManagement(ctx, "job-revoked", "worker-a"); err == nil {
			t.Fatal("revoked worker completed the task")
		}
	})

	t.Run("stale manifest digest never attests", func(t *testing.T) {
		repo, _ := newInferenceRepository(t)
		insertMaintenanceTask(t, repo, "job-stale", "worker-a", model.CatalogID, "sha256:stale", "install", now)
		insertMaintenanceWorker(t, repo, "worker-a", []string{"install"}, "online")
		if _, err := repo.db.Exec(`UPDATE jobs SET state='running',attempt_count=1,lease_owner='worker-a',lease_expires_at=? WHERE id='job-stale'`, formatTime(deadline)); err != nil {
			t.Fatal(err)
		}
		err := repo.CompleteModelManagement(ctx, "job-stale", "worker-a")
		if err == nil || !strings.Contains(err.Error(), "no longer matches") {
			t.Fatalf("stale digest completion error=%v", err)
		}
		var state string
		if err := repo.db.QueryRow(`SELECT state FROM jobs WHERE id='job-stale'`).Scan(&state); err != nil || state != string(domain.JobRunning) {
			t.Fatalf("stale digest left state=%q err=%v", state, err)
		}
	})
}

func TestCompleteModelManagementLocalTargetAttestsUnderStableIdentity(t *testing.T) {
	repo, _ := newInferenceRepository(t)
	now := time.Now().UTC().Add(-time.Second)
	model, ok := inferencecatalog.Lookup("sound-yamnet")
	if !ok {
		t.Fatal("sound model missing from catalog")
	}
	insertMaintenanceTask(t, repo, "job-local", "local", model.CatalogID, model.ManifestDigest(), "install", now)
	ctx := context.Background()
	deadline := time.Now().UTC().Add(time.Minute)
	if _, err := repo.db.Exec(`UPDATE jobs SET state='running',attempt_count=1,lease_owner='local-first',lease_expires_at=? WHERE id='job-local'`, formatTime(deadline)); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteModelManagement(ctx, "job-local", "local-first"); err != nil {
		t.Fatalf("local completion: %v", err)
	}
	var workerID string
	if err := repo.db.QueryRow(`SELECT worker_id FROM model_attestations WHERE catalog_id=? AND action='install'`, model.CatalogID).Scan(&workerID); err != nil {
		t.Fatal(err)
	}
	if workerID != "local" {
		t.Fatalf("local attestation worker_id=%q, want the stable local target", workerID)
	}
	var eventWorker any
	if err := repo.db.QueryRow(`SELECT worker_id FROM job_events WHERE job_id='job-local' AND event_type='succeeded'`).Scan(&eventWorker); err != nil {
		t.Fatal(err)
	}
	if eventWorker != nil {
		t.Fatalf("local completion event references worker %v", eventWorker)
	}
}
