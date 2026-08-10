package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// setupModelRunsTest creates a fresh repository with a library root, asset,
// and asset location, returning the repo and the asset ID.
func setupModelRunsTest(t *testing.T) (*Repository, string) {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-1','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-1','fp',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc-1','asset-1','root-1','night.mp4','/footage/night.mp4',1,1,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	return repo, "asset-1"
}

// TestFailModelRunStateIsFailed verifies that after FailModelRun the
// model_runs row carries state='failed', raw_response, error_code,
// error_message, and finished_at.
func TestFailModelRunStateIsFailed(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)

	runID, alreadyCommitted, err := repo.CreateModelRun(ctx, assetID, "video_analysis", "test-provider", "test-model", "hash-fail-1", "v1", "v1", `{"test":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if alreadyCommitted {
		t.Fatal("fresh run must not be already committed")
	}

	// Verify state is 'running' after creation.
	var state string
	if err := repo.db.QueryRowContext(ctx, `SELECT state FROM model_runs WHERE id=?`, runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Fatalf("expected state='running', got %q", state)
	}

	// Fail the run.
	raw := `{"error":"upstream timeout"}`
	if err := repo.FailModelRun(ctx, runID, "PROVIDER_TIMEOUT", "request timed out after 30s", raw); err != nil {
		t.Fatal(err)
	}

	// Verify failure state and fields.
	var (
		failedState    string
		failedRaw      *string
		failedCode     *string
		failedMessage  *string
		failedFinished *string
	)
	if err := repo.db.QueryRowContext(ctx, `SELECT state, raw_response, error_code, error_message, finished_at FROM model_runs WHERE id=?`, runID).Scan(&failedState, &failedRaw, &failedCode, &failedMessage, &failedFinished); err != nil {
		t.Fatal(err)
	}
	if failedState != "failed" {
		t.Fatalf("expected state='failed', got %q", failedState)
	}
	if failedRaw == nil || *failedRaw != raw {
		t.Fatalf("expected raw_response=%q, got %v", raw, failedRaw)
	}
	if failedCode == nil || *failedCode != "PROVIDER_TIMEOUT" {
		t.Fatalf("expected error_code='PROVIDER_TIMEOUT', got %v", failedCode)
	}
	if failedMessage == nil || *failedMessage != "request timed out after 30s" {
		t.Fatalf("expected error_message='request timed out after 30s', got %v", failedMessage)
	}
	if failedFinished == nil {
		t.Fatal("expected finished_at to be set")
	}
}

// TestFailModelRunDoesNotLeakToCanonicalTables verifies that after FailModelRun
// the canonical tables (asset_analysis, asset_shots, asset_tag_links) remain
// untouched for the asset.
func TestFailModelRunDoesNotLeakToCanonicalTables(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)

	runID, _, err := repo.CreateModelRun(ctx, assetID, "video_analysis", "test-provider", "test-model", "hash-fail-2", "v1", "v1", `{"test":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.FailModelRun(ctx, runID, "MODEL_ERROR", "invalid json in response", `{"raw":"bad"}`); err != nil {
		t.Fatal(err)
	}

	// asset_analysis must not have a row for this asset.
	var analysisCount int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_analysis WHERE asset_id=?`, assetID).Scan(&analysisCount); err != nil {
		t.Fatal(err)
	}
	if analysisCount != 0 {
		t.Fatalf("asset_analysis must be empty after FailModelRun, got %d rows", analysisCount)
	}

	// asset_shots must not have rows for this asset.
	var shotCount int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_shots WHERE asset_id=?`, assetID).Scan(&shotCount); err != nil {
		t.Fatal(err)
	}
	if shotCount != 0 {
		t.Fatalf("asset_shots must be empty after FailModelRun, got %d rows", shotCount)
	}

	// asset_tag_links must not have AI-sourced rows for this asset.
	var tagCount int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_tag_links WHERE asset_id=? AND source='ai'`, assetID).Scan(&tagCount); err != nil {
		t.Fatal(err)
	}
	if tagCount != 0 {
		t.Fatalf("asset_tag_links must be empty after FailModelRun, got %d rows", tagCount)
	}

	// Canonical tags must be empty (nothing was ever committed).
	tags, err := repo.ListCanonicalTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 0 {
		t.Fatalf("canonical tags must be empty after FailModelRun, got %d", len(tags))
	}
}

// TestCommitAnalysisWithShotsSucceedsAfterFailedRun verifies that after one
// model run fails for an asset, a second run (with a different input hash)
// can still go through StageModelRun → CommitAnalysisWithShots and produce
// canonical data.
func TestCommitAnalysisWithShotsSucceedsAfterFailedRun(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)

	// Run 1: fail.
	run1ID, _, err := repo.CreateModelRun(ctx, assetID, "video_analysis", "test-provider", "test-model", "hash-run-1", "v1", "v1", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.FailModelRun(ctx, run1ID, "TRANSIENT", "first attempt failed", `{}`); err != nil {
		t.Fatal(err)
	}

	// Run 2: success path (different input hash so it does not collide).
	run2ID, alreadyCommitted, err := repo.CreateModelRun(ctx, assetID, "video_analysis", "test-provider", "test-model", "hash-run-2", "v1", "v1", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if alreadyCommitted {
		t.Fatal("fresh run must not be already committed")
	}

	analysis := domain.StructuredAnalysis{
		AssetType: "b-roll",
		SceneTags: []string{"forest", "daylight"},
		ShotSize:  "wide",
	}
	parsed := `{"asset_type":"b-roll","scene_tags":["forest","daylight"],"shot_size":"wide"}`
	if err := repo.StageModelRun(ctx, run2ID, `{"response":"ok"}`, parsed); err != nil {
		t.Fatal(err)
	}

	shots := []domain.AssetShot{
		{StartMS: 0, EndMS: 3000, Ordinal: 0, Description: "wide forest pan"},
		{StartMS: 3000, EndMS: 6000, Ordinal: 1, Description: "sunlight through trees"},
	}
	if err := repo.CommitAnalysisWithShots(ctx, assetID, run2ID, "v1", analysis, shots); err != nil {
		t.Fatalf("CommitAnalysisWithShots after a failed run must succeed: %v", err)
	}

	// Verify run states.
	var run1State, run2State string
	if err := repo.db.QueryRowContext(ctx, `SELECT state FROM model_runs WHERE id=?`, run1ID).Scan(&run1State); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT state FROM model_runs WHERE id=?`, run2ID).Scan(&run2State); err != nil {
		t.Fatal(err)
	}
	if run1State != "failed" {
		t.Fatalf("run 1 state must remain 'failed', got %q", run1State)
	}
	if run2State != "committed" {
		t.Fatalf("run 2 state must be 'committed', got %q", run2State)
	}

	// asset_analysis must have data.
	var analysisCount int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_analysis WHERE asset_id=?`, assetID).Scan(&analysisCount); err != nil {
		t.Fatal(err)
	}
	if analysisCount != 1 {
		t.Fatalf("expected 1 asset_analysis row after successful commit, got %d", analysisCount)
	}

	// asset_shots must have data (2 shots).
	assetShots, err := repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(assetShots) != 2 {
		t.Fatalf("expected 2 asset_shots after successful commit, got %d: %+v", len(assetShots), assetShots)
	}

	// FTS5 search must find the asset after rebuild.
	if err := repo.RebuildSearch(ctx, assetID); err != nil {
		t.Fatal(err)
	}
	ids, err := repo.Search(ctx, "forest", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != assetID {
		t.Fatalf("search for 'forest' must return asset-1 after commit, got %v", ids)
	}
}

// TestCreateModelRunRetryAfterFailureAllocatesNewRun verifies that a failed
// attempt does not occupy the dedup key for a later retry.
func TestCreateModelRunRetryAfterFailureAllocatesNewRun(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)

	// First call creates a run.
	run1ID, alreadyCommitted, err := repo.CreateModelRun(ctx, assetID, "video_analysis", "test-provider", "test-model", "hash-dedup", "v1", "v1", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if alreadyCommitted {
		t.Fatal("first create must not be already committed")
	}
	// Fail it.
	if err := repo.FailModelRun(ctx, run1ID, "ERROR", "fail", `{}`); err != nil {
		t.Fatal(err)
	}

	// A failed attempt is retryable, so the same key allocates a new run.
	run2ID, alreadyCommitted, err := repo.CreateModelRun(ctx, assetID, "video_analysis", "test-provider", "test-model", "hash-dedup", "v1", "v1", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if alreadyCommitted {
		t.Fatal("a failed run must not be reported as already committed")
	}
	if run2ID == run1ID {
		t.Fatalf("retry after failure must allocate a new run ID, got %s", run2ID)
	}
}

func TestModelRunTransitionsAreStrictAndPreservePayload(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)
	runID, _, err := repo.CreateModelRun(ctx, assetID, "vision", "provider", "model", "matrix", "p1", "s1", `{"request":"original"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, runID, `{"raw":"stage"}`, `{"parsed":true}`); err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, runID, `{"raw":"overwrite"}`, `{"parsed":false}`); err == nil || !errors.Is(err, domain.ErrModelRunNotRunning) || !errors.Is(err, domain.ErrPermanentFailure) {
		t.Fatalf("double stage error = %v", err)
	}
	if err := repo.FailModelRun(ctx, runID, "late", "late", `{"raw":"late"}`); err == nil || !errors.Is(err, domain.ErrModelRunNotRunning) {
		t.Fatalf("late fail error = %v", err)
	}
	var raw, parsed, state string
	if err := repo.db.QueryRowContext(ctx, `SELECT state,raw_response,parsed_json FROM model_runs WHERE id=?`, runID).Scan(&state, &raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if state != "validated" || raw != `{"raw":"stage"}` || parsed != `{"parsed":true}` {
		t.Fatalf("payload changed: state=%s raw=%s parsed=%s", state, raw, parsed)
	}
	if err := repo.MarkModelRunCommitted(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkModelRunCommitted(ctx, runID); err == nil || !errors.Is(err, domain.ErrCommitRunNotValidated) || !errors.Is(err, domain.ErrPermanentFailure) {
		t.Fatalf("double commit error = %v", err)
	}
}

func TestModelRunStageAndFailConcurrentOneWinner(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)
	runID, _, err := repo.CreateModelRun(ctx, assetID, "vision", "provider", "model", "race", "p1", "s1", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); results <- repo.StageModelRun(ctx, runID, `{"winner":"stage"}`, `{}`) }()
	go func() { defer wg.Done(); results <- repo.FailModelRun(ctx, runID, "race", "race", `{"winner":"fail"}`) }()
	wg.Wait()
	close(results)
	var success, failure int
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, domain.ErrModelRunNotRunning) {
			failure++
		} else {
			t.Fatalf("unexpected race error: %v", err)
		}
	}
	if success != 1 || failure != 1 {
		t.Fatalf("race results success=%d failure=%d", success, failure)
	}
}

func TestCommitShotRefinementRollsBackOnStaleRun(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)
	oldRun, _, err := repo.CreateModelRun(ctx, assetID, "vision", "provider", "model", "old", "p1", "s1", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, oldRun, `{}`, `{}`); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitShotRefinement(ctx, assetID, oldRun, []domain.AssetShot{{StartMS: 0, EndMS: 1000, Description: "trusted"}}); err != nil {
		t.Fatal(err)
	}
	newRun, _, err := repo.CreateModelRun(ctx, assetID, "vision", "provider", "model", "new", "p1", "s1", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, newRun, `{}`, `{}`); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkModelRunCommitted(ctx, newRun); err != nil {
		t.Fatal(err)
	}
	err = repo.CommitShotRefinement(ctx, assetID, newRun, []domain.AssetShot{{StartMS: 0, EndMS: 1000, Description: "stale"}})
	if err == nil || !errors.Is(err, domain.ErrCommitRunNotValidated) {
		t.Fatalf("stale refinement error = %v", err)
	}
	shots, err := repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 1 || shots[0].Description != "trusted" {
		t.Fatalf("stale refinement replaced trusted shots: %+v", shots)
	}
}
