package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

func summaryRepo(t *testing.T) (*Repository, context.Context) {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "job-summary.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-summary','summary-fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	return repo, ctx
}

func TestJobSummaryCountsAnEmptyQueueAsZero(t *testing.T) {
	repo, ctx := summaryRepo(t)
	summary, err := repo.JobSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary != (domain.JobSummary{}) {
		t.Fatalf("empty queue summarised as %+v", summary)
	}
}

// A pending job has never failed, so its last_error_code is NULL. The deferred
// predicate compares that column, and `NULL = ?` is NULL rather than false — so
// a bare comparison, negated for the pending arm, drops every ordinary pending
// job out of the count. That is most of a real queue, which is why it gets its
// own test rather than riding along with the others.
func TestJobSummaryCountsPendingJobsThatHaveNeverFailed(t *testing.T) {
	repo, ctx := summaryRepo(t)
	for _, hash := range []string{"a", "b", "c"} {
		if err := repo.EnqueueJob(ctx, "asset-summary", domain.JobProbe, hash, 10); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := repo.JobSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Pending != 3 || summary.Total != 3 {
		t.Fatalf("summary=%+v, want 3 pending of 3 total", summary)
	}
	if summary.Deferred != 0 {
		t.Fatalf("never-failed jobs counted as deferred: %+v", summary)
	}
}

// The categories must stay disjoint: a parked job is not backlog, and a
// permanently failed one is not something an operator can retry into success.
func TestJobSummarySeparatesDeferredAndTerminalFromTheRest(t *testing.T) {
	repo, ctx := summaryRepo(t)
	for _, hash := range []string{"deferred", "terminal", "running", "waiting"} {
		if err := repo.EnqueueJob(ctx, "asset-summary", domain.JobProbe, hash, 10); err != nil {
			t.Fatal(err)
		}
	}

	parked, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
	if err != nil || parked == nil {
		t.Fatalf("lease for defer: %+v %v", parked, err)
	}
	if err := repo.DeferJob(ctx, parked.ID, "worker", time.Now().Add(5*time.Hour), domain.JobDeferProviderRouteExhausted, "every key failing"); err != nil {
		t.Fatal(err)
	}
	dead, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
	if err != nil || dead == nil {
		t.Fatalf("lease for terminal: %+v %v", dead, err)
	}
	if err := repo.FailJobTerminally(ctx, dead.ID, "worker", "permanent"); err != nil {
		t.Fatal(err)
	}
	live, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
	if err != nil || live == nil {
		t.Fatalf("lease for running: %+v %v", live, err)
	}

	summary, err := repo.JobSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.JobSummary{Pending: 1, Running: 1, Terminal: 1, Deferred: 1, Total: 4}
	if summary != want {
		t.Fatalf("summary=%+v, want %+v", summary, want)
	}
	if summary.Pending+summary.Running+summary.Succeeded+summary.Failed+summary.Terminal+summary.Deferred != summary.Total {
		t.Fatalf("categories do not exhaust the queue: %+v", summary)
	}
}

// The whole reason this endpoint exists: /progress used to count the hundred
// newest rows, and jobs are created newest-last as the chain advances, so a
// queue whose finished work is older than its backlog reported that work as
// absent. The summary must see all of it.
func TestJobSummarySeesPastTheHundredNewestJobs(t *testing.T) {
	repo, ctx := summaryRepo(t)
	const finished = 120
	now := formatTime(time.Now())
	for i := 0; i < finished; i++ {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at) VALUES(?,?,?,'succeeded',10,1,3,?,?,?,?)`,
			"done-"+string(rune('a'+i%26))+formatInt(i), "asset-summary", string(domain.JobProbe), now, "done-hash-"+formatInt(i), now, now); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 100; i++ {
		if err := repo.EnqueueJob(ctx, "asset-summary", domain.JobAnalyze, "fresh-"+formatInt(i), 10); err != nil {
			t.Fatal(err)
		}
	}

	recent, err := repo.ListJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range recent {
		if job.State == domain.JobSucceeded {
			t.Fatalf("fixture is not exercising the bias: a succeeded job is among the newest 100")
		}
	}

	summary, err := repo.JobSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Succeeded != finished {
		t.Fatalf("summary saw %d succeeded, want %d", summary.Succeeded, finished)
	}
	if summary.Pending != 100 || summary.Total != finished+100 {
		t.Fatalf("summary=%+v, want 100 pending of %d", summary, finished+100)
	}
}

func formatInt(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
