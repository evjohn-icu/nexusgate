package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// TestCommitAnalysisRejectsFailedRun verifies that CommitAnalysisWithShots
// refuses a run in 'failed' state, leaves canonical tables untouched, and
// wraps the refusal in both ErrCommitRunNotValidated and ErrPermanentFailure.
func TestCommitAnalysisRejectsFailedRun(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)

	runID, _, err := repo.CreateModelRun(ctx, assetID, "video_analysis", "test-provider", "test-model", "hash-guard-failed", "v1", "v1", `{}`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.FailModelRun(ctx, runID, "MODEL_ERROR", "test failure", `{}`, "", ""); err != nil {
		t.Fatal(err)
	}

	err = repo.CommitAnalysisWithShots(ctx, assetID, runID, "v1", domain.StructuredAnalysis{AssetType: "b_roll", Summary: "must not write"}, nil, "", "")
	if err == nil {
		t.Fatal("CommitAnalysisWithShots with a failed run must return an error")
	}
	if !errors.Is(err, domain.ErrCommitRunNotValidated) {
		t.Fatalf("error must wrap ErrCommitRunNotValidated, got: %v", err)
	}
	if !errors.Is(err, domain.ErrPermanentFailure) {
		t.Fatalf("error must wrap ErrPermanentFailure, got: %v", err)
	}

	// Canonical tables must be untouched.
	var analysisCount int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_analysis WHERE asset_id=?`, assetID).Scan(&analysisCount); err != nil {
		t.Fatal(err)
	}
	if analysisCount != 0 {
		t.Fatalf("asset_analysis must be empty after rejected commit, got %d rows", analysisCount)
	}

	var shotCount int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_shots WHERE asset_id=?`, assetID).Scan(&shotCount); err != nil {
		t.Fatal(err)
	}
	if shotCount != 0 {
		t.Fatalf("asset_shots must be empty after rejected commit, got %d rows", shotCount)
	}

	// Run state must remain 'failed'.
	var state string
	if err := repo.db.QueryRowContext(ctx, `SELECT state FROM model_runs WHERE id=?`, runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("run state must remain 'failed', got %q", state)
	}
}

// TestCommitAnalysisRejectsCommittedRun verifies that a second commit of the
// same run is refused and does not overwrite the first commit's canonical data.
func TestCommitAnalysisRejectsCommittedRun(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)

	// First commit — must succeed.
	runID, _, err := repo.CreateModelRun(ctx, assetID, "video_analysis", "test-provider", "test-model", "hash-guard-committed", "v1", "v1", `{}`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, runID, `{"response":"ok"}`, `{"asset_type":"b_roll","summary":"first commit"}`, "", ""); err != nil {
		t.Fatal(err)
	}
	firstAnalysis := domain.StructuredAnalysis{AssetType: "b_roll", Summary: "first commit", ShotSize: "wide"}
	if err := repo.CommitAnalysisWithShots(ctx, assetID, runID, "v1", firstAnalysis, nil, "", ""); err != nil {
		t.Fatalf("first commit must succeed: %v", err)
	}

	// Second commit with same run — must fail.
	secondAnalysis := domain.StructuredAnalysis{AssetType: "talking_to_camera", Summary: "second commit overwrite attempt"}
	err = repo.CommitAnalysisWithShots(ctx, assetID, runID, "v1", secondAnalysis, nil, "", "")
	if err == nil {
		t.Fatal("second CommitAnalysisWithShots with a committed run must return an error")
	}
	if !errors.Is(err, domain.ErrCommitRunNotValidated) {
		t.Fatalf("error must wrap ErrCommitRunNotValidated, got: %v", err)
	}

	// asset_analysis must still hold the first commit's data.
	var summary string
	if err := repo.db.QueryRowContext(ctx, `SELECT summary FROM asset_analysis WHERE asset_id=?`, assetID).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if summary != "first commit" {
		t.Fatalf("asset_analysis must keep first commit summary, got %q", summary)
	}

	// Run state must remain 'committed'.
	var state string
	if err := repo.db.QueryRowContext(ctx, `SELECT state FROM model_runs WHERE id=?`, runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "committed" {
		t.Fatalf("run state must remain 'committed', got %q", state)
	}
}

// TestCommitAnalysisRejectsNonexistentRun verifies that a commit with a
// run ID that does not exist is refused and writes nothing to canonical tables.
func TestCommitAnalysisRejectsNonexistentRun(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)

	err := repo.CommitAnalysis(ctx, assetID, "nonexistent-run-id", "v1", domain.StructuredAnalysis{AssetType: "b_roll", Summary: "ghost"}, "", "")
	if err == nil {
		t.Fatal("CommitAnalysis with a nonexistent run must return an error")
	}
	if !errors.Is(err, domain.ErrCommitRunNotValidated) {
		t.Fatalf("error must wrap ErrCommitRunNotValidated, got: %v", err)
	}
	if !errors.Is(err, domain.ErrPermanentFailure) {
		t.Fatalf("error must wrap ErrPermanentFailure, got: %v", err)
	}

	// Canonical tables must be empty.
	var analysisCount int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_analysis WHERE asset_id=?`, assetID).Scan(&analysisCount); err != nil {
		t.Fatal(err)
	}
	if analysisCount != 0 {
		t.Fatalf("asset_analysis must be empty after rejected commit, got %d rows", analysisCount)
	}
}

// TestCommitAnalysisSucceedsForValidatedRun verifies the happy path:
// StageModelRun followed by CommitAnalysisWithShots writes canonical data
// and transitions the run to 'committed'.
func TestCommitAnalysisSucceedsForValidatedRun(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)

	runID, _, err := repo.CreateModelRun(ctx, assetID, "video_analysis", "test-provider", "test-model", "hash-guard-validated", "v1", "v1", `{}`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, runID, `{"response":"ok"}`, `{"asset_type":"b_roll","summary":"validated commit","shot_size":"wide"}`, "", ""); err != nil {
		t.Fatal(err)
	}

	analysis := domain.StructuredAnalysis{AssetType: "b_roll", Summary: "validated commit", ShotSize: "wide"}
	shots := []domain.AssetShot{
		{StartMS: 0, EndMS: 3000, Ordinal: 0, Description: "opening shot"},
	}
	if err := repo.CommitAnalysisWithShots(ctx, assetID, runID, "v1", analysis, shots, "", ""); err != nil {
		t.Fatalf("CommitAnalysisWithShots with a validated run must succeed: %v", err)
	}

	// Run state must be 'committed'.
	var state string
	if err := repo.db.QueryRowContext(ctx, `SELECT state FROM model_runs WHERE id=?`, runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "committed" {
		t.Fatalf("run state must be 'committed', got %q", state)
	}

	// asset_analysis must have data.
	var summary string
	if err := repo.db.QueryRowContext(ctx, `SELECT summary FROM asset_analysis WHERE asset_id=?`, assetID).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if summary != "validated commit" {
		t.Fatalf("asset_analysis summary mismatch, got %q", summary)
	}

	// asset_shots must have the shot.
	var shotCount int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_shots WHERE asset_id=?`, assetID).Scan(&shotCount); err != nil {
		t.Fatal(err)
	}
	if shotCount != 1 {
		t.Fatalf("expected 1 asset_shot, got %d", shotCount)
	}
}
