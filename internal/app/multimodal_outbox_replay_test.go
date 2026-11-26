package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/remote"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

// outboxReplayFixture builds one asset with a real location (real
// fingerprint, size and probe mtime, because the drained tasks must key on
// the canonical derive identity those fields reconstruct), extracted audio,
// and a committed analysis. It returns the asset ID and the canonical derive
// identity a cache repair's maintenance key reconstructs to.
func outboxReplayFixture(t *testing.T, repo *sqlite.Repository, dir, id string) (string, string) {
	t.Helper()
	ctx := context.Background()
	footage := filepath.Join(dir, "footage")
	if err := os.MkdirAll(footage, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(footage, id+".mov")
	if err := os.WriteFile(source, []byte("footage"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, footage)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := repo.UpsertScannedFile(ctx, root, id+".mov", source, info, "fp-"+id)
	if err != nil {
		t.Fatal(err)
	}
	assetID := scanned.AssetID
	if err := repo.SaveMediaMetadata(ctx, assetID, domain.MediaMetadata{DurationMS: 20000, HasAudio: true}, "test", "", ""); err != nil {
		t.Fatal(err)
	}
	// Extracted audio artifact: the row is what makes audio-driven
	// operations plannable after the derive replays.
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) VALUES('audio-`+id+`',?,'audio','audio-16k-v1','`+filepath.Join(dir, "audio.m4a")+`',10,?)`, assetID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	// A committed analysis: the repair derive must not enqueue the paid
	// chain on top of it, and the canonical rows must survive the replay.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO model_runs(id,asset_id,capability,provider,model,input_hash,prompt_version,schema_version,state,request_json,started_at) VALUES('run-`+id+`',?,'vision','fixture','fixture-model','committed-input','footage-analysis-v4','asset-analysis/v2','committed','{}','`+now+`')`, assetID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO asset_analysis(asset_id,source_run_id,schema_version,asset_type,shot_size,camera_motion,audio_type,lighting,people_count,has_speech,quality,summary,scene_tags_json,subjects_json,mood_tags_json,usable_as_json,quality_flags_json,extra_tags_json,editorial_reason,updated_at) VALUES(?,'run-`+id+`','asset-analysis/v2','other','unknown','unknown','unknown','unknown',0,0,'unknown','committed summary','[]','[]','[]','[]','[]','[]','','`+now+`')`, assetID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO asset_shots(id,asset_id,source_run_id,ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at) VALUES('shot-`+id+`',?,'run-`+id+`',0,0,2000,'committed shot','[]','[]','[]','[]',0.9,'`+now+`')`, assetID); err != nil {
		t.Fatal(err)
	}
	loc, err := repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	maintenance := "cache-rederive:" + hashStrings("outbox", id)
	canonical := canonicalDeriveInputHash(maintenance, loc.QuickFingerprint, loc.FileSize, loc.ProbeModifiedNS)
	return assetID, canonical
}

func outboxService(t *testing.T, repo *sqlite.Repository, dir string) *Service {
	t.Helper()
	service, err := NewService(repo, config.Config{
		DataDir:  dir,
		CacheDir: filepath.Join(dir, "cache"),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func leaseOutboxFixtureDerive(t *testing.T, repo *sqlite.Repository, assetID string) (*remote.WorkerJob, string) {
	t.Helper()
	ctx := context.Background()
	location, err := repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	worker, _, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{
		Name: "derive fixture", Platform: "linux",
		Capabilities: remote.WorkerCapabilities{Proxy: true, LibraryRoots: []string{location.RootID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextWorkerDerive(ctx, worker, time.Minute, domain.LeaseFilter{})
	if err != nil || job == nil || job.AssetID != assetID {
		t.Fatalf("lease derive for %s: job=%+v err=%v", assetID, job, err)
	}
	return job, worker.ID
}

func outboxMarkers(t *testing.T, repo *sqlite.Repository) int {
	t.Helper()
	var n int
	if err := repo.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM derive_modality_outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func outboxInferenceTasks(t *testing.T, repo *sqlite.Repository, assetID string) map[domain.InferenceOperation]string {
	t.Helper()
	ctx := context.Background()
	rows, err := repo.DB().QueryContext(ctx, `SELECT t.operation,t.task_key FROM inference_tasks t JOIN jobs j ON j.id=t.job_id WHERE j.asset_id=?`, assetID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	tasks := map[domain.InferenceOperation]string{}
	for rows.Next() {
		var op domain.InferenceOperation
		var key string
		if err := rows.Scan(&op, &key); err != nil {
			t.Fatal(err)
		}
		tasks[op] = key
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return tasks
}

func outboxPaidChainJobs(t *testing.T, repo *sqlite.Repository, assetID string) []domain.JobType {
	t.Helper()
	rows, err := repo.DB().QueryContext(context.Background(), `SELECT job_type FROM jobs WHERE asset_id=? AND job_type IN ('analyze','speech_gate','transcribe')`, assetID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var types []domain.JobType
	for rows.Next() {
		var jt domain.JobType
		if err := rows.Scan(&jt); err != nil {
			t.Fatal(err)
		}
		types = append(types, jt)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return types
}

// A remote cache-repair derive completes at the repository boundary; the
// marker must survive a Hub restart, and replaying it must queue exactly the
// enabled audio modalities under the canonical derive identity — never a
// second paid analysis, and never a task keyed by the maintenance key.
func TestDeriveModalityOutboxReplaysAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assetID, canonical := outboxReplayFixture(t, repo, dir, "replay")
	settings := domain.DefaultMultimodalSettings()
	settings.Enabled[domain.OpSoundEvents] = true
	settings.Enabled[domain.OpAudioEmbedding] = true
	if err := repo.PutMultimodalSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}

	// The repair derive runs and completes at the repository level, the way
	// a Worker's completion call does.
	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "cache-rederive:"+canonical, 50); err != nil {
		t.Fatal(err)
	}
	job, workerID := leaseOutboxFixtureDerive(t, repo, assetID)
	if err := repo.CompleteWorkerJob(ctx, job.JobID, workerID, domain.JobSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if outboxMarkers(t, repo) != 1 {
		t.Fatal("completion must record exactly one outbox marker")
	}
	if paid := outboxPaidChainJobs(t, repo, assetID); len(paid) != 0 {
		t.Fatalf("committed analysis must not re-enqueue the paid chain: %+v", paid)
	}
	if tasks := outboxInferenceTasks(t, repo, assetID); len(tasks) != 0 {
		t.Fatalf("modalities must not be queued before the drain: %+v", tasks)
	}

	// Restart simulation: a fresh service over the same database drains the
	// persisted marker.
	service := outboxService(t, repo, dir)
	if err := service.drainDeriveModalityOutbox(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if outboxMarkers(t, repo) != 0 {
		t.Fatal("marker must be cleared after a successful replay")
	}
	planned, err := service.PlanAssetModalities(ctx, assetID, canonical)
	if err != nil {
		t.Fatal(err)
	}
	want := map[domain.InferenceOperation]string{}
	for _, task := range planned {
		want[task.Operation] = task.TaskKey
	}
	got := outboxInferenceTasks(t, repo, assetID)
	if len(got) == 0 {
		t.Fatal("drain queued no modalities")
	}
	for op, key := range want {
		if got[op] != key {
			t.Fatalf("queued %s task key = %q, want the canonical-identity key %q", op, got[op], key)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("queued tasks %+v, want exactly the planned ones %+v", got, want)
	}
	if paid := outboxPaidChainJobs(t, repo, assetID); len(paid) != 0 {
		t.Fatalf("drain must not enqueue the paid chain for a committed asset: %+v", paid)
	}

	// A second drain after the marker cleared is a no-op and cannot
	// duplicate tasks.
	if err := service.drainDeriveModalityOutbox(ctx); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if again := outboxInferenceTasks(t, repo, assetID); len(again) != len(want) {
		t.Fatalf("second drain duplicated tasks: %+v", again)
	}
}

// An ordinary (non-repair) derive replays the same way, and its identity is
// the job's own input hash.
func TestDeriveModalityOutboxReplaysPlainDerive(t *testing.T) {
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "outbox-plain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assetID, canonical := outboxReplayFixture(t, repo, dir, "plain")
	service := outboxService(t, repo, dir)
	settings := domain.DefaultMultimodalSettings()
	settings.Enabled[domain.OpSoundEvents] = true
	if err := repo.PutMultimodalSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}

	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, canonical, 50); err != nil {
		t.Fatal(err)
	}
	job, workerID := leaseOutboxFixtureDerive(t, repo, assetID)
	if err := repo.CompleteWorkerJob(ctx, job.JobID, workerID, domain.JobSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if err := service.drainDeriveModalityOutbox(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	got := outboxInferenceTasks(t, repo, assetID)
	planned, err := service.PlanAssetModalities(ctx, assetID, canonical)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(planned) != 1 || got[planned[0].Operation] != planned[0].TaskKey {
		t.Fatalf("plain derive replay queued %+v, want the planned %+v", got, planned)
	}
}

// The error-returning core reports a real enqueue failure instead of
// swallowing it into a log line, so the outbox marker stays queued and the
// serving maintenance loop can retry.
func TestEnqueuePlannedModalitiesReportsFailures(t *testing.T) {
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "err.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service := outboxService(t, repo, dir)
	settings := domain.DefaultMultimodalSettings()
	settings.Enabled[domain.OpAudioEmbedding] = true
	if err := repo.PutMultimodalSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	// An unknown asset has no location, so planning cannot build a task; the
	// hook variant must still log-and-continue, but the internal variant the
	// outbox drain uses must surface the failure.
	if err := service.enqueuePlannedModalities(ctx, "missing-asset", "hash"); err == nil {
		t.Fatal("enqueuePlannedModalities must report the planning failure")
	}
	service.EnqueuePlannedModalities(ctx, "missing-asset", "hash")
}

// 0047: the marker is owned by its derive job row and cannot outlive it.
func TestDeriveModalityOutboxCascadesWithJob(t *testing.T) {
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "cascade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO jobs(id,job_type,state,priority,run_after,input_hash,created_at,updated_at) VALUES('job-cascade','derive','succeeded',0,?,'h','`+now+`','`+now+`')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO derive_modality_outbox(derive_job_id) VALUES('job-cascade')`); err != nil {
		t.Fatal(err)
	}
	if outboxMarkers(t, repo) != 1 {
		t.Fatal("marker missing before job deletion")
	}
	if _, err := repo.DB().ExecContext(ctx, `DELETE FROM jobs WHERE id='job-cascade'`); err != nil {
		t.Fatal(err)
	}
	if outboxMarkers(t, repo) != 0 {
		t.Fatal("marker must cascade away with its derive job")
	}
}
