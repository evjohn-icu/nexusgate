package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// completeJobLeaseLostRepo wraps a real *sqlite.Repository, overrides
// CompleteJob to always return ErrJobLeaseLost (simulating the race where
// another holder reclaimed the lease after this one finished the work), and
// forces a long cooldown so the test can tell whether cooldown was skipped.
type completeJobLeaseLostRepo struct {
	*sqlite.Repository
}

func (r *completeJobLeaseLostRepo) CompleteJob(_ context.Context, _, _ string, _ domain.JobState, _ string) error {
	return domain.ErrJobLeaseLost
}

func (r *completeJobLeaseLostRepo) GetPipelineThrottle(_ context.Context) (domain.PipelineThrottle, error) {
	return domain.PipelineThrottle{
		CooldownSeconds: 3600,
		OffPeakEnabled:  false,
	}, nil
}

// TestRunUntilIdleSkipsCooldownWhenCompleteJobLosesLease verifies that when
// CompleteJob returns ErrJobLeaseLost the cooldown is skipped rather than
// wasting a full sleep on a result that was already discarded. It uses a
// real *sqlite.Repository base so leasing and job lifecycle are the real
// code, and overrides only CompleteJob (inject lease-lost) and
// GetPipelineThrottle (force a 1-hour cooldown outside off-peak). If the
// cooldown is not skipped, RunUntilIdle would block on sleepContext for 3600
// seconds; a 2-second context timeout catches that. The JobIndex stage is the
// cheapest path that reaches a pipeline stage without FFmpeg on PATH — it
// calls only RebuildSearch, which the real repo handles without external deps.
func TestRunUntilIdleSkipsCooldownWhenCompleteJobLosesLease(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dir, "pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Join(dir, "footage")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(rootDir, "clip.mov")
	if err := os.WriteFile(source, []byte("footage"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, rootDir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertScannedFile(ctx, root, "clip.mov", source, info, "fp-clip"); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	if err := repo.EnqueueJob(ctx, assets[0].ID, domain.JobIndex, "index-input", 10); err != nil {
		t.Fatal(err)
	}

	wrapped := &completeJobLeaseLostRepo{Repository: repo}
	pipeline := NewPipeline(wrapped, t.TempDir(), nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral)

	timeoutCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	start := time.Now()
	executed, err := pipeline.RunUntilIdle(timeoutCtx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("RunUntilIdle should not error when cooldown is skipped: %v", err)
	}
	if executed != 1 {
		t.Fatalf("expected 1 executed, got %d", executed)
	}
	if elapsed > time.Second {
		t.Fatalf("cooldown was not skipped; RunUntilIdle took %v (expected <1s)", elapsed)
	}
}
