package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

func leaseBoundaryJob(t *testing.T, repo *Repository, owner string) string {
	t.Helper()
	now := time.Now().UTC()
	jobID := "lease-boundary-job"
	_, err := repo.db.ExecContext(context.Background(), `INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,lease_owner,lease_expires_at,created_at,updated_at) VALUES(?,?,?,'running',1,1,3,?,?,?,?,?,?)`, jobID, "asset-1", "analyze", now, "input", owner, formatTime(now.Add(time.Hour)), formatTime(now), formatTime(now))
	if err != nil {
		t.Fatal(err)
	}
	return jobID
}

func reclaimLease(t *testing.T, repo *Repository, jobID, owner string) {
	t.Helper()
	if _, err := repo.db.ExecContext(context.Background(), `UPDATE jobs SET lease_owner=?,lease_expires_at=? WHERE id=?`, owner, formatTime(time.Now().UTC().Add(time.Hour)), jobID); err != nil {
		t.Fatal(err)
	}
}

func TestStaleLeaseCanonicalBoundariesRollbackAndOwnerBSucceeds(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)
	jobID := leaseBoundaryJob(t, repo, "owner-a")
	runID, _, err := repo.CreateModelRun(ctx, assetID, "vision", "fixture", "model", "lease-boundary", "prompt", "schema", "{}", jobID, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, runID, "raw-a", "{}", jobID, "owner-a"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTranscript(ctx, assetID, "fixture", "model", "transcript", domain.Transcript{Text: "before"}, jobID, "owner-a"); err != nil {
		t.Fatal(err)
	}
	reclaimLease(t, repo, jobID, "owner-b")

	if err := repo.SaveTranscript(ctx, assetID, "fixture", "model", "transcript", domain.Transcript{Text: "stale"}, jobID, "owner-a"); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("SaveTranscript error=%v", err)
	}
	if err := repo.CommitAnalysisWithShots(ctx, assetID, runID, "schema", domain.StructuredAnalysis{Summary: "stale"}, []domain.AssetShot{{StartMS: 0, EndMS: 1000, Description: "stale"}}, jobID, "owner-a"); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("CommitAnalysisWithShots error=%v", err)
	}
	var state, summary string
	if err := repo.db.QueryRowContext(ctx, `SELECT state FROM model_runs WHERE id=?`, runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "validated" {
		t.Fatalf("run state=%q, want validated", state)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_analysis WHERE asset_id=?`, assetID).Scan(new(int)); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT full_text FROM transcripts WHERE asset_id=?`, assetID).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if summary != "before" {
		t.Fatalf("transcript=%q, want before", summary)
	}

	if err := repo.CommitAnalysisWithShots(ctx, assetID, runID, "schema", domain.StructuredAnalysis{Summary: "owner b"}, []domain.AssetShot{{StartMS: 0, EndMS: 1000, Description: "owner b"}}, jobID, "owner-b"); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT state FROM model_runs WHERE id=?`, runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "committed" {
		t.Fatalf("owner B state=%q", state)
	}
}

func TestStaleLeaseCommitShotRefinementRollsBack(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)
	jobID := leaseBoundaryJob(t, repo, "owner-a")
	runID, _, err := repo.CreateModelRun(ctx, assetID, "vision", "fixture", "model", "refine-boundary", "prompt", "schema", "{}", jobID, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, runID, "raw", "{}", jobID, "owner-a"); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAssetShots(ctx, assetID, "", []domain.AssetShot{{StartMS: 0, EndMS: 1000, Description: "trusted"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	before, err := repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	reclaimLease(t, repo, jobID, "owner-b")
	if err := repo.CommitShotRefinement(ctx, assetID, runID, []domain.AssetShot{{StartMS: 0, EndMS: 1000, Description: "stale"}}, jobID, "owner-a"); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("error=%v", err)
	}
	after, err := repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Description != before[0].Description {
		t.Fatalf("stale refinement changed shots: before=%+v after=%+v", before, after)
	}
	if err := repo.CommitShotRefinement(ctx, assetID, runID, []domain.AssetShot{{StartMS: 0, EndMS: 1000, Description: "owner b"}}, jobID, "owner-b"); err != nil {
		t.Fatal(err)
	}
	after, err = repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Description != "owner b" {
		t.Fatalf("owner B shots=%+v", after)
	}
}

func TestStaleLeaseModelRunLifecycleWritesRejected(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)
	jobID := leaseBoundaryJob(t, repo, "owner-a")
	runID, _, err := repo.CreateModelRun(ctx, assetID, "vision", "fixture", "model", "lifecycle-boundary", "prompt", "schema", "{}", jobID, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	reclaimLease(t, repo, jobID, "owner-b")
	if err := repo.StageModelRun(ctx, runID, "stale", "{}", jobID, "owner-a"); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("StageModelRun error=%v", err)
	}
	if err := repo.FailModelRun(ctx, runID, "stale", "stale", "stale", jobID, "owner-a"); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("FailModelRun error=%v", err)
	}
	if err := repo.MarkModelRunCommitted(ctx, runID, jobID, "owner-a"); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("MarkModelRunCommitted error=%v", err)
	}
	var state string
	if err := repo.db.QueryRowContext(ctx, `SELECT state FROM model_runs WHERE id=?`, runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Fatalf("stale lifecycle changed state to %q", state)
	}
}

func TestStaleLeaseCreateModelRunRejected(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)
	jobID := leaseBoundaryJob(t, repo, "owner-a")
	reclaimLease(t, repo, jobID, "owner-b")
	if _, _, err := repo.CreateModelRun(ctx, assetID, "vision", "fixture", "model", "stale-create", "prompt", "schema", "{}", jobID, "owner-a"); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("CreateModelRun error=%v", err)
	}
	var count int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_runs WHERE input_hash='stale-create'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale CreateModelRun inserted %d rows", count)
	}
}

// TestStaleLeaseProbeWritesRejected covers the three probe-stage writes that
// used to take no lease at all: SaveMediaMetadata, SaveSpeechClassification
// and SaveAlignment. Their rows are canonical (media_metadata,
// capture_metadata, speech_classifications, alignment_runs, transcript_words)
// and CompleteJob's CAS did not protect them, because they land before it.
// The asserted property is the one the other eight writes already have: a
// holder whose lease was reclaimed gets ErrJobLeaseLost and leaves owner-b's
// row exactly as it was, rather than silently overwriting it.
//
// This lives in the sqlite package deliberately. internal/app's fakes enforce
// no lease and would pass whatever the SQL says.
func TestStaleLeaseProbeWritesRejected(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)
	jobID := leaseBoundaryJob(t, repo, "owner-a")

	// owner-a holds the lease: all three writes land.
	if err := repo.SaveMediaMetadata(ctx, assetID, domain.MediaMetadata{DurationMS: 1000, CameraModel: "owner-a"}, "probe-v1", jobID, "owner-a"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveSpeechClassification(ctx, assetID, domain.SpeechClassification{Classification: "speech", SpeechProbability: 0.9, Reason: "owner-a"}, jobID, "owner-a"); err != nil {
		t.Fatal(err)
	}
	confidence := 0.9
	if err := repo.SaveAlignment(ctx, assetID, "fixture", "model", "align-a", "{}", domain.AlignmentResult{Words: []domain.AlignmentWord{{StartMS: 0, EndMS: 100, Text: "owner-a", Confidence: &confidence}}}, jobID, "owner-a"); err != nil {
		t.Fatal(err)
	}

	reclaimLease(t, repo, jobID, "owner-b")

	if err := repo.SaveMediaMetadata(ctx, assetID, domain.MediaMetadata{DurationMS: 9999, CameraModel: "stale"}, "probe-v1", jobID, "owner-a"); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("SaveMediaMetadata error=%v, want ErrJobLeaseLost", err)
	}
	if err := repo.SaveSpeechClassification(ctx, assetID, domain.SpeechClassification{Classification: "no_speech", SpeechProbability: 0.01, Reason: "stale"}, jobID, "owner-a"); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("SaveSpeechClassification error=%v, want ErrJobLeaseLost", err)
	}
	if err := repo.SaveAlignment(ctx, assetID, "fixture", "model", "align-stale", "{}", domain.AlignmentResult{Words: []domain.AlignmentWord{{StartMS: 0, EndMS: 100, Text: "stale"}}}, jobID, "owner-a"); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("SaveAlignment error=%v, want ErrJobLeaseLost", err)
	}

	// Nothing the stale holder tried may be observable.
	var cameraModel string
	if err := repo.db.QueryRowContext(ctx, `SELECT camera_model FROM media_metadata WHERE asset_id=?`, assetID).Scan(&cameraModel); err != nil {
		t.Fatal(err)
	}
	if cameraModel != "owner-a" {
		t.Fatalf("media_metadata.camera_model=%q, want owner-a", cameraModel)
	}
	var reason string
	if err := repo.db.QueryRowContext(ctx, `SELECT reason FROM speech_classifications WHERE asset_id=?`, assetID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "owner-a" {
		t.Fatalf("speech_classifications.reason=%q, want owner-a", reason)
	}
	var alignmentRuns int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alignment_runs WHERE asset_id=? AND input_hash='align-stale'`, assetID).Scan(&alignmentRuns); err != nil {
		t.Fatal(err)
	}
	if alignmentRuns != 0 {
		t.Fatalf("stale SaveAlignment inserted %d alignment_runs rows", alignmentRuns)
	}
	// transcript_words is the one that reaches the evidence gate: the stale
	// holder must not have replaced owner-a's words with its own.
	var word string
	if err := repo.db.QueryRowContext(ctx, `SELECT text FROM transcript_words WHERE asset_id=? ORDER BY ordinal LIMIT 1`, assetID).Scan(&word); err != nil {
		t.Fatal(err)
	}
	if word != "owner-a" {
		t.Fatalf("transcript_words.text=%q, want owner-a", word)
	}
}

// TestProbeWritesWithoutLeaseStillWork pins the other half of the contract:
// an empty jobID/owner means "no lease to check", which is what the CLI and
// every fixture rely on. Adding the guard must not make those callers fail.
func TestProbeWritesWithoutLeaseStillWork(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)
	if err := repo.SaveMediaMetadata(ctx, assetID, domain.MediaMetadata{DurationMS: 1000}, "probe-v1", "", ""); err != nil {
		t.Fatalf("SaveMediaMetadata without a lease: %v", err)
	}
	if err := repo.SaveSpeechClassification(ctx, assetID, domain.SpeechClassification{Classification: "speech", SpeechProbability: 0.9}, "", ""); err != nil {
		t.Fatalf("SaveSpeechClassification without a lease: %v", err)
	}
	if err := repo.SaveAlignment(ctx, assetID, "fixture", "model", "align-none", "{}", domain.AlignmentResult{Words: []domain.AlignmentWord{{StartMS: 0, EndMS: 100, Text: "hi"}}}, "", ""); err != nil {
		t.Fatalf("SaveAlignment without a lease: %v", err)
	}
}
