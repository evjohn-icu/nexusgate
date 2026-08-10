package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// deriveWriteFailureRepo is the real repository with SaveArtifact forced to
// fail. Derive reaches it after every render, so the injected error stands in
// for the ENOSPC the filesystem answers mid-write — the case this file
// classifies. Leasing, attempt counting and the defer run against the real
// SQL for the same reason pipeline_defer_test.go uses it.
type deriveWriteFailureRepo struct {
	*sqlite.Repository
	fail error
}

func (r *deriveWriteFailureRepo) SaveArtifact(context.Context, domain.DerivedArtifact, string, string) error {
	return r.fail
}

// newQueuedDeriveWriteFailure builds a Hub with one asset, one queued derive
// job, and the thumbnail/proxy already present in the pipeline cache dir so
// the stage skips ffmpeg entirely: what is under test is the error
// classification, not the render. The failure is then delivered by the
// artifact write, which is exactly where a full disk surfaces in production.
func newQueuedDeriveWriteFailure(t *testing.T, cacheDir string, fail error) *deriveWriteFailureRepo {
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
	base := filepath.Join(cacheDir, assets[0].ID)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	// The software-mode artifact names, so needThumb/needProxy are both false
	// and the ffmpeg renderer is never reached.
	for _, name := range []string{"thumbnail-software.jpg", "proxy-software.mp4"} {
		if err := os.WriteFile(filepath.Join(base, name), []byte("placeholder"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.EnqueueJob(ctx, assets[0].ID, domain.JobDerive, "derive-input", 90); err != nil {
		t.Fatal(err)
	}
	return &deriveWriteFailureRepo{Repository: repo, fail: fail}
}

// settleDiskJob drives the pipeline until the job reaches a state the caller
// accepts or the deadline passes. RunUntilIdle returns as soon as the next
// job is not due yet — a retry backoff looks like that from the outside — so
// a job meant to spend attempts is re-entered, exactly as re-running
// `timingdex pipeline run` does.
func settleDiskJob(t *testing.T, repo *deriveWriteFailureRepo, pipeline *Pipeline, settled func(domain.Job) bool) domain.Job {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)
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

// A disk that answers ENOSPC mid-write is not the job's fault: the next
// attempt writes to the same full volume and fails the same way, so
// hot-retrying would burn the job's whole attempt budget in seconds — derive
// fails mid-encode, when the most work has already been paid for. The job
// must park with the disk_space_low reason and hand the attempt back.
func TestPipelineDefersDeriveOnWrappedENOSPCWithoutSpendingAnAttempt(t *testing.T) {
	cacheDir := t.TempDir()
	repo := newQueuedDeriveWriteFailure(t, cacheDir, fmt.Errorf("derive: %w", syscall.ENOSPC))
	pipeline := NewPipeline(repo, cacheDir, nil, nil, nil, nil, nil, media.HardwarePlan{Mode: "software"}, nil, providerRouteDeferral, 0)

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
	// ListJobs only projects DeferredReason for provider_route_exhausted (the
	// repository layer's one surfaced reason), so the persisted reason code is
	// read from the row it is written to. The code is the wire contract the
	// queue's visible state and the resume machinery read.
	var code string
	if err := repo.DB().QueryRowContext(context.Background(), `SELECT COALESCE(last_error_code,'') FROM jobs WHERE id=?`, job.ID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if code != domain.JobDeferDiskSpaceLow {
		t.Fatalf("an ENOSPC failure must defer with the disk_space_low reason: code=%q job=%+v", code, job)
	}
	if job.RunAfter.Before(before.Add(diskSpaceRetryDelay - time.Minute)) {
		t.Fatalf("defer did not park the job for the full wait: run_after=%s", job.RunAfter)
	}
	if !job.RunAfter.After(time.Now()) {
		t.Fatalf("a job due in the past is not deferred at all: run_after=%s", job.RunAfter)
	}
	if !strings.Contains(job.LastError, "no space left on device") {
		t.Fatalf("the defer must record the failure that parked the job: %+v", job)
	}
}

// The contrast, and the property the classification must not weaken: a
// failure that is the job's own still spends its three attempts and then
// stops for good. The wrapper shares the derive stage and the artifact write
// with the ENOSPC case, so the only difference is the error's identity.
func TestPipelineStillExhaustsAllAttemptsOnAnOrdinaryDeriveFailure(t *testing.T) {
	cacheDir := t.TempDir()
	repo := newQueuedDeriveWriteFailure(t, cacheDir, errors.New("artifact write failed: temporary io timeout"))
	pipeline := NewPipeline(repo, cacheDir, nil, nil, nil, nil, nil, media.HardwarePlan{Mode: "software"}, nil, providerRouteDeferral, 0)

	job := settleDiskJob(t, repo, pipeline, func(job domain.Job) bool { return job.Terminal })
	if !job.Terminal || job.State != domain.JobFailed {
		t.Fatalf("an ordinary failure must end terminally: %+v", job)
	}
	if job.AttemptCount != job.MaxAttempts {
		t.Fatalf("all attempts must be spent before giving up: %+v", job)
	}
	if job.DeferredReason != "" {
		t.Fatalf("an ordinary failure is not a disk-space wait: %+v", job)
	}
}

// The classification is errors.Is against the sentinel, never the message
// text: a plain error whose prose happens to read "no space left on device"
// must stay retryable, and an ENOSPC must stay a disk-full verdict through
// every wrapper — including domain.Permanent's, which deliberately keeps the
// wrapped chain reachable.
func TestIsNoSpaceErrMatchesTheSentinelNotTheMessage(t *testing.T) {
	if !isNoSpaceErr(fmt.Errorf("derive: %w", syscall.ENOSPC)) {
		t.Fatal("a wrapped ENOSPC must classify as a full disk")
	}
	if !isNoSpaceErr(domain.Permanent(fmt.Errorf("derive: %w", syscall.ENOSPC))) {
		t.Fatal("the marker must not hide the ENOSPC behind it")
	}
	if isNoSpaceErr(errors.New("no space left on device")) {
		t.Fatal("message text must never classify: that is how unrelated prose joins the verdict")
	}
	if isNoSpaceErr(nil) {
		t.Fatal("nil is not a full disk")
	}
}
