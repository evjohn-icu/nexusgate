package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/providerchannels"
	"github.com/evjohn-icu/timingdex/internal/providerpool"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// indexFailureRepo is the real repository with one pipeline stage forced to
// fail. What these tests are about is the queue accounting around a failure —
// which attempt was spent, which row is still leasable — so leasing, attempt
// counting and the defer all have to be the real SQL. A hand-written fake
// could only agree with whatever reading of the lease predicate it was written
// from, which is the reading under test.
type indexFailureRepo struct {
	*sqlite.Repository
	fail error
}

func (r *indexFailureRepo) RebuildSearch(context.Context, string) error { return r.fail }

// newQueuedIndexJob builds a Hub with one asset and one queued index job. Index
// is the cheapest stage that reaches a repository call, so the injected failure
// stands in for any provider failure without needing FFmpeg on PATH.
func newQueuedIndexJob(t *testing.T, fail error) *indexFailureRepo {
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
	if err := repo.EnqueueJob(ctx, assets[0].ID, domain.JobIndex, "index-input", 10); err != nil {
		t.Fatal(err)
	}
	return &indexFailureRepo{Repository: repo, fail: fail}
}

// singleJob drives the pipeline until the queue stops changing. RunUntilIdle
// returns as soon as the next job is not due yet, which is what a bounded
// backoff looks like from the outside, so a job that is meant to burn three
// attempts needs the pipeline to be woken again after each delay — exactly what
// re-running `timingdex pipeline run` does.
func singleJob(t *testing.T, repo *indexFailureRepo, pipeline *Pipeline, settled func(domain.Job) bool) domain.Job {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(15 * time.Second)
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

// Quota exhaustion is not the job's fault: the job never ran, the account did.
// Charging it an attempt would burn the whole budget inside ten seconds of
// backoff and then fail work permanently for something only waiting can fix.
func TestPipelineDefersWithoutSpendingAnAttemptWhenEveryProviderKeyFails(t *testing.T) {
	exhausted := fmt.Errorf("analyse asset: %w", fmt.Errorf("%w: %w",
		providerchannels.ErrRouteExhausted,
		providerpool.HTTPError{Code: 429, Err: errors.New("monthly quota exhausted")}))
	repo := newQueuedIndexJob(t, exhausted)
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral)

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
		t.Fatalf("a provider-wide outage must hand the attempt back: %+v", job)
	}
	if job.State != domain.JobPending || job.Terminal {
		t.Fatalf("a deferred job stays queued, not failed: %+v", job)
	}
	if job.DeferredReason != domain.JobDeferProviderRouteExhausted {
		t.Fatalf("a job parked for hours must say so, or the queue reads as stuck: %+v", job)
	}
	if job.RunAfter.Before(before.Add(providerRouteDeferral - time.Minute)) {
		t.Fatalf("defer did not park the job for the full wait: run_after=%s", job.RunAfter)
	}
	if !job.RunAfter.After(time.Now()) {
		t.Fatalf("a job due in the past is not deferred at all: run_after=%s", job.RunAfter)
	}
}

// The contrast, and the property the defer must not weaken: a failure that is
// the job's own still spends its three attempts and then stops for good.
func TestPipelineStillExhaustsThreeAttemptsOnAnOrdinaryFailure(t *testing.T) {
	repo := newQueuedIndexJob(t, errors.New("search index write failed: temporary io timeout"))
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral)

	job := singleJob(t, repo, pipeline, func(job domain.Job) bool { return job.Terminal })
	if !job.Terminal || job.State != domain.JobFailed {
		t.Fatalf("an ordinary failure must end terminally: %+v", job)
	}
	if job.AttemptCount != job.MaxAttempts {
		t.Fatalf("all three attempts must be spent before giving up: %+v", job)
	}
	if job.DeferredReason != "" {
		t.Fatalf("an ordinary failure is not a quota wait: %+v", job)
	}
}

// A permanent failure keeps failing on the first attempt: the defer path must
// not have made every failure look survivable.
func TestPipelineStillFailsPermanentErrorsOnTheFirstAttempt(t *testing.T) {
	repo := newQueuedIndexJob(t, domain.Permanent(errors.New("video analysis provider is not configured")))
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral)

	if _, err := pipeline.RunUntilIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobs, err := repo.ListJobs(context.Background(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	if !jobs[0].Terminal || jobs[0].AttemptCount != 1 {
		t.Fatalf("a configuration error must not be retried: %+v", jobs[0])
	}
}
