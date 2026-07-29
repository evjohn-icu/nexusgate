package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ev/timingdex/internal/domain"
)

func TestRetryJobReschedulesLeasedWorkWithBackoff(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-retry','retry-fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-retry", domain.JobAnalyze, "retry-input", 10); err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, "worker", time.Minute, domain.LeaseFilter{})
	if err != nil || job == nil || job.AttemptCount != 1 {
		t.Fatalf("lease job=%+v err=%v", job, err)
	}
	before := time.Now()
	if err := repo.RetryJob(ctx, job.ID, "temporary provider outage", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	jobs, err := repo.ListJobs(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	if jobs[0].State != domain.JobPending || jobs[0].RunAfter.Before(before.Add(time.Second)) || jobs[0].LastError != "temporary provider outage" {
		t.Fatalf("retry was not delayed and observable: %+v", jobs[0])
	}
	if next, err := repo.LeaseNextJob(ctx, "worker", time.Minute, domain.LeaseFilter{}); err != nil || next != nil {
		t.Fatalf("backoff job must not be immediately leased: job=%+v err=%v", next, err)
	}
}

// A permanent failure has to stop being leased. CompleteJob only sets
// state='failed', and the lease predicate accepts failed jobs that still have
// attempts left, so classifying an error as permanent used to change nothing
// except that the job was re-run with no backoff at all.
func TestFailJobTerminallyStopsFurtherLeasing(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "jobs-terminal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-terminal','terminal-fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-terminal", domain.JobAnalyze, "terminal-input", 10); err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, "worker", time.Minute, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease job=%+v err=%v", job, err)
	}
	if job.AttemptCount >= job.MaxAttempts {
		t.Fatalf("attempts must remain so the test is meaningful: %+v", job)
	}
	if err := repo.FailJobTerminally(ctx, job.ID, "video provider channel \"x\" is disabled"); err != nil {
		t.Fatal(err)
	}
	if next, err := repo.LeaseNextJob(ctx, "worker", time.Minute, domain.LeaseFilter{}); err != nil || next != nil {
		t.Fatalf("terminally failed job must not be leased again: job=%+v err=%v", next, err)
	}
	jobs, err := repo.ListJobs(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	if jobs[0].State != domain.JobFailed || jobs[0].LastError == "" {
		t.Fatalf("failure must stay observable on the progress page: %+v", jobs[0])
	}
	if !jobs[0].Terminal {
		t.Fatalf("the progress page distinguishes permanent failure by this flag: %+v", jobs[0])
	}
	// The reason the flag exists: the earlier implementation reached the same
	// unleasable state by setting attempt_count to max_attempts, so a job that
	// ran once was reported as having used every attempt.
	if jobs[0].AttemptCount != 1 || jobs[0].AttemptCount >= jobs[0].MaxAttempts {
		t.Fatalf("attempt_count must stay an honest count of actual runs: %+v", jobs[0])
	}
}

// Both ways a job stops being retried are one-way, and EnqueueJob is
// INSERT OR IGNORE, so rescanning cannot revive them. Configuring a provider
// that was missing when the job ran would otherwise leave that work stranded.
func TestRequeueFailedJobsRevivesTerminalAndExhaustedWork(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "jobs-requeue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-requeue','requeue-fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	// One job the pipeline classified permanent, one that simply ran out of
	// attempts, and one succeeded job that must be left alone: it is the
	// idempotency record that keeps re-running the pipeline cheap.
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,last_error_message,terminal,created_at,updated_at) VALUES
		('j-terminal','asset-requeue','transcribe','failed',0,1,3,?,'h-terminal','provider channel capability is not configured',1,?,?),
		('j-exhausted','asset-requeue','analyze','failed',0,3,3,?,'h-exhausted','upstream timeout',0,?,?),
		('j-done','asset-requeue','probe','succeeded',0,1,3,?,'h-done',NULL,0,?,?)`, now, now, now, now, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if next, err := repo.LeaseNextJob(ctx, "worker", time.Minute, domain.LeaseFilter{}); err != nil || next != nil {
		t.Fatalf("neither failed job should be leasable before requeue: job=%+v err=%v", next, err)
	}

	requeued, err := repo.RequeueFailedJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 2 {
		t.Fatalf("requeued=%d want 2 (the succeeded job must not be touched)", requeued)
	}

	leased := map[string]bool{}
	for range 2 {
		job, err := repo.LeaseNextJob(ctx, "worker", time.Minute, domain.LeaseFilter{})
		if err != nil || job == nil {
			t.Fatalf("requeued job should be leasable: job=%+v err=%v", job, err)
		}
		if job.AttemptCount != 1 {
			t.Fatalf("requeue must reset the attempt budget: %+v", job)
		}
		leased[job.ID] = true
	}
	if !leased["j-terminal"] || !leased["j-exhausted"] {
		t.Fatalf("both failure modes must come back: %v", leased)
	}

	jobs, err := repo.ListJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.ID == "j-done" && (job.State != domain.JobSucceeded || job.AttemptCount != 1) {
			t.Fatalf("succeeded job was disturbed: %+v", job)
		}
		if job.ID == "j-terminal" {
			if job.Terminal {
				t.Fatalf("requeue must clear the terminal flag or the job is unleasable again: %+v", job)
			}
			if job.LastError != "" {
				t.Fatalf("stale failure text would misreport a job that is running again: %+v", job)
			}
		}
	}
}

// leasePredicateSQL is the WHERE/ORDER BY of LeaseNextJob, kept here so the plan
// assertions below test the query that actually runs. An earlier version of this
// test hardcoded its own copy, which silently went stale the moment the size
// ceiling was added to the real one -- it kept passing while asserting a plan
// for a query nothing executed.
const leasePredicateSQL = `SELECT id FROM jobs INDEXED BY idx_jobs_lease_order WHERE state IN ('pending','failed') AND terminal=0 AND attempt_count<max_attempts AND run_after<=? AND (lease_expires_at IS NULL OR lease_expires_at<=?) AND (?=0 OR NOT EXISTS (SELECT 1 FROM assets a WHERE a.id=jobs.asset_id AND a.file_size>?)) ORDER BY priority DESC,created_at LIMIT 1`

// The lease predicate is the hottest query in the pipeline and its ORDER BY
// cannot be served by any state-leading index, so LeaseNextJob names the
// partial index from migration 0018 explicitly. This asserts the plan that
// INDEXED BY is there to guarantee -- including with the throttle's size
// ceiling engaged, which is the case most likely to cost the ordered scan.
func TestLeaseNextJobPlanAvoidsSort(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "jobs-plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ceiling := range []int64{0, 5_000_000} {
		plan := queryPlan(t, repo, leasePredicateSQL, "z", "z", ceiling, ceiling)
		if !strings.Contains(plan, "idx_jobs_lease_order") {
			t.Fatalf("ceiling=%d: lease must run on the ordered partial index:\n%s", ceiling, plan)
		}
		if strings.Contains(plan, "TEMP B-TREE") {
			t.Fatalf("ceiling=%d: the index exists precisely to remove this sort:\n%s", ceiling, plan)
		}
		// The size check must stay a primary-key lookup. A scan here would make
		// every throttled lease proportional to library size.
		if ceiling > 0 && !strings.Contains(plan, "USING INTEGER PRIMARY KEY") && !strings.Contains(plan, "USING INDEX sqlite_autoindex_assets") && !strings.Contains(plan, "USING PRIMARY KEY") {
			t.Fatalf("ceiling=%d: asset size check must seek, not scan:\n%s", ceiling, plan)
		}
	}
}

func queryPlan(t *testing.T, repo *Repository, query string, args ...any) string {
	t.Helper()
	rows, err := repo.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, aux int
		var detail string
		if err := rows.Scan(&id, &parent, &aux, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan
}

// The size ceiling must hold back an oversized asset without leasing it: leasing
// increments attempt_count, so a job that were leased and put back would burn
// its whole budget while waiting for the off-peak window it never got to see.
func TestLeaseNextJobHonoursSizeCeilingWithoutSpendingAttempts(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "jobs-ceiling.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('big','big-fp',9000,'discovered',?,?),('small','small-fp',10,'discovered',?,?)`, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "big", domain.JobDerive, "big-input", 100); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "small", domain.JobDerive, "small-input", 50); err != nil {
		t.Fatal(err)
	}

	// The oversized asset outranks the small one on priority, so if the ceiling
	// were ignored it would be handed out first.
	job, err := repo.LeaseNextJob(ctx, "worker", time.Minute, domain.LeaseFilter{MaxAssetBytes: 1000})
	if err != nil || job == nil {
		t.Fatalf("lease job=%+v err=%v", job, err)
	}
	if job.AssetID != "small" {
		t.Fatalf("ceiling ignored: leased %q", job.AssetID)
	}
	if next, err := repo.LeaseNextJob(ctx, "worker", time.Minute, domain.LeaseFilter{MaxAssetBytes: 1000}); err != nil || next != nil {
		t.Fatalf("oversized asset must stay held: job=%+v err=%v", next, err)
	}

	jobs, err := repo.ListJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, held := range jobs {
		if held.AssetID == "big" && held.AttemptCount != 0 {
			t.Fatalf("a held job must not spend an attempt: %+v", held)
		}
	}

	// Window opens: no ceiling, and the held work becomes available untouched.
	released, err := repo.LeaseNextJob(ctx, "worker", time.Minute, domain.LeaseFilter{})
	if err != nil || released == nil || released.AssetID != "big" {
		t.Fatalf("removing the ceiling must release the held job: job=%+v err=%v", released, err)
	}
	if released.AttemptCount != 1 {
		t.Fatalf("first real run must be attempt 1, not a resumed count: %+v", released)
	}
}

// CompleteJob is the contrast: it leaves the job leasable, which is why the
// permanent path needs its own method.
func TestCompleteJobLeavesFailedJobLeasable(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "jobs-completefail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-cf','cf-fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-cf", domain.JobAnalyze, "cf-input", 10); err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, "worker", time.Minute, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease job=%+v err=%v", job, err)
	}
	if err := repo.CompleteJob(ctx, job.ID, domain.JobFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	next, err := repo.LeaseNextJob(ctx, "worker", time.Minute, domain.LeaseFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if next == nil {
		t.Fatal("expected CompleteJob to leave the job leasable; if this now fails the lease predicate changed and FailJobTerminally may be redundant")
	}
}
