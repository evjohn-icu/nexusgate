package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
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
