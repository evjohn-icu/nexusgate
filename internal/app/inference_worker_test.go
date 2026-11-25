package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/remote"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

func TestWorkerInferenceLeaseProgressAndResult(t *testing.T) {
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "worker-inference.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root','/media','` + now + `','` + now + `')`,
		`INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset','fp',100,'discovered','` + now + `','` + now + `')`,
		`INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc','asset','root','clip.mp4','/media/clip.mp4',1,1,1,'` + now + `')`,
		`INSERT INTO media_metadata(asset_id,ffprobe_json,exiftool_json,normalized_json,probe_version,updated_at,duration_ms) VALUES('asset','{}','{}','{}','test','` + now + `',10000)`,
		`INSERT INTO workers(id,name,platform,token_hash,status,capabilities_json,last_seen_at,created_at) VALUES('worker-other','other','linux','token','online','{}','` + now + `','` + now + `')`,
	} {
		if _, err := repo.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewService(repo, config.Config{
		DataDir: dir, CacheDir: filepath.Join(dir, "cache"),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	model, ok := inferencecatalog.Lookup("sound-yamnet")
	if !ok {
		t.Fatal("sound model missing from catalog")
	}
	request := domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion, OperationID: "op-worker",
		Operation: domain.OpSoundEvents, ModelID: model.ModelID, ModelRevision: model.Revision,
		InputHash: "input", ConfigHash: "config", TimeoutMS: 60000,
		Input:      domain.InferenceInput{Kind: domain.InputKindAsset, AssetID: "asset", DurationMS: 10000},
		SoundEvent: &domain.SoundEventSettings{},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(request)
	jobID, err := repo.EnqueueInferenceTask(ctx, nil, domain.InferenceTask{
		AssetID: "asset", TaskKey: "worker-task", Operation: domain.OpSoundEvents,
		RequestJSON: string(encoded), ConfigHash: request.ConfigHash, ModelRevision: request.ModelRevision,
	}, 50)
	if err != nil {
		t.Fatal(err)
	}
	capability := remote.InferenceCapability{
		Operation: domain.OpSoundEvents, SchemaVersion: domain.InferenceSchemaVersion,
		ModelID: request.ModelID, ModelRevision: request.ModelRevision, ConfigHash: request.ConfigHash,
		Backend: "cpu", Available: true,
	}
	pair, err := service.CreateWorkerPairingWithGrants(ctx, time.Minute, remote.WorkerGrants{
		InferenceOperations: []string{string(domain.OpSoundEvents)}, ModelManagementOperations: []string{"install"},
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, token, err := service.EnrollWorker(ctx, pair.Token, remote.WorkerRegistration{Name: "worker", Platform: "linux", Capabilities: remote.WorkerCapabilities{
		LibraryRoots: []string{"root"}, Inference: []remote.InferenceCapability{capability},
	}})
	if err != nil || len(worker.Capabilities.Inference) != 0 {
		t.Fatalf("enrollment inference=%v, err=%v", worker.Capabilities.Inference, err)
	}
	announce := func() {
		t.Helper()
		if err := service.HeartbeatWorker(ctx, worker.ID, "", remote.WorkerCapabilities{
			LibraryRoots: []string{"root"}, Inference: []remote.InferenceCapability{capability},
			ModelManagementOperations: []string{"probe"},
		}); err != nil {
			t.Fatal(err)
		}
		var err error
		worker, err = service.AuthenticateWorker(ctx, token)
		if err != nil || len(worker.Capabilities.Inference) != 1 || len(worker.Capabilities.ModelManagementOperations) != 1 || worker.Capabilities.ModelManagementOperations[0] != "install" {
			t.Fatalf("heartbeat capabilities=%+v, err=%v", worker.Capabilities, err)
		}
	}
	announce()
	if job, err := service.LeaseNextWorkerInference(ctx, worker, time.Minute, domain.LeaseFilter{}); err != nil || job != nil {
		t.Fatalf("before attestation: job=%+v err=%v", job, err)
	}
	installID, err := service.QueueModelManagement(ctx, "install", model.CatalogID, worker.ID, model.ManifestDigest(), true)
	if err != nil {
		t.Fatal(err)
	}
	worker.Capabilities.Inference = nil
	maintenance, err := service.LeaseNextWorkerInference(ctx, worker, time.Minute, domain.LeaseFilter{})
	if err != nil || maintenance == nil || maintenance.JobID != installID || maintenance.ModelManagement == nil {
		t.Fatalf("maintenance=%+v err=%v", maintenance, err)
	}
	if err := service.CompleteWorkerJob(ctx, installID, "worker-other", domain.JobSucceeded, ""); err == nil {
		t.Fatal("another worker completed the maintenance lease")
	}
	for range 2 {
		if err := service.CompleteWorkerJob(ctx, installID, worker.ID, domain.JobSucceeded, ""); err != nil {
			t.Fatal(err)
		}
	}
	announce()
	leased, err := service.LeaseNextWorkerInference(ctx, worker, time.Minute, domain.LeaseFilter{})
	if err != nil || leased == nil || leased.JobID != jobID {
		t.Fatalf("lease=%+v err=%v", leased, err)
	}
	progress, inferenceJob, err := service.WorkerInferenceProgress(ctx, worker.ID, jobID, "inference", 0.5, "progress", "working")
	if err != nil || !inferenceJob || !progress.LeaseValid || progress.CancelRequested {
		t.Fatalf("progress=%+v inference=%v err=%v", progress, inferenceJob, err)
	}
	result := domain.InferenceResult{
		SchemaVersion: request.SchemaVersion, OperationID: request.OperationID, Operation: request.Operation,
		ModelID: request.ModelID, ModelRevision: request.ModelRevision, InputHash: request.InputHash,
		ConfigHash: request.ConfigHash, Backend: "cpu",
		Coverage: []domain.TimeInterval{{StartMS: 0, EndMS: 1000}},
		Events:   []domain.SoundEvent{{Label: "rain", StartMS: 100, EndMS: 900, RawScore: 0.8, EvidenceStatus: "possible", BoundaryResolutionMS: 100}},
	}
	if err := service.SubmitWorkerInferenceResult(ctx, worker.ID, jobID, remote.InferenceResultSubmission{
		OperationID: request.OperationID, Result: result,
	}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := repo.DB().QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=?`, jobID).Scan(&state); err != nil || state != string(domain.JobSucceeded) {
		t.Fatalf("job state=%q err=%v", state, err)
	}
}
