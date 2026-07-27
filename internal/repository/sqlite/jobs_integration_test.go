package sqlite

import (
	"context"
	"path/filepath"
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
	job, err := repo.LeaseNextJob(ctx, "worker", time.Minute)
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
	if next, err := repo.LeaseNextJob(ctx, "worker", time.Minute); err != nil || next != nil {
		t.Fatalf("backoff job must not be immediately leased: job=%+v err=%v", next, err)
	}
}
