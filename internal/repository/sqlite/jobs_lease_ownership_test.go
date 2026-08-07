package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// leaseAndExpire leases id under owner with a lease so short it is already
// expired by the time the caller can act on it, giving a deterministic way to
// reach the state LeaseNextJob's "OR (state='running' AND
// lease_expires_at<=?)" reclaim arm exists for, without waiting out a real
// 2-minute Hub-local lease.
func leaseAndExpire(t *testing.T, ctx context.Context, repo *Repository, owner string) *domain.Job {
	t.Helper()
	job, err := repo.LeaseNextJob(ctx, owner, func(domain.JobType) time.Duration { return time.Millisecond }, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease as %s: job=%+v err=%v", owner, job, err)
	}
	time.Sleep(20 * time.Millisecond)
	return job
}

// jobLeaseRow is a raw read of the columns the ownership predicates guard,
// bypassing every repository method under test so the assertions below do
// not depend on the correctness of the thing they are checking.
type jobLeaseRow struct {
	state        string
	leaseOwner   string
	attemptCount int
	terminal     int
	lastError    string
	runAfter     string
}

func readJobLeaseRow(t *testing.T, ctx context.Context, repo *Repository, id string) jobLeaseRow {
	t.Helper()
	var row jobLeaseRow
	var owner, lastErr *string
	if err := repo.db.QueryRowContext(ctx, `SELECT state,lease_owner,attempt_count,terminal,last_error_message,run_after FROM jobs WHERE id=?`, id).
		Scan(&row.state, &owner, &row.attemptCount, &row.terminal, &lastErr, &row.runAfter); err != nil {
		t.Fatalf("read job %s: %v", id, err)
	}
	if owner != nil {
		row.leaseOwner = *owner
	}
	if lastErr != nil {
		row.lastError = *lastErr
	}
	return row
}

// This is the defect this test exists to catch: LeaseNextJob's acquisition is
// a correct compare-and-swap (the widened reclaim predicate that made an
// expired lease reclaimable re-checks expiry at UPDATE time, not just at
// SELECT time), but CompleteJob/FailJobTerminally/RetryJob/DeferJob had no
// ownership predicate at all before this change -- any caller holding a job
// ID could write to it regardless of whether its lease was still current.
// Reachable, not theoretical: the Hub-local pipeline leases for a fixed,
// never-renewed 2 minutes and runs synchronous work that routinely exceeds
// it, so a paired Worker or a second Hub process can and does reclaim a job
// out from under a still-running local execution.
func TestStaleLeaseHolderCannotWriteAJobReclaimedByAnotherOwner(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "lease-ownership.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-lease','lease-fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-lease", domain.JobAnalyze, "lease-input", 10); err != nil {
		t.Fatal(err)
	}

	original := leaseAndExpire(t, ctx, repo, "owner-a")

	// The reclaim: owner-b takes the same row through the exact path a paired
	// Worker or a second `timingdex serve`/`pipeline run` process would.
	reclaimed, err := repo.LeaseNextJob(ctx, "owner-b", nil, domain.LeaseFilter{})
	if err != nil || reclaimed == nil {
		t.Fatalf("reclaim as owner-b: job=%+v err=%v", reclaimed, err)
	}
	if reclaimed.ID != original.ID {
		t.Fatalf("reclaim leased a different job: got %s want %s", reclaimed.ID, original.ID)
	}
	if reclaimed.AttemptCount != 2 {
		t.Fatalf("reclaim must still increment attempt_count like any other lease: %+v", reclaimed)
	}

	baseline := readJobLeaseRow(t, ctx, repo, original.ID)
	if baseline.leaseOwner != "owner-b" || baseline.state != string(domain.JobRunning) {
		t.Fatalf("setup did not leave the job owned by B: %+v", baseline)
	}

	assertUnaffectedByA := func(t *testing.T, label string, callErr error) {
		t.Helper()
		if callErr == nil {
			t.Fatalf("%s: stale owner-a call must fail once owner-b holds the lease", label)
		}
		// errors.Is against the sentinel, not a message substring: this is the
		// assertion that survives leaseLostErr's text being reworded, which a
		// substring check would not (see domain.ErrJobLeaseLost's doc comment).
		if !errors.Is(callErr, domain.ErrJobLeaseLost) {
			t.Fatalf("%s: error does not wrap domain.ErrJobLeaseLost: %v", label, callErr)
		}
		after := readJobLeaseRow(t, ctx, repo, original.ID)
		if after != baseline {
			t.Fatalf("%s: A's write affected the row despite losing the lease: before=%+v after=%+v", label, baseline, after)
		}
	}

	t.Run("CompleteJob", func(t *testing.T) {
		assertUnaffectedByA(t, "CompleteJob", repo.CompleteJob(ctx, original.ID, "owner-a", domain.JobSucceeded, "stale success"))
	})
	t.Run("FailJobTerminally", func(t *testing.T) {
		assertUnaffectedByA(t, "FailJobTerminally", repo.FailJobTerminally(ctx, original.ID, "owner-a", "stale permanent failure"))
	})
	t.Run("RetryJob", func(t *testing.T) {
		assertUnaffectedByA(t, "RetryJob", repo.RetryJob(ctx, original.ID, "owner-a", "stale retry", time.Second))
	})
	t.Run("DeferJob", func(t *testing.T) {
		assertUnaffectedByA(t, "DeferJob", repo.DeferJob(ctx, original.ID, "owner-a", time.Now().Add(time.Hour), domain.JobDeferProviderRouteExhausted, "stale defer"))
	})

	// The contrast: owner-b, the actual holder, can still do all four. This is
	// what proves the predicate checks ownership rather than merely rejecting
	// every write -- a bug that made every completion fail would also make
	// these stale-owner assertions pass for the wrong reason.
	if err := repo.CompleteJob(ctx, original.ID, "owner-b", domain.JobSucceeded, ""); err != nil {
		t.Fatalf("legitimate holder must still be able to complete the job: %v", err)
	}
	final := readJobLeaseRow(t, ctx, repo, original.ID)
	if final.state != string(domain.JobSucceeded) || final.leaseOwner != "" {
		t.Fatalf("legitimate CompleteJob did not take effect: %+v", final)
	}
}

// SaveArtifact is not one of the four completion methods above, but it is
// reachable through the identical race: JobDerive writes a thumbnail, a
// proxy, and (when the source has audio) an audio extract synchronously,
// inside one execution, and any one of those renders can outlive the fixed
// lease just as easily as the completion write that follows them.
func TestStaleLeaseHolderCannotSaveArtifactForAJobReclaimedByAnotherOwner(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "lease-ownership-artifact.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-lease-artifact','lease-artifact-fp',1,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-lease-artifact", domain.JobDerive, "lease-artifact-input", 10); err != nil {
		t.Fatal(err)
	}

	original := leaseAndExpire(t, ctx, repo, "owner-a")
	reclaimed, err := repo.LeaseNextJob(ctx, "owner-b", nil, domain.LeaseFilter{})
	if err != nil || reclaimed == nil || reclaimed.ID != original.ID {
		t.Fatalf("reclaim as owner-b: job=%+v err=%v", reclaimed, err)
	}

	staleArtifact := domain.DerivedArtifact{ID: "artifact-stale", AssetID: "asset-lease-artifact", Type: "proxy", ProfileHash: "proxy-v1", LocalPath: "/derived/stale.mp4", SizeBytes: 42}
	if err := repo.SaveArtifact(ctx, staleArtifact, original.ID, "owner-a"); err == nil {
		t.Fatal("stale owner-a must not be able to save an artifact once owner-b holds the lease")
	} else if !errors.Is(err, domain.ErrJobLeaseLost) {
		// errors.Is, not a message substring -- see domain.ErrJobLeaseLost's
		// doc comment for why the message is free to reword.
		t.Fatalf("error does not wrap domain.ErrJobLeaseLost: %v", err)
	}
	if got, err := repo.GetArtifact(ctx, "asset-lease-artifact", "proxy"); err != nil {
		t.Fatal(err)
	} else if got != nil {
		t.Fatalf("stale write must not have reached the table: %+v", got)
	}

	// The legitimate holder can still write.
	liveArtifact := domain.DerivedArtifact{ID: "artifact-live", AssetID: "asset-lease-artifact", Type: "proxy", ProfileHash: "proxy-v1", LocalPath: "/derived/live.mp4", SizeBytes: 99}
	if err := repo.SaveArtifact(ctx, liveArtifact, original.ID, "owner-b"); err != nil {
		t.Fatalf("legitimate holder must still be able to save an artifact: %v", err)
	}
	got, err := repo.GetArtifact(ctx, "asset-lease-artifact", "proxy")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.LocalPath != "/derived/live.mp4" {
		t.Fatalf("legitimate SaveArtifact did not take effect: %+v", got)
	}
}
