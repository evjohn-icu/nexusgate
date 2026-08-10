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

// newQueuedAnalyzeJob builds a Hub with one asset and one queued analyze job.
// The source is a placeholder file and no video provider is wired, so the job
// can never actually analyze — what matters is that the budget gate verdict
// is decided before any of that work is reached. Leasing, attempt counting
// and the defer all run against the real SQL, for the same reason
// disk_preflight_test.go uses it: a hand-written fake could only agree with
// whatever reading of the lease predicate it was written from.
func newQueuedAnalyzeJob(t *testing.T) *sqlite.Repository {
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
	if err := repo.EnqueueJob(ctx, assets[0].ID, domain.JobAnalyze, "analyze-input", 30); err != nil {
		t.Fatal(err)
	}
	return repo
}

// seedCostLedger adds one estimate row to today's (UTC) ledger.
func seedCostLedger(t *testing.T, repo *sqlite.Repository, estimate float64) {
	t.Helper()
	ctx := context.Background()
	err := repo.RecordCostEstimate(ctx, domain.CostEntry{
		Day:        time.Now().UTC().Format("2006-01-02"),
		Capability: "video_analysis",
		Provider:   "gemini",
		Model:      "gemini-flash",
		Estimate:   estimate,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A spent daily budget is not the job's fault: the next attempt would present
// identical inputs to the same paid call at the same spent budget, so the job
// must be parked until the period rolls over and the attempt handed back,
// exactly like the disk-space and provider-route deferrals.
func TestDailyBudgetExhaustedDefersAnalyzeJobWithoutSpendingAnAttempt(t *testing.T) {
	repo := newQueuedAnalyzeJob(t)
	seedCostLedger(t, repo, 60)
	if err := repo.SavePipelineThrottle(context.Background(), domain.PipelineThrottle{DailyBudget: 50}); err != nil {
		t.Fatal(err)
	}
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)

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
		t.Fatalf("a spent budget must hand the attempt back: %+v", job)
	}
	if job.State != domain.JobPending || job.Terminal {
		t.Fatalf("a deferred job stays queued, not failed: %+v", job)
	}
	if job.DeferredReason != domain.JobDeferBudgetExhausted {
		t.Fatalf("deferred_reason=%q, want %q", job.DeferredReason, domain.JobDeferBudgetExhausted)
	}
	want := nextBudgetReset(before)
	if !job.RunAfter.Equal(want) {
		t.Fatalf("run_after=%s, want next-day reset %s", job.RunAfter, want)
	}
	if !job.RunAfter.After(before) {
		t.Fatalf("a job due in the past is not deferred at all: run_after=%s", job.RunAfter)
	}
}

// The contrast: a fresh install ships with the knobs at zero, and zero must
// behave exactly as before the gate existed. The ledger read must not even be
// consulted, so the job runs its own course to a verdict.
func TestBudgetDisabledLetsAnalyzeJobRunItsCourse(t *testing.T) {
	repo := newQueuedAnalyzeJob(t)
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)

	job := settleJob(t, repo, pipeline, func(job domain.Job) bool { return job.Terminal })
	if job.State != domain.JobFailed || !job.Terminal {
		t.Fatalf("with the gate disabled the job must run its own course to a verdict: %+v", job)
	}
	if job.DeferredReason != "" {
		t.Fatalf("no deferral may happen with the budget disabled: %+v", job)
	}
}

// A budget that is set but not yet spent must let the job through: the gate
// is a cap, not a gate on the first call.
func TestBudgetBelowThresholdLetsAnalyzeJobRunItsCourse(t *testing.T) {
	repo := newQueuedAnalyzeJob(t)
	seedCostLedger(t, repo, 30)
	if err := repo.SavePipelineThrottle(context.Background(), domain.PipelineThrottle{DailyBudget: 50}); err != nil {
		t.Fatal(err)
	}
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)

	job := settleJob(t, repo, pipeline, func(job domain.Job) bool { return job.Terminal })
	if job.State != domain.JobFailed || !job.Terminal {
		t.Fatalf("a job under the budget must run its own course to a verdict: %+v", job)
	}
	if job.DeferredReason != "" {
		t.Fatalf("a job under the budget must not be deferred: %+v", job)
	}
}

// A spent monthly budget parks the job until the 1st of the next month, not
// the next day: a daily park would only re-hit the monthly gate the next day.
func TestMonthlyBudgetExhaustedDefersToFirstOfNextMonth(t *testing.T) {
	repo := newQueuedAnalyzeJob(t)
	ctx := context.Background()
	monthKey := time.Now().UTC().Format("2006-01-02")[:7]
	// Two entries on past days of the current month; the monthly sum crosses
	// the budget while no single day does.
	for i, estimate := range []float64{30, 30} {
		day := monthKey + "-0" + string(rune('1'+i))
		if err := repo.RecordCostEstimate(ctx, domain.CostEntry{Day: day, Capability: "video_analysis", Provider: "gemini", Estimate: estimate}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.SavePipelineThrottle(ctx, domain.PipelineThrottle{MonthlyBudget: 50}); err != nil {
		t.Fatal(err)
	}
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)

	before := time.Now()
	if _, err := pipeline.RunUntilIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobs, err := repo.ListJobs(context.Background(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	job := jobs[0]
	if job.DeferredReason != domain.JobDeferBudgetExhausted {
		t.Fatalf("deferred_reason=%q, want %q", job.DeferredReason, domain.JobDeferBudgetExhausted)
	}
	if job.AttemptCount != 0 {
		t.Fatalf("a spent monthly budget must hand the attempt back: %+v", job)
	}
	want := nextMonthBudgetReset(before)
	if !job.RunAfter.Equal(want) {
		t.Fatalf("run_after=%s, want first-of-month reset %s", job.RunAfter, want)
	}
}

// When both budgets are spent the monthly park wins: a daily park would only
// re-hit the monthly gate the next day, so the shorter deferral buys nothing.
func TestMonthlyBudgetWinsWhenBothBudgetsAreSpent(t *testing.T) {
	repo := newQueuedAnalyzeJob(t)
	seedCostLedger(t, repo, 60)
	if err := repo.SavePipelineThrottle(context.Background(), domain.PipelineThrottle{DailyBudget: 50, MonthlyBudget: 50}); err != nil {
		t.Fatal(err)
	}
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)

	before := time.Now()
	if _, err := pipeline.RunUntilIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobs, err := repo.ListJobs(context.Background(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	job := jobs[0]
	if job.DeferredReason != domain.JobDeferBudgetExhausted {
		t.Fatalf("deferred_reason=%q, want %q", job.DeferredReason, domain.JobDeferBudgetExhausted)
	}
	if want := nextMonthBudgetReset(before); !job.RunAfter.Equal(want) {
		t.Fatalf("run_after=%s, want the monthly reset %s (monthly must win)", job.RunAfter, want)
	}
}

// The reason code is part of the wire contract between the pipeline and
// whatever surface renders the queue (/progress, JobSummary, the issues view,
// the deferred-job resume machinery), so it is pinned like the other reasons.
func TestJobDeferBudgetExhaustedReasonCodeIsStable(t *testing.T) {
	if domain.JobDeferBudgetExhausted != "budget_exhausted" {
		t.Fatalf("reason code=%q, want %q", domain.JobDeferBudgetExhausted, "budget_exhausted")
	}
}

// The alignment stage is a paid provider call like transcribe/analyze, so a
// spent budget must park it before any of it runs — the gate fires before the
// audio artifact or transcript are even read, let alone Align called.
// TestSpentBudgetDoesNotDeferAlignJob pins the corrected alignment gate:
// alignment runs a local os/exec provider and costs no Provider money, so a
// spent daily budget must not park it until the next period. The job runs to
// success normally; only the paid analyze successor is deferred.
func TestSpentBudgetDoesNotDeferAlignJob(t *testing.T) {
	repo := newQueuedAlignJob(t)
	seedCostLedger(t, repo, 60)
	if err := repo.SavePipelineThrottle(context.Background(), domain.PipelineThrottle{DailyBudget: 50}); err != nil {
		t.Fatal(err)
	}
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, fakeAlignmentProvider{}, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)

	if _, err := pipeline.RunUntilIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobs, err := repo.ListJobs(context.Background(), 10)
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	var align, analyze domain.Job
	for _, j := range jobs {
		switch j.Type {
		case domain.JobAlign:
			align = j
		case domain.JobAnalyze:
			analyze = j
		}
	}
	if align.ID == "" {
		t.Fatalf("no align job in %+v", jobs)
	}
	if align.State != domain.JobSucceeded {
		t.Fatalf("a spent budget must not park a local, free alignment job; align=%+v", align)
	}
	if align.AttemptCount != 1 {
		t.Fatalf("align must run exactly once and succeed: %+v", align)
	}
	// The paid stage behind it is still budget-gated: analyze is deferred to
	// the next day instead of spending a call.
	if analyze.ID == "" {
		t.Fatalf("no analyze successor in %+v", jobs)
	}
	if analyze.State != domain.JobPending || analyze.DeferredReason != domain.JobDeferBudgetExhausted {
		t.Fatalf("analyze successor must be budget-deferred, got %+v", analyze)
	}
	if want := nextBudgetReset(time.Now()); !analyze.RunAfter.Equal(want) {
		t.Fatalf("analyze run_after=%s, want next-day reset %s", analyze.RunAfter, want)
	}
}

// newQueuedAlignJob seeds an asset with audio artifact + transcript and a
// queued align job, the preconditions a real alignment pass needs. The budget
// gate must short-circuit before any of them are read.
func newQueuedAlignJob(t *testing.T) *sqlite.Repository {
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
	footage := filepath.Join(dir, "footage")
	if err := os.MkdirAll(footage, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(footage, "clip.mov")
	if err := os.WriteFile(source, []byte("footage"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, footage)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := repo.UpsertScannedFile(ctx, root, "clip.mov", source, info, "fp-align")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, scanned.AssetID, domain.JobDerive, "seed-artifact", 50); err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, "seed", nil, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease for artifact seed: job=%+v err=%v", job, err)
	}
	if err := repo.SaveArtifact(ctx, domain.DerivedArtifact{ID: "art-audio", AssetID: scanned.AssetID, Type: "audio", ProfileHash: "a1", LocalPath: filepath.Join(dir, "audio.wav"), SizeBytes: 1}, job.ID, "seed"); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteJob(ctx, job.ID, "seed", domain.JobSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	untimed := domain.Transcript{Language: "zh", Text: "整段视频的完整语音内容", Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 0, Text: "整段视频的完整语音内容"}}}
	if err := repo.SaveTranscript(ctx, scanned.AssetID, "qwen", "qwen3-asr-flash", "thash", untimed); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, scanned.AssetID, domain.JobAlign, "align-input", 50); err != nil {
		t.Fatal(err)
	}
	return repo
}
