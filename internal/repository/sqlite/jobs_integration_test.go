package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
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
	job, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
	if err != nil || job == nil || job.AttemptCount != 1 {
		t.Fatalf("lease job=%+v err=%v", job, err)
	}
	before := time.Now()
	if err := repo.RetryJob(ctx, job.ID, "worker", domain.JobFailureCategoryProviderUnavailable, "temporary provider outage", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	jobs, err := repo.ListJobs(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	if jobs[0].State != domain.JobPending || jobs[0].RunAfter.Before(before.Add(time.Second)) || jobs[0].LastError != "temporary provider outage" {
		t.Fatalf("retry was not delayed and observable: %+v", jobs[0])
	}
	if next, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{}); err != nil || next != nil {
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
	job, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease job=%+v err=%v", job, err)
	}
	if job.AttemptCount >= job.MaxAttempts {
		t.Fatalf("attempts must remain so the test is meaningful: %+v", job)
	}
	if err := repo.FailJobTerminally(ctx, job.ID, "worker", domain.JobFailureCategoryConfiguration, "video provider channel \"x\" is disabled"); err != nil {
		t.Fatal(err)
	}
	if next, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{}); err != nil || next != nil {
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
	if next, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{}); err != nil || next != nil {
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
		job, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
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

// A category-scoped requeue must revive only the jobs that actually carry
// that code — the issues view's per-row retry depends on it. The 'unknown'
// bucket is the NULL/empty fallback JobIssues reports, so it must match rows
// without a code, and an empty-string category matches nothing at all: the
// API keeps "" meaning "everything" and this method's WHERE clause is what
// narrows.
func TestRequeueFailedJobsByCategoryRevivesOnlyThatCategory(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "jobs-requeue-category.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-cat','cat-fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	quota := string(domain.JobFailureCategoryProviderQuota)
	auth := string(domain.JobFailureCategoryProviderAuth)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,last_error_message,last_error_code,terminal,created_at,updated_at) VALUES
		('j-quota','asset-cat','analyze','failed',0,3,3,?,'h-quota','monthly quota exhausted',?,1,?,?),
		('j-auth','asset-cat','analyze','failed',0,3,3,?,'h-auth','key rejected',?,0,?,?),
		('j-uncoded','asset-cat','probe','failed',0,3,3,?,'h-uncoded','predates classification',NULL,0,?,?),
		('j-done','asset-cat','probe','succeeded',0,1,3,?,'h-done',NULL,NULL,0,?,?)`,
		now, quota, now, now, now, auth, now, now, now, now, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}

	// Empty-string category narrows to nothing: requeueing everything is the
	// API's job when the body is absent, never the repository's default.
	if n, err := repo.RequeueFailedJobsByCategory(ctx, ""); err != nil || n != 0 {
		t.Fatalf("empty category requeued=%d err=%v, want 0", n, err)
	}

	if n, err := repo.RequeueFailedJobsByCategory(ctx, quota); err != nil || n != 1 {
		t.Fatalf("provider_quota requeued=%d err=%v, want 1", n, err)
	}

	// Only the quota job may have moved: its terminal flag and attempt budget
	// must be reset the same way the all-failed requeue does.
	var state, code string
	var terminal int
	var attempts int
	if err := repo.db.QueryRowContext(ctx, `SELECT state,COALESCE(last_error_code,''),terminal,attempt_count FROM jobs WHERE id='j-quota'`).Scan(&state, &code, &terminal, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.JobPending) || code != "" || terminal != 0 || attempts != 0 {
		t.Fatalf("requeued job state=%q code=%q terminal=%d attempts=%d, want pending / no code / 0 / 0", state, code, terminal, attempts)
	}
	for _, id := range []string{"j-auth", "j-uncoded"} {
		if err := repo.db.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=?`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != string(domain.JobFailed) {
			t.Fatalf("job %s was disturbed by another category's requeue: state=%q", id, state)
		}
	}

	// The unknown bucket matches the NULL-code row, and re-running a category
	// that is now empty returns 0 rather than erroring.
	if n, err := repo.RequeueFailedJobsByCategory(ctx, string(domain.JobFailureCategoryUnknown)); err != nil || n != 1 {
		t.Fatalf("unknown requeued=%d err=%v, want 1", n, err)
	}
	if n, err := repo.RequeueFailedJobsByCategory(ctx, quota); err != nil || n != 0 {
		t.Fatalf("re-running provider_quota requeued=%d err=%v, want 0", n, err)
	}
	if n, err := repo.RequeueFailedJobsByCategory(ctx, auth); err != nil || n != 1 {
		t.Fatalf("provider_auth requeued=%d err=%v, want 1", n, err)
	}
}

// A provider-wide outage is not the job's failure, so parking a job for it must
// leave the queue exactly as it found it apart from the wait: same attempt
// budget, same leasable row, no terminal flag.
func TestDeferJobParksWorkWithoutSpendingAnAttempt(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "jobs-defer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-defer','defer-fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-defer", domain.JobAnalyze, "defer-input", 10); err != nil {
		t.Fatal(err)
	}

	// Four cycles is one more than max_attempts: a deferral that quietly spent
	// an attempt would strand the job on the fourth, which is the whole reason
	// this method exists rather than a longer RetryJob delay.
	for cycle := range 4 {
		job, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
		if err != nil || job == nil {
			t.Fatalf("cycle %d: a deferred job must come back: job=%+v err=%v", cycle, job, err)
		}
		if job.AttemptCount != 1 {
			t.Fatalf("cycle %d: every run must start from an untouched budget: %+v", cycle, job)
		}
		resumeAt := time.Now().Add(5 * time.Hour)
		if err := repo.DeferJob(ctx, job.ID, "worker", resumeAt, domain.JobDeferProviderRouteExhausted, "every provider key on this route is failing"); err != nil {
			t.Fatal(err)
		}

		jobs, err := repo.ListJobs(ctx, 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("cycle %d: jobs=%+v err=%v", cycle, jobs, err)
		}
		parked := jobs[0]
		if parked.AttemptCount != 0 {
			t.Fatalf("cycle %d: the attempt the lease spent must be handed back: %+v", cycle, parked)
		}
		if parked.State != domain.JobPending || parked.Terminal {
			t.Fatalf("cycle %d: a parked job is queued, not failed: %+v", cycle, parked)
		}
		if parked.DeferredReason != domain.JobDeferProviderRouteExhausted {
			t.Fatalf("cycle %d: a job waiting hours must be distinguishable from a stuck queue: %+v", cycle, parked)
		}
		if !parked.RunAfter.After(time.Now().Add(4 * time.Hour)) {
			t.Fatalf("cycle %d: run_after=%s did not carry the wait", cycle, parked.RunAfter)
		}
		if next, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{}); err != nil || next != nil {
			t.Fatalf("cycle %d: a parked job must not be handed out early: job=%+v err=%v", cycle, next, err)
		}

		// The wait elapses. Nothing else is touched, so this asserts the row is
		// leasable purely on run_after -- including the attempt_count and
		// lease_expires_at terms of the lease predicate.
		if _, err := repo.db.ExecContext(ctx, `UPDATE jobs SET run_after=? WHERE id=?`, formatTime(time.Now().Add(-time.Second)), job.ID); err != nil {
			t.Fatal(err)
		}
		due, err := repo.ListJobs(ctx, 10)
		if err != nil || len(due) != 1 {
			t.Fatalf("cycle %d: jobs=%+v err=%v", cycle, due, err)
		}
		if due[0].DeferredReason != "" {
			t.Fatalf("cycle %d: a job that is due again is no longer waiting: %+v", cycle, due[0])
		}
	}
}

// The case the decrement is actually for. On the third lease attempt_count
// equals max_attempts, and RetryJob -- whose WHERE requires attempt_count <
// max_attempts -- can no longer reschedule anything. Deferring has to restore
// the attempt, not merely postpone, or the job would sit unleasable forever.
func TestDeferJobOnTheLastAttemptStillLeavesTheJobLeasable(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "jobs-defer-last.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-last','last-fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-last", domain.JobAnalyze, "last-input", 10); err != nil {
		t.Fatal(err)
	}

	var job *domain.Job
	for attempt := 1; attempt <= 3; attempt++ {
		job, err = repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
		if err != nil || job == nil {
			t.Fatalf("attempt %d: job=%+v err=%v", attempt, job, err)
		}
		if job.AttemptCount != attempt {
			t.Fatalf("attempt %d: leased job reports %d attempts", attempt, job.AttemptCount)
		}
		if attempt < 3 {
			if err := repo.RetryJob(ctx, job.ID, "worker", domain.JobFailureCategoryUnknown, "transient", 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	if job.AttemptCount != job.MaxAttempts {
		t.Fatalf("the third lease should have spent the budget: %+v", job)
	}

	// RetryJob is the method that cannot help here, and saying so out loud is
	// the point: it matches no row, so the job would be left running.
	if err := repo.RetryJob(ctx, job.ID, "worker", domain.JobFailureCategoryUnknown, "transient", 0); err != nil {
		t.Fatal(err)
	}
	stuck, err := repo.ListJobs(ctx, 10)
	if err != nil || len(stuck) != 1 {
		t.Fatalf("jobs=%+v err=%v", stuck, err)
	}
	if stuck[0].State != domain.JobRunning {
		t.Fatalf("RetryJob is expected to be a no-op at max_attempts; if it now reschedules, DeferJob's decrement may be reconsidered: %+v", stuck[0])
	}

	if err := repo.DeferJob(ctx, job.ID, "worker", time.Now().Add(-time.Second), domain.JobDeferProviderRouteExhausted, "every provider key on this route is failing"); err != nil {
		t.Fatal(err)
	}
	next, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
	if err != nil || next == nil {
		t.Fatalf("a job deferred on its last attempt must still run again: job=%+v err=%v", next, err)
	}
	if next.AttemptCount != 3 {
		t.Fatalf("the restored attempt must be the one the outage consumed, not a fresh budget: %+v", next)
	}
}

// leasePredicateSQL is the WHERE/ORDER BY of LeaseNextJob, kept here so the plan
// assertions below test the query that actually runs. An earlier version of this
// test hardcoded its own copy, which silently went stale the moment the size
// ceiling was added to the real one -- it kept passing while asserting a plan
// for a query nothing executed.
const leasePredicateSQL = `SELECT id FROM jobs INDEXED BY idx_jobs_lease_order WHERE state IN ('pending','failed','running') AND terminal=0 AND assigned_worker_id IS NULL AND attempt_count<max_attempts AND run_after<=? AND (lease_expires_at IS NULL OR lease_expires_at<=?) AND (?=0 OR NOT EXISTS (SELECT 1 FROM assets a WHERE a.id=jobs.asset_id AND a.file_size>?)) ORDER BY priority DESC,created_at LIMIT 1`

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
	job, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{MaxAssetBytes: 1000})
	if err != nil || job == nil {
		t.Fatalf("lease job=%+v err=%v", job, err)
	}
	if job.AssetID != "small" {
		t.Fatalf("ceiling ignored: leased %q", job.AssetID)
	}
	if next, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{MaxAssetBytes: 1000}); err != nil || next != nil {
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
	released, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
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
	job, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease job=%+v err=%v", job, err)
	}
	if err := repo.CompleteJob(ctx, job.ID, "worker", domain.JobFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	next, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if next == nil {
		t.Fatal("expected CompleteJob to leave the job leasable; if this now fails the lease predicate changed and FailJobTerminally may be redundant")
	}
}

// A deferred job is 'pending', so RequeueFailedJobs cannot reach it and an
// operator who has topped up their account would otherwise wait out the full
// five hours. Resuming must make it leasable immediately and must leave work
// postponed for any other reason alone.
func TestResumeDeferredJobsReleasesOnlyQuotaWaitsAndLeasesImmediately(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "jobs-resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-resume','resume-fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-resume", domain.JobAnalyze, "resume-deferred", 10); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-resume", domain.JobProbe, "resume-backoff", 5); err != nil {
		t.Fatal(err)
	}

	deferred, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
	if err != nil || deferred == nil {
		t.Fatalf("lease deferred job: %+v %v", deferred, err)
	}
	if err := repo.DeferJob(ctx, deferred.ID, "worker", time.Now().Add(5*time.Hour), domain.JobDeferProviderRouteExhausted, "every key failing"); err != nil {
		t.Fatal(err)
	}
	backoff, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
	if err != nil || backoff == nil {
		t.Fatalf("lease backoff job: %+v %v", backoff, err)
	}
	if err := repo.RetryJob(ctx, backoff.ID, "worker", domain.JobFailureCategoryProviderUnavailable, "transient", time.Hour); err != nil {
		t.Fatal(err)
	}
	if job, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{}); err != nil || job != nil {
		t.Fatalf("nothing should be due yet, got %+v %v", job, err)
	}

	resumed, err := repo.ResumeDeferredJobs(ctx, domain.JobDeferProviderRouteExhausted)
	if err != nil {
		t.Fatal(err)
	}
	if resumed != 1 {
		t.Fatalf("resumed %d jobs, want exactly the quota wait", resumed)
	}

	got, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
	if err != nil || got == nil {
		t.Fatalf("resumed job did not become leasable: %+v %v", got, err)
	}
	if got.ID != deferred.ID {
		t.Fatalf("leased %s, want the resumed quota wait %s", got.ID, deferred.ID)
	}
	// The ordinary backoff must still be waiting out its hour.
	if job, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{}); err != nil || job != nil {
		t.Fatalf("resume disturbed an unrelated backoff: %+v %v", job, err)
	}
	if resumed, err := repo.ResumeDeferredJobs(ctx, domain.JobDeferProviderRouteExhausted); err != nil || resumed != 0 {
		t.Fatalf("second resume moved %d jobs, want 0 (reason code must be cleared)", resumed)
	}
}
