package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

func newInferenceRepository(t *testing.T) (*Repository, string) {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-inference','/media',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE library_roots SET health_state='healthy' WHERE id='root-inference'`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-inference','fingerprint',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('location-inference','asset-inference','root-inference','clip.mp4','/media/clip.mp4',123,1,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO media_metadata(asset_id,ffprobe_json,exiftool_json,normalized_json,probe_version,updated_at,duration_ms) VALUES('asset-inference','{}','{}','{}','test',?,10000)`, now); err != nil {
		t.Fatal(err)
	}
	return repo, "asset-inference"
}

func inferenceTestSpace(t *testing.T, repo *Repository, modality domain.Modality) domain.EmbeddingSpace {
	t.Helper()
	space := domain.EmbeddingSpace{
		Modality:          modality,
		Provider:          "fixture-provider",
		ModelID:           "fixture-model",
		ModelRevision:     "revision-1",
		Dimension:         2,
		Normalization:     "l2",
		PreprocessingJSON: `{"fixture":true}`,
		RuntimeDigest:     "runtime-1",
	}
	id, err := domain.CanonicalSpaceID(space)
	if err != nil {
		t.Fatal(err)
	}
	space.ID = id
	if err := repo.UpsertEmbeddingSpace(context.Background(), space); err != nil {
		t.Fatal(err)
	}
	return space
}

func inferenceTestRequest(op domain.InferenceOperation, assetID, spaceID, key string) domain.InferenceRequest {
	input := domain.InferenceInput{Kind: domain.InputKindAsset, AssetID: assetID, DurationMS: 10_000}
	if assetID == "" {
		input = domain.InferenceInput{Kind: domain.InputKindText, Text: "a test query"}
	}
	req := domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion,
		OperationID:   "operation-" + key,
		Operation:     op,
		ModelID:       "fixture-model",
		ModelRevision: "revision-1",
		SpaceID:       spaceID,
		InputHash:     "input-" + key,
		ConfigHash:    "config-" + string(op),
		TimeoutMS:     60_000,
		Input:         input,
	}
	switch op {
	case domain.OpVisualEmbedding:
		req.Visual = &domain.VisualSettings{}
		if assetID != "" {
			req.Visual.Strategy = "shot_frame_pooled"
		}
	case domain.OpSoundEvents:
		req.SoundEvent = &domain.SoundEventSettings{}
	case domain.OpAudioEmbedding:
		req.Audio = &domain.AudioSettings{WindowMS: 1000, HopMS: 500}
	case domain.OpDiarization:
		req.Diarization = &domain.DiarizationSettings{}
	case domain.OpSpeakerEmbedding:
		req.Speaker = &domain.SpeakerSettings{Interval: &domain.TimeInterval{StartMS: 0, EndMS: 1000}}
	case domain.OpASR:
		req.ASR = &domain.ASRSettings{}
	case domain.OpAlignment:
		req.Alignment = &domain.AlignmentSettings{}
	}
	return req
}

func enqueueInferenceTestTask(t *testing.T, repo *Repository, req domain.InferenceRequest, key string) string {
	t.Helper()
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	task := domain.InferenceTask{
		AssetID:       req.Input.AssetID,
		TaskKey:       key,
		Operation:     req.Operation,
		RequestJSON:   string(encoded),
		ConfigHash:    req.ConfigHash,
		ModelRevision: req.ModelRevision,
	}
	jobID, err := repo.EnqueueInferenceTask(context.Background(), nil, task, 60)
	if err != nil {
		t.Fatal(err)
	}
	return jobID
}

func leaseInferenceTestJob(t *testing.T, repo *Repository, jobID, owner string, expires time.Time) {
	t.Helper()
	if _, err := repo.db.ExecContext(context.Background(), `UPDATE jobs SET state='running',attempt_count=1,lease_owner=?,lease_expires_at=? WHERE id=?`, owner, formatTime(expires), jobID); err != nil {
		t.Fatal(err)
	}
}

func inferenceTestResult(req domain.InferenceRequest) domain.InferenceResult {
	result := domain.InferenceResult{
		SchemaVersion: req.SchemaVersion,
		OperationID:   req.OperationID,
		Operation:     req.Operation,
		ModelID:       req.ModelID,
		ModelRevision: req.ModelRevision,
		SpaceID:       req.SpaceID,
		ConfigHash:    req.ConfigHash,
		InputHash:     req.InputHash,
		Backend:       "fixture-backend",
		Coverage:      []domain.TimeInterval{{StartMS: 0, EndMS: 1000}},
	}
	switch req.Operation {
	case domain.OpSoundEvents:
		result.Events = []domain.SoundEvent{{Label: "rain", StartMS: 100, EndMS: 500, RawScore: 0.8, EvidenceStatus: "possible", BoundaryResolutionMS: 100}}
	case domain.OpAudioEmbedding:
		result.Embeddings = []domain.EmbeddingRecord{{StartMS: 0, EndMS: 1000, SampleIndex: -1, Vector: []float32{1, 0}}}
	case domain.OpVisualEmbedding:
		result.Embeddings = []domain.EmbeddingRecord{{SampleIndex: -1, Vector: []float32{1, 0}}}
	case domain.OpDiarization:
		result.Turns = []domain.SpeakerTurn{{StartMS: 100, EndMS: 700, LocalSpeakerID: "speaker-1"}}
	case domain.OpASR:
		result.Segments = []domain.TranscriptSegment{{StartMS: 100, EndMS: 700, Text: "hello"}}
	case domain.OpAlignment:
		result.Words = []domain.AlignmentWord{{StartMS: 100, EndMS: 400, Text: "hello"}}
	case domain.OpSpeakerEmbedding:
		result.Embeddings = []domain.EmbeddingRecord{{StartMS: 0, EndMS: 1000, SampleIndex: -1, Vector: []float32{1, 0}}}
	}
	return result
}

func inferenceTestCount(t *testing.T, repo *Repository, table, where string, args ...any) int {
	t.Helper()
	var count int
	query := "SELECT COUNT(*) FROM " + table
	if where != "" {
		query += " WHERE " + where
	}
	if err := repo.db.QueryRowContext(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestInferenceEnqueueDeduplicatesAssetAndAssetlessTasks(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	space := inferenceTestSpace(t, repo, domain.ModalityVisual)
	assetRequest := inferenceTestRequest(domain.OpSoundEvents, assetID, "", "asset-dedupe")
	first := enqueueInferenceTestTask(t, repo, assetRequest, "task-key-asset")
	second := enqueueInferenceTestTask(t, repo, assetRequest, "task-key-asset")
	if first != second {
		t.Fatalf("asset enqueue returned jobs %q and %q", first, second)
	}
	queryRequest := inferenceTestRequest(domain.OpVisualEmbedding, "", space.ID, "query-dedupe")
	queryFirst := enqueueInferenceTestTask(t, repo, queryRequest, "task-key-query")
	querySecond := enqueueInferenceTestTask(t, repo, queryRequest, "task-key-query")
	if queryFirst != querySecond {
		t.Fatalf("assetless enqueue returned jobs %q and %q", queryFirst, querySecond)
	}
	if got := inferenceTestCount(t, repo, "jobs", "job_type='inference'"); got != 2 {
		t.Fatalf("inference jobs=%d, want 2", got)
	}
	if got := inferenceTestCount(t, repo, "inference_tasks", ""); got != 2 {
		t.Fatalf("inference tasks=%d, want 2", got)
	}
}

func TestInferenceStageRequiresLiveOwnerAndLease(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	req := inferenceTestRequest(domain.OpSoundEvents, assetID, "", "stage-lease")
	jobID := enqueueInferenceTestTask(t, repo, req, "task-key-stage-lease")
	now := time.Now().UTC()
	leaseInferenceTestJob(t, repo, jobID, "owner-a", now.Add(time.Minute))
	result := inferenceTestResult(req)
	wrongOperation := result
	wrongOperation.OperationID = "different-operation"
	if err := repo.StageInferenceTask(context.Background(), jobID, "owner-a", wrongOperation); !errors.Is(err, domain.ErrPermanentFailure) {
		t.Fatalf("mismatched operation id error=%v, want permanent failure", err)
	}
	if err := repo.StageInferenceTask(context.Background(), jobID, "owner-b", result); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("wrong-owner stage error=%v, want ErrJobLeaseLost", err)
	}
	if _, err := repo.db.Exec(`UPDATE jobs SET lease_expires_at=? WHERE id=?`, formatTime(now.Add(-time.Second)), jobID); err != nil {
		t.Fatal(err)
	}
	if err := repo.StageInferenceTask(context.Background(), jobID, "owner-a", result); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("expired stage error=%v, want ErrJobLeaseLost", err)
	}
	if got := inferenceTestCount(t, repo, "inference_tasks", "job_id=? AND staged_json IS NOT NULL", jobID); got != 0 {
		t.Fatalf("staged rows after rejected calls=%d, want 0", got)
	}
}

func TestCancelInferenceTaskDistinguishesPendingAndRunning(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	ctx := context.Background()
	pendingRequest := inferenceTestRequest(domain.OpSoundEvents, assetID, "", "cancel-pending")
	pendingID := enqueueInferenceTestTask(t, repo, pendingRequest, "task-key-cancel-pending")
	if err := repo.CancelInferenceTask(ctx, pendingID); err != nil {
		t.Fatal(err)
	}
	pendingTask, err := repo.GetInferenceTask(ctx, pendingID)
	if err != nil || !pendingTask.CancelRequested {
		t.Fatalf("pending task cancel flag=%v err=%v", pendingTask.CancelRequested, err)
	}
	var state, errorCode string
	if err := repo.db.QueryRow(`SELECT state,last_error_code FROM jobs WHERE id=?`, pendingID).Scan(&state, &errorCode); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.JobFailed) || errorCode != "cancelled" {
		t.Fatalf("pending cancel state=%q error_code=%q", state, errorCode)
	}

	runningRequest := inferenceTestRequest(domain.OpSoundEvents, assetID, "", "cancel-running")
	runningID := enqueueInferenceTestTask(t, repo, runningRequest, "task-key-cancel-running")
	leaseInferenceTestJob(t, repo, runningID, "owner-a", time.Now().UTC().Add(time.Minute))
	if err := repo.CancelInferenceTask(ctx, runningID); err != nil {
		t.Fatal(err)
	}
	runningTask, err := repo.GetInferenceTask(ctx, runningID)
	if err != nil || !runningTask.CancelRequested {
		t.Fatalf("running task cancel flag=%v err=%v", runningTask.CancelRequested, err)
	}
	if err := repo.db.QueryRow(`SELECT state FROM jobs WHERE id=?`, runningID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.JobRunning) {
		t.Fatalf("running cancellation changed job state to %q", state)
	}
}

func TestInferenceCommitPublishesRowsAndIsIdempotent(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	space := inferenceTestSpace(t, repo, domain.ModalityAudio)
	now := time.Now().UTC()

	audioRequest := inferenceTestRequest(domain.OpAudioEmbedding, assetID, space.ID, "audio-commit")
	audioJob := enqueueInferenceTestTask(t, repo, audioRequest, "task-key-audio-commit")
	leaseInferenceTestJob(t, repo, audioJob, "owner-a", now.Add(time.Minute))
	if err := repo.StageInferenceTask(context.Background(), audioJob, "owner-a", inferenceTestResult(audioRequest)); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitInferenceTask(context.Background(), audioJob, "owner-a"); err != nil {
		t.Fatal(err)
	}

	eventRequest := inferenceTestRequest(domain.OpSoundEvents, assetID, "", "event-commit")
	eventJob := enqueueInferenceTestTask(t, repo, eventRequest, "task-key-event-commit")
	leaseInferenceTestJob(t, repo, eventJob, "owner-a", now.Add(time.Minute))
	if err := repo.StageInferenceTask(context.Background(), eventJob, "owner-a", inferenceTestResult(eventRequest)); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitInferenceTask(context.Background(), eventJob, "owner-a"); err != nil {
		t.Fatal(err)
	}
	if got := inferenceTestCount(t, repo, "audio_events", "asset_id=?", assetID); got != 1 {
		t.Fatalf("audio events=%d, want 1", got)
	}
	if got := inferenceTestCount(t, repo, "audio_embeddings", "asset_id=?", assetID); got != 1 {
		t.Fatalf("audio embeddings=%d, want 1", got)
	}
	if got := inferenceTestCount(t, repo, "modality_generations", "asset_id=?", assetID); got != 2 {
		t.Fatalf("modality generations=%d, want 2", got)
	}
	var state string
	if err := repo.db.QueryRow(`SELECT state FROM jobs WHERE id=?`, audioJob).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.JobSucceeded) {
		t.Fatalf("audio job state=%q, want succeeded", state)
	}
	active, err := repo.ActiveGeneration(context.Background(), assetID, domain.OpSoundEvents, eventRequest.ConfigHash)
	if err != nil || active.State != domain.GenerationCommitted {
		t.Fatalf("active generation=%+v err=%v", active, err)
	}
	before := inferenceTestCount(t, repo, "modality_generations", "asset_id=?", assetID)
	if err := repo.CommitInferenceTask(context.Background(), audioJob, "owner-a"); err != nil {
		t.Fatalf("idempotent second commit: %v", err)
	}
	if after := inferenceTestCount(t, repo, "modality_generations", "asset_id=?", assetID); after != before {
		t.Fatalf("generation count after repeated commit=%d, was %d", after, before)
	}
}

func TestInferenceCommitRejectsInvalidVectorsWithoutPublishing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vector  []float32
		corrupt bool
	}{
		{name: "wrong dimension", vector: []float32{1}},
		{name: "NaN vector", vector: []float32{float32(math.NaN()), 0}, corrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, assetID := newInferenceRepository(t)
			space := inferenceTestSpace(t, repo, domain.ModalityAudio)
			req := inferenceTestRequest(domain.OpAudioEmbedding, assetID, space.ID, strings.ReplaceAll(tc.name, " ", "-"))
			jobID := enqueueInferenceTestTask(t, repo, req, "task-key-invalid-"+strings.ReplaceAll(tc.name, " ", "-"))
			leaseInferenceTestJob(t, repo, jobID, "owner-a", time.Now().UTC().Add(time.Minute))
			result := inferenceTestResult(req)
			result.Embeddings[0].Vector = tc.vector
			if tc.corrupt {
				// JSON has no NaN token. Put the invalid representation directly
				// into the staged row to exercise Commit's decode/validation path.
				good, err := marshalStoredInferenceResult(inferenceTestResult(req))
				if err != nil {
					t.Fatal(err)
				}
				bad := strings.Replace(good, `"vectors":[[1,0]]`, `"vectors":[[NaN,0]]`, 1)
				if bad == good {
					t.Fatalf("fixture did not contain expected vector JSON: %s", good)
				}
				if _, err := repo.db.Exec(`UPDATE inference_tasks SET staged_json=? WHERE job_id=?`, bad, jobID); err != nil {
					t.Fatal(err)
				}
			} else {
				encoded, err := marshalStoredInferenceResult(result)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := repo.db.Exec(`UPDATE inference_tasks SET staged_json=? WHERE job_id=?`, encoded, jobID); err != nil {
					t.Fatal(err)
				}
			}
			if err := repo.CommitInferenceTask(context.Background(), jobID, "owner-a"); err == nil {
				t.Fatal("invalid staged result committed")
			}
			if got := inferenceTestCount(t, repo, "modality_generations", ""); got != 0 {
				t.Fatalf("generations=%d, want 0", got)
			}
			if got := inferenceTestCount(t, repo, "audio_embeddings", ""); got != 0 {
				t.Fatalf("audio embeddings=%d, want 0", got)
			}
			if got := inferenceTestCount(t, repo, "model_runs", ""); got != 0 {
				t.Fatalf("model runs=%d, want 0", got)
			}
			var state string
			if err := repo.db.QueryRow(`SELECT state FROM jobs WHERE id=?`, jobID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != string(domain.JobRunning) {
				t.Fatalf("job state=%q, want running", state)
			}
		})
	}
}

func TestVisualShotIdentityAndStaleCommitGuard(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	space := inferenceTestSpace(t, repo, domain.ModalityVisual)
	ctx := context.Background()
	if err := repo.ReplaceAssetShots(ctx, assetID, "", []domain.AssetShot{{Ordinal: 0, StartMS: 0, EndMS: 1000, Description: "opening shot"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	original, err := repo.ListAssetShots(ctx, assetID)
	if err != nil || len(original) != 1 {
		t.Fatalf("original shots=%+v err=%v", original, err)
	}
	oldHash := domain.ShotSetHash(original)
	req := inferenceTestRequest(domain.OpVisualEmbedding, assetID, space.ID, "visual-stale")
	req.Visual = &domain.VisualSettings{Strategy: "shot_frame_pooled", ShotSetHash: oldHash, Shots: []domain.VisualShot{{ShotID: original[0].ID, StartMS: 0, EndMS: 1000, TimestampsMS: []int64{500}}}}
	jobID := enqueueInferenceTestTask(t, repo, req, "task-key-visual-stale")
	leaseInferenceTestJob(t, repo, jobID, "owner-a", time.Now().UTC().Add(time.Minute))
	result := inferenceTestResult(req)
	result.Embeddings[0] = domain.EmbeddingRecord{ShotID: original[0].ID, SampleIndex: -1, Vector: []float32{1, 0}}
	if err := repo.StageInferenceTask(ctx, jobID, "owner-a", result); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAssetShots(ctx, assetID, "", []domain.AssetShot{{Ordinal: 0, StartMS: 0, EndMS: 900, Description: "changed boundary"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitInferenceTask(ctx, jobID, "owner-a"); err == nil || !errors.Is(err, domain.ErrPermanentFailure) {
		t.Fatalf("stale shot-set commit error=%v, want permanent failure", err)
	}
	if got := inferenceTestCount(t, repo, "modality_generations", ""); got != 0 {
		t.Fatalf("stale visual commit left %d generations", got)
	}
}

func TestReplaceAssetShotsKeepsIDsForUnchangedIntervals(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	ctx := context.Background()
	shots := []domain.AssetShot{
		{Ordinal: 0, StartMS: 0, EndMS: 1000, Description: "first"},
		{Ordinal: 1, StartMS: 1000, EndMS: 2000, Description: "second"},
	}
	if err := repo.ReplaceAssetShots(ctx, assetID, "", shots, "", ""); err != nil {
		t.Fatal(err)
	}
	duplicate := []domain.AssetShot{
		{Ordinal: 0, StartMS: 0, EndMS: 1000, Description: "duplicate one"},
		{Ordinal: 1, StartMS: 0, EndMS: 1000, Description: "duplicate two"},
	}
	if err := repo.ReplaceAssetShots(ctx, assetID, "", duplicate, "", ""); !errors.Is(err, domain.ErrPermanentFailure) {
		t.Fatalf("duplicate interval error=%v, want permanent failure", err)
	}
	if count := inferenceTestCount(t, repo, "asset_shots", "asset_id=?", assetID); count != len(shots) {
		t.Fatalf("duplicate interval changed canonical shots: %d", count)
	}
	before, err := repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	shots[0].Description = "updated first"
	shots[1].Description = "updated second"
	if err := repo.ReplaceAssetShots(ctx, assetID, "", shots, "", ""); err != nil {
		t.Fatal(err)
	}
	after, err := repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("shot counts before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID {
			t.Fatalf("shot %d ID changed from %q to %q", i, before[i].ID, after[i].ID)
		}
	}
}

func TestActiveGenerationSurvivesUncommittedReplacement(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	ctx := context.Background()
	request := inferenceTestRequest(domain.OpSoundEvents, assetID, "", "head-old")
	jobID := enqueueInferenceTestTask(t, repo, request, "task-key-head-old")
	leaseInferenceTestJob(t, repo, jobID, "owner-a", time.Now().UTC().Add(time.Minute))
	if err := repo.StageInferenceTask(ctx, jobID, "owner-a", inferenceTestResult(request)); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitInferenceTask(ctx, jobID, "owner-a"); err != nil {
		t.Fatal(err)
	}
	before, err := repo.ActiveGeneration(ctx, assetID, domain.OpSoundEvents, request.ConfigHash)
	if err != nil {
		t.Fatal(err)
	}
	replacement := inferenceTestRequest(domain.OpSoundEvents, assetID, "", "head-new")
	replacement.ConfigHash = request.ConfigHash
	replacementJob := enqueueInferenceTestTask(t, repo, replacement, "task-key-head-new")
	leaseInferenceTestJob(t, repo, replacementJob, "owner-b", time.Now().UTC().Add(time.Minute))
	if err := repo.StageInferenceTask(ctx, replacementJob, "owner-b", inferenceTestResult(replacement)); err != nil {
		t.Fatal(err)
	}
	after, err := repo.ActiveGeneration(ctx, assetID, domain.OpSoundEvents, request.ConfigHash)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID {
		t.Fatalf("uncommitted replacement changed head from %q to %q", before.ID, after.ID)
	}
}

func TestAssetlessInferenceLeaseAndFailureAreNullSafe(t *testing.T) {
	repo, _ := newInferenceRepository(t)
	space := inferenceTestSpace(t, repo, domain.ModalityVisual)
	firstReq := inferenceTestRequest(domain.OpVisualEmbedding, "", space.ID, "assetless-worker-a")
	secondReq := inferenceTestRequest(domain.OpVisualEmbedding, "", space.ID, "assetless-worker-b")
	firstID := enqueueInferenceTestTask(t, repo, firstReq, "task-key-assetless-a")
	secondID := enqueueInferenceTestTask(t, repo, secondReq, "task-key-assetless-b")
	now := time.Now().UTC()
	for _, worker := range []string{"worker-a", "worker-b"} {
		nowText := formatTime(now)
		if _, err := repo.db.Exec(`INSERT INTO workers(id,name,platform,token_hash,status,capabilities_json,last_seen_at,created_at) VALUES(?,?,?,?,'online','{}',?,?)`, worker, worker, "linux", "token-"+worker, nowText, nowText); err != nil {
			t.Fatal(err)
		}
	}
	caps := []remote.InferenceCapability{{
		Operation: domain.OpVisualEmbedding, ModelID: "fixture-model",
		ModelRevision: "revision-1", SpaceID: space.ID, Available: true,
	}}
	first, err := repo.LeaseNextWorkerInference(context.Background(), "worker-a", time.Minute, nil, caps, 0, now)
	if err != nil || first == nil {
		t.Fatalf("assetless no-root worker lease=%+v err=%v", first, err)
	}
	if first.AssetID != "" || first.RootID != "" || first.Inference == nil {
		t.Fatalf("assetless lease has asset data: %+v", first)
	}
	second, err := repo.LeaseNextWorkerInference(context.Background(), "worker-b", time.Minute, nil, caps, 0, now.Add(time.Second))
	if err != nil || second == nil {
		t.Fatalf("second assetless worker lease=%+v err=%v", second, err)
	}
	if first.JobID == second.JobID || !((first.JobID == firstID && second.JobID == secondID) || (first.JobID == secondID && second.JobID == firstID)) {
		t.Fatalf("leased jobs are not the two queued jobs: first=%q second=%q", first.JobID, second.JobID)
	}
	if err := repo.CompleteWorkerJob(context.Background(), first.JobID, "worker-a", domain.JobFailed, "fixture failure"); err != nil {
		t.Fatalf("assetless worker failure completion: %v", err)
	}
	var state, owner string
	if err := repo.db.QueryRow(`SELECT state,COALESCE(lease_owner,'') FROM jobs WHERE id=?`, second.JobID).Scan(&state, &owner); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.JobRunning) || owner != "worker-b" {
		t.Fatalf("other worker job mutated by assetless failure: state=%q owner=%q", state, owner)
	}
}

func TestLeaseNextLocalInferenceUsesUsableAssetLocation(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	request := inferenceTestRequest(domain.OpSoundEvents, assetID, "", "local-asset-lease")
	jobID := enqueueInferenceTestTask(t, repo, request, "task-key-local-asset-lease")
	job, err := repo.LeaseNextLocalInference(context.Background(), "hub-local-inference", time.Minute, []remote.InferenceCapability{{
		Operation: domain.OpSoundEvents, ModelID: "fixture-model", ModelRevision: "revision-1", Available: true,
	}}, 0, time.Now().UTC())
	if err != nil || job == nil {
		t.Fatalf("local inference lease=%+v err=%v", job, err)
	}
	if job.JobID != jobID || job.AssetID != assetID || job.RootID != "root-inference" || job.RelativePath != "clip.mp4" || job.Inference == nil {
		t.Fatalf("unexpected local inference lease: %+v", job)
	}
}
func TestEmbeddingSpaceIDMatchesApplicationContract(t *testing.T) {
	repo, _ := newInferenceRepository(t)
	space := domain.EmbeddingSpace{
		Modality:          domain.ModalityVisual,
		Provider:          "provider",
		ModelID:           "model",
		ModelRevision:     "immutable-revision",
		Dimension:         4,
		Normalization:     "l2",
		PreprocessingJSON: `{"resize":224}`,
		RuntimeDigest:     "runtime-digest",
	}
	want, err := domain.CanonicalSpaceID(space)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertEmbeddingSpace(context.Background(), space); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetEmbeddingSpace(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want {
		t.Fatalf("stored canonical space id=%q, want %q", got.ID, want)
	}
}

func TestLeaseNextJobKeepsDetectShotsOnLocalQueue(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	ctx := context.Background()
	if err := repo.EnqueueJob(ctx, assetID, domain.JobDetectShots, "detect-shot-input", 80); err != nil {
		t.Fatal(err)
	}
	space := inferenceTestSpace(t, repo, domain.ModalityVisual)
	request := inferenceTestRequest(domain.OpVisualEmbedding, "", space.ID, "query-lease-exclusion")
	_ = enqueueInferenceTestTask(t, repo, request, "task-key-local-exclusion")
	job, err := repo.LeaseNextJob(ctx, "hub-local", nil, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("local lease=%+v err=%v", job, err)
	}
	if job.Type != domain.JobDetectShots {
		t.Fatalf("LeaseNextJob returned %q, want detect_shots", job.Type)
	}
}

func TestLeaseNextModelManagementTargetsWorker(t *testing.T) {
	repo, _ := newInferenceRepository(t)
	now := time.Now().UTC()
	jobID := "model-management-job"
	if _, err := repo.db.Exec(`INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at) VALUES(?,NULL,'model_install','pending',10,0,3,?,?,?,?)`, jobID, formatTime(now), "management-input", formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`INSERT INTO model_management_tasks(job_id,target_worker_id,catalog_id,manifest_digest,action,license_accepted_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, jobID, "hub-local-owner", "catalog-model", "sha256:manifest", "install", formatTime(now), formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	leased, err := repo.LeaseNextModelManagement(context.Background(), "hub-local-owner", time.Minute, now)
	if err != nil || leased == nil {
		t.Fatalf("model management lease=%+v err=%v", leased, err)
	}
	if leased.JobID != jobID || leased.JobType != domain.JobModelInstall || leased.LeaseDeadline.IsZero() {
		t.Fatalf("unexpected management lease: %+v", leased)
	}
}

func prepareSpeakerEmbeddingCommit(t *testing.T, key string) (*Repository, string, int64, int64, []domain.SpeakerEmbedding) {
	t.Helper()
	repo, assetID := newInferenceRepository(t)
	ctx := context.Background()
	space := inferenceTestSpace(t, repo, domain.ModalitySpeaker)
	consent, err := repo.SetSpeakerConsent(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	profileID := "profile-" + key
	const profileGeneration = int64(7)
	if _, err := repo.db.Exec(`INSERT INTO speaker_profiles(id,generation,state,created_at) VALUES(?,?,'active',?)`, profileID, profileGeneration, formatTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	request := inferenceTestRequest(domain.OpSpeakerEmbedding, assetID, space.ID, key)
	request.Speaker = &domain.SpeakerSettings{ProfileID: profileID, Interval: &domain.TimeInterval{StartMS: 100, EndMS: 1000}}
	jobID := enqueueInferenceTestTask(t, repo, request, "speaker-task-"+key)
	leaseInferenceTestJob(t, repo, jobID, "speaker-owner", time.Now().UTC().Add(time.Minute))
	records := []domain.SpeakerEmbedding{{
		ProfileID:         profileID,
		AssetID:           assetID,
		GenerationID:      "diarization-generation-" + key,
		SpaceID:           space.ID,
		StartMS:           200,
		EndMS:             800,
		SpeechDurationMS:  600,
		QualityJSON:       `{}`,
		SecretRef:         "biometric-secret-" + key,
		ConsentGeneration: consent.Generation,
		ProfileGeneration: profileGeneration,
	}}
	return repo, jobID, consent.Generation, profileGeneration, records
}

func TestCommitSpeakerEmbeddingFencesConsentProfileAndLease(t *testing.T) {
	t.Run("stale consent", func(t *testing.T) {
		repo, jobID, consentGeneration, profileGeneration, records := prepareSpeakerEmbeddingCommit(t, "stale-consent")
		err := repo.CommitSpeakerEmbedding(context.Background(), jobID, "speaker-owner", consentGeneration-1, profileGeneration, records)
		if !errors.Is(err, domain.ErrPermanentFailure) {
			t.Fatalf("stale consent error=%v, want permanent failure", err)
		}
		if got := inferenceTestCount(t, repo, "speaker_embeddings", ""); got != 0 {
			t.Fatalf("stale consent published %d rows", got)
		}
		var state string
		if err := repo.db.QueryRow(`SELECT state FROM jobs WHERE id=?`, jobID).Scan(&state); err != nil || state != string(domain.JobRunning) {
			t.Fatalf("stale consent job state=%q err=%v, want running", state, err)
		}
	})
	t.Run("valid encrypted references", func(t *testing.T) {
		repo, jobID, consentGeneration, profileGeneration, records := prepareSpeakerEmbeddingCommit(t, "valid")
		if err := repo.CommitSpeakerEmbedding(context.Background(), jobID, "speaker-owner", consentGeneration, profileGeneration, records); err != nil {
			t.Fatal(err)
		}
		if got := inferenceTestCount(t, repo, "speaker_embeddings", ""); got != len(records) {
			t.Fatalf("speaker embeddings=%d, want %d", got, len(records))
		}
		if got := inferenceTestCount(t, repo, "modality_generations", ""); got != 0 {
			t.Fatalf("speaker commit created %d modality generations", got)
		}
		var state string
		if err := repo.db.QueryRow(`SELECT state FROM jobs WHERE id=?`, jobID).Scan(&state); err != nil || state != string(domain.JobSucceeded) {
			t.Fatalf("valid speaker commit job state=%q err=%v, want succeeded", state, err)
		}
	})
	t.Run("lost lease", func(t *testing.T) {
		repo, jobID, consentGeneration, profileGeneration, records := prepareSpeakerEmbeddingCommit(t, "lost-lease")
		if _, err := repo.db.Exec(`UPDATE jobs SET lease_expires_at=? WHERE id=?`, formatTime(time.Now().UTC().Add(-time.Second)), jobID); err != nil {
			t.Fatal(err)
		}
		err := repo.CommitSpeakerEmbedding(context.Background(), jobID, "speaker-owner", consentGeneration, profileGeneration, records)
		if !errors.Is(err, domain.ErrJobLeaseLost) {
			t.Fatalf("lost-lease speaker commit error=%v, want ErrJobLeaseLost", err)
		}
		if got := inferenceTestCount(t, repo, "speaker_embeddings", ""); got != 0 {
			t.Fatalf("lost lease published %d speaker embedding rows", got)
		}
	})
}

func TestCancelledInferenceCannotStageOrCommit(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	req := inferenceTestRequest(domain.OpSoundEvents, assetID, "", "cancelled-fence")
	jobID := enqueueInferenceTestTask(t, repo, req, "cancelled-fence")
	leaseInferenceTestJob(t, repo, jobID, "owner", time.Now().UTC().Add(time.Minute))
	if err := repo.CancelInferenceTask(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	err := repo.StageInferenceTask(context.Background(), jobID, "owner", inferenceTestResult(req))
	if !errors.Is(err, domain.ErrInferenceCancelled) || !errors.Is(err, domain.ErrPermanentFailure) {
		t.Fatalf("stage after cancel = %v, want permanent ErrInferenceCancelled", err)
	}
	var staged int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM inference_tasks WHERE job_id=? AND staged_json IS NOT NULL`, jobID).Scan(&staged); err != nil {
		t.Fatal(err)
	}
	if staged != 0 {
		t.Fatal("cancelled task staged a result")
	}
	var state string
	if err := repo.db.QueryRow(`SELECT state FROM jobs WHERE id=?`, jobID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.JobRunning) {
		t.Fatalf("cancel of a running task must leave the job for its owner to stop, state=%q", state)
	}
}

func TestStagedInferenceResultKeepsVectorsForRetryDigest(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	space := inferenceTestSpace(t, repo, domain.ModalityAudio)
	req := inferenceTestRequest(domain.OpAudioEmbedding, assetID, space.ID, "staged-vectors")
	jobID := enqueueInferenceTestTask(t, repo, req, "staged-vectors")
	leaseInferenceTestJob(t, repo, jobID, "owner", time.Now().UTC().Add(time.Minute))
	result := inferenceTestResult(req)
	if err := repo.StageInferenceTask(context.Background(), jobID, "owner", result); err != nil {
		t.Fatal(err)
	}
	staged, ok, err := repo.GetStagedInferenceResult(context.Background(), jobID)
	if err != nil || !ok {
		t.Fatalf("GetStagedInferenceResult ok=%v err=%v", ok, err)
	}
	if len(staged.Embeddings) != 1 || len(staged.Embeddings[0].Vector) != 2 || staged.Embeddings[0].Vector[0] != 1 {
		t.Fatalf("staged vectors were dropped: %+v", staged.Embeddings)
	}
}

func TestInferenceLeaseRequiresExactModelAndProbeAttestation(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	request := inferenceTestRequest(domain.OpSoundEvents, assetID, "", "routing")
	jobID := enqueueInferenceTestTask(t, repo, request, "task-key-routing")
	now := time.Now().UTC()
	if _, err := repo.db.Exec(`INSERT INTO workers(id,name,platform,token_hash,status,capabilities_json,last_seen_at,created_at) VALUES('worker-routing','w','linux','token','online','{}',?,?)`, formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	base := remote.InferenceCapability{
		Operation: domain.OpSoundEvents, ModelID: "fixture-model",
		ModelRevision: "revision-1", Available: true,
		CatalogID: "sound-yamnet",
	}
	wrong := base
	wrong.ModelID = "other-model"
	job, err := repo.LeaseNextWorkerInference(context.Background(), "worker-routing", time.Minute, []string{"root-inference"}, []remote.InferenceCapability{wrong}, 0, now)
	if err != nil || job != nil {
		t.Fatalf("wrong-model capability leased job=%+v err=%v", job, err)
	}
	job, err = repo.LeaseNextWorkerInference(context.Background(), "worker-routing", time.Minute, []string{"root-inference"}, []remote.InferenceCapability{base}, 0, now)
	if err != nil || job != nil {
		t.Fatalf("unattested capability leased job=%+v err=%v", job, err)
	}
	if err := repo.RecordModelAttestation(context.Background(), "worker-routing", base.CatalogID, "sha256:manifest", "probe", "{}"); err != nil {
		t.Fatal(err)
	}
	job, err = repo.LeaseNextWorkerInference(context.Background(), "worker-routing", time.Minute, []string{"root-inference"}, []remote.InferenceCapability{base}, 0, now)
	if err != nil || job == nil || job.JobID != jobID {
		t.Fatalf("attested capability lease=%+v err=%v", job, err)
	}
}

func TestLocalInferenceLeaseDoesNotRequireWorkerAttestation(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	request := inferenceTestRequest(domain.OpSoundEvents, assetID, "", "local-routing")
	jobID := enqueueInferenceTestTask(t, repo, request, "task-key-local-routing")
	job, err := repo.LeaseNextLocalInference(context.Background(), "hub-local", time.Minute, []remote.InferenceCapability{{
		Operation: domain.OpSoundEvents, ModelID: "fixture-model",
		ModelRevision: "revision-1", Available: true,
		CatalogID: "sound-yamnet",
	}}, 0, time.Now().UTC())
	if err != nil || job == nil || job.JobID != jobID {
		t.Fatalf("local lease=%+v err=%v", job, err)
	}
}
