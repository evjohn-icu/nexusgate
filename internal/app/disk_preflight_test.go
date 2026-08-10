package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// newQueuedDeriveJob builds a Hub with one asset and one queued derive job.
// The source is a placeholder file, so the job can never actually derive —
// what matters is that the preflight verdict is decided before any of that
// work is reached. Leasing, attempt counting and the defer all run against
// the real SQL, for the same reason pipeline_defer_test.go uses it: a
// hand-written fake could only agree with whatever reading of the lease
// predicate it was written from.
func newQueuedDeriveJob(t *testing.T) *sqlite.Repository {
	t.Helper()
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
	if err := repo.EnqueueJob(ctx, assets[0].ID, domain.JobDerive, "derive-input", 90); err != nil {
		t.Fatal(err)
	}
	return repo
}

// settleJob drives the pipeline until the job reaches a state the caller
// accepts or the deadline passes. RunUntilIdle returns as soon as the next
// job is not due yet — which is what a retry backoff looks like from the
// outside — so the caller must re-enter it, exactly as re-running
// `timingdex pipeline run` does.
func settleJob(t *testing.T, repo *sqlite.Repository, pipeline *Pipeline, settled func(domain.Job) bool) domain.Job {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := pipeline.RunUntilIdle(ctx); err != nil {
			t.Fatal(err)
		}
		jobs, err := repo.ListJobs(ctx, 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("jobs=%+v err=%v", jobs, err)
		}
		if settled(jobs[0]) || time.Now().After(deadline) {
			return jobs[0]
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A full disk is not the job's fault: ffmpeg would fail mid-encode with an
// ordinary retryable error, burning the job's attempts against a volume that
// only the operator can free. The preflight must park the job instead and
// hand the attempt back, exactly like the provider-route deferral does.
func TestDiskSpacePreflightDefersHeavyJobWithoutSpendingAnAttempt(t *testing.T) {
	repo := newQueuedDeriveJob(t)
	orig := diskFreeCheck
	diskFreeCheck = func(string) (int64, error) { return 0, nil }
	t.Cleanup(func() { diskFreeCheck = orig })
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 1<<62)

	before := time.Now()
	if _, err := pipeline.RunUntilIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobs, err := repo.ListJobs(context.Background(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	job := jobs[0]
	if job.AttemptCount != 0 {
		t.Fatalf("a full disk must hand the attempt back: %+v", job)
	}
	if job.State != domain.JobPending || job.Terminal {
		t.Fatalf("a deferred job stays queued, not failed: %+v", job)
	}
	if job.RunAfter.Before(before.Add(diskSpaceRetryDelay - time.Minute)) {
		t.Fatalf("defer did not park the job for the full wait: run_after=%s", job.RunAfter)
	}
	if !job.RunAfter.After(time.Now()) {
		t.Fatalf("a job due in the past is not deferred at all: run_after=%s", job.RunAfter)
	}
	if !strings.Contains(job.LastError, "below the configured minimum") {
		t.Fatalf("the defer must record the floor that parked the job: %+v", job)
	}
}

// The contrast: a fresh install ships with the knob at zero, and zero must
// behave exactly as before the preflight existed. The probe is replaced with
// one that fails the test if it is even consulted.
func TestDiskSpacePreflightDisabledLetsHeavyJobRunItsCourse(t *testing.T) {
	repo := newQueuedDeriveJob(t)
	orig := diskFreeCheck
	diskFreeCheck = func(string) (int64, error) {
		t.Fatal("the preflight must not probe the disk when minimum_free_space_bytes is 0")
		return 0, nil
	}
	t.Cleanup(func() { diskFreeCheck = orig })
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)

	job := settleJob(t, repo, pipeline, func(job domain.Job) bool { return job.Terminal })
	if job.State != domain.JobFailed || !job.Terminal {
		t.Fatalf("with the preflight disabled the job must run its own course to a verdict: %+v", job)
	}
	if job.AttemptCount != job.MaxAttempts {
		t.Fatalf("all attempts must be spent before giving up: %+v", job)
	}
	if job.DeferredReason != "" {
		t.Fatalf("no deferral may happen with the preflight disabled: %+v", job)
	}
}

// A probe that errors must read as "unknown", never as "blocked": a false
// blocker would park every heavy job in the queue while the operator hunts a
// disk problem that does not exist. This is also the Windows behaviour.
func TestDiskSpacePreflightFailsOpenWhenTheProbeErrors(t *testing.T) {
	repo := newQueuedDeriveJob(t)
	orig := diskFreeCheck
	diskFreeCheck = func(string) (int64, error) { return 0, errors.New("probe broken") }
	t.Cleanup(func() { diskFreeCheck = orig })
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 1<<62)

	job := settleJob(t, repo, pipeline, func(job domain.Job) bool { return job.Terminal })
	if job.State != domain.JobFailed || !job.Terminal {
		t.Fatalf("an unreadable probe must not park the job: %+v", job)
	}
	if job.DeferredReason != "" {
		t.Fatalf("an unreadable probe must not produce a defer: %+v", job)
	}
}

// The reason code is part of the wire contract between the pipeline and
// whatever surface renders the queue (/progress, JobSummary, the deferred-job
// resume machinery), so it is pinned like JobDeferProviderRouteExhausted.
func TestJobDeferDiskSpaceLowReasonCodeIsStable(t *testing.T) {
	if domain.JobDeferDiskSpaceLow != "disk_space_low" {
		t.Fatalf("reason code=%q, want %q", domain.JobDeferDiskSpaceLow, "disk_space_low")
	}
}
