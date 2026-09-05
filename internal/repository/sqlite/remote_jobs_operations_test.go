package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

func TestWorkerProgressEventsRequireLeaseOwnershipAndPersist(t *testing.T) {
	ctx := context.Background()
	repo, jobID, owner, other := newRemoteOperationsFixture(t, "progress")

	job, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease job=%+v err=%v", job, err)
	}
	if err := repo.RecordWorkerJobProgress(ctx, jobID, other.ID, "proxy", 25, "progress", "must reject non-owner"); err == nil || !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("non-owner progress error=%v", err)
	}
	if err := repo.RecordWorkerJobProgress(ctx, jobID, owner.ID, "proxy", 42.5, "progress", "encoding"); err != nil {
		t.Fatal(err)
	}

	status, err := repo.GetWorkerJobStatus(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if status.CurrentStage != "proxy" || status.Progress != 42.5 || status.LeaseOwner != owner.ID {
		t.Fatalf("unexpected current status: %+v", status)
	}
	if len(status.Events) < 2 {
		t.Fatalf("expected lease and progress events, got %+v", status.Events)
	}
	last := status.Events[len(status.Events)-1]
	if last.Stage != "proxy" || last.Progress != 42.5 || last.EventType != "progress" || last.WorkerID != owner.ID {
		t.Fatalf("unexpected progress event: %+v", last)
	}
}

func TestWorkerJobStatusShowsRetryAndLastFailure(t *testing.T) {
	ctx := context.Background()
	repo, jobID, owner, _ := newRemoteOperationsFixture(t, "retry")

	job, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease job=%+v err=%v", job, err)
	}
	if err := repo.RetryWorkerJob(ctx, jobID, owner.ID, "proxy", "ffmpeg_timeout", "proxy timed out", 15*time.Second); err != nil {
		t.Fatal(err)
	}

	status, err := repo.GetWorkerJobStatus(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != domain.JobPending || status.LastErrorCode != "ffmpeg_timeout" || status.LastErrorMessage != "proxy timed out" {
		t.Fatalf("retry state not visible: %+v", status)
	}
	if status.LastFailureWorkerID != owner.ID || status.LastFailureStage != "proxy" || status.LastFailureAt.IsZero() {
		t.Fatalf("last failure not visible: %+v", status)
	}
	if !status.RunAfter.After(time.Now().UTC()) {
		t.Fatalf("retry should be delayed: %+v", status.RunAfter)
	}
	if len(status.Events) < 2 || status.Events[len(status.Events)-1].EventType != "retry_scheduled" {
		t.Fatalf("retry event not visible: %+v", status.Events)
	}
}

// TestWorkerFailureBacksOffThenGoesTerminal replaces the old
// TestCompletedWorkerFailureRemainsVisible, which asserted the *buggy*
// behavior: CompleteWorkerJob used to mark a Worker failure 'failed'
// immediately, with no backoff. RetryWorkerJob was written to give it a
// backoff and a retry_scheduled event, but nothing on the wire ever called
// it -- Complete is the only failure endpoint a Worker has -- so a flaky
// Worker re-leased and re-failed the same job up to three times back to back
// with zero delay, then stopped with no distinction from a real permanent
// failure. This asserts the fixed behavior: a failure with attempts
// remaining schedules a delayed retry, and only exhausting every attempt
// goes terminal, while the last-failure fields stay visible throughout.
func TestWorkerFailureBacksOffThenGoesTerminal(t *testing.T) {
	ctx := context.Background()
	repo, jobID, owner, _ := newRemoteOperationsFixture(t, "terminal-failure")

	if _, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteWorkerJob(ctx, jobID, owner.ID, domain.JobFailed, "proxy unavailable"); err != nil {
		t.Fatal(err)
	}
	status, err := repo.GetWorkerJobStatus(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != domain.JobPending {
		t.Fatalf("a failure with attempts remaining must retry, not go terminal: %+v", status)
	}
	if !status.RunAfter.After(time.Now().UTC()) {
		t.Fatalf("retry must be delayed, or a dead Worker immediately re-leases and re-fails: %+v", status)
	}
	if status.LastErrorMessage != "proxy unavailable" || status.LastFailureWorkerID != owner.ID || status.LastFailureAt.IsZero() {
		t.Fatalf("last failure must stay visible through a scheduled retry: %+v", status)
	}

	// Drive the remaining two attempts to exhaustion. run_after is forced
	// into the past between leases so the test does not sleep for real
	// backoff time -- the delay itself is already asserted above.
	for i := 0; i < 2; i++ {
		makeLeasableNow(t, repo, jobID)
		if job, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{}); err != nil || job == nil {
			t.Fatalf("attempt %d: could not re-lease job=%+v err=%v", i, job, err)
		}
		if err := repo.CompleteWorkerJob(ctx, jobID, owner.ID, domain.JobFailed, "proxy unavailable"); err != nil {
			t.Fatal(err)
		}
	}

	status, err = repo.GetWorkerJobStatus(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != domain.JobFailed || status.AttemptCount != status.MaxAttempts {
		t.Fatalf("exhausting every attempt must go terminal: %+v", status)
	}
	if status.LastErrorMessage != "proxy unavailable" || status.LastFailureWorkerID != owner.ID || status.LastFailureAt.IsZero() {
		t.Fatalf("terminal failure not visible: %+v", status)
	}
	var terminal int
	if err := repo.db.QueryRowContext(ctx, `SELECT terminal FROM jobs WHERE id=?`, jobID).Scan(&terminal); err != nil {
		t.Fatal(err)
	}
	if terminal != 1 {
		t.Fatalf("exhausted worker failure must set terminal, or /progress and RequeueFailedJobs cannot tell it apart from a job that still has budget")
	}
}

// TestExpiredWorkerLeaseIsReclaimableAndPreservesAttemptCount is Bug 1 from
// the fix task: a Worker that dies mid-derive used to leave the job at
// state='running' forever. Nothing anywhere transitioned it back to
// leasable, so with more than one Worker online a single crash permanently
// stranded a job with no recovery but manual SQL. attempt_count must survive
// the reclaim (it is not a fresh attempt budget, it is attempt number two).
func TestExpiredWorkerLeaseIsReclaimableAndPreservesAttemptCount(t *testing.T) {
	ctx := context.Background()
	repo, jobID, owner, other := newRemoteOperationsFixture(t, "reclaim")

	job, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("initial lease job=%+v err=%v", job, err)
	}
	if job.AttemptCount != 1 {
		t.Fatalf("expected the first lease to count as attempt 1: %+v", job)
	}

	expireLease(t, repo, jobID)

	reclaimed, err := repo.LeaseNextWorkerDerive(ctx, other, time.Minute, domain.LeaseFilter{})
	if err != nil || reclaimed == nil {
		t.Fatalf("an expired lease must be reclaimable by another worker: job=%+v err=%v", reclaimed, err)
	}
	if reclaimed.AttemptCount != 2 {
		t.Fatalf("reclaiming must count as a new attempt without resetting the old ones: %+v", reclaimed)
	}
	status, err := repo.GetWorkerJobStatus(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if status.LeaseOwner != other.ID {
		t.Fatalf("the reclaiming worker must hold the lease: %+v", status)
	}
}

// TestConcurrentReclaimOfExpiredLeaseHasExactlyOneWinner exercises the CAS
// this fix depends on for correctness: two Workers racing to reclaim the same
// expired lease must never both end up believing they own it, or the same
// asset gets derived twice with both writers thinking they are exclusive.
func TestConcurrentReclaimOfExpiredLeaseHasExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	repo, jobID, owner, other := newRemoteOperationsFixture(t, "concurrent-reclaim")

	if _, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{}); err != nil {
		t.Fatal(err)
	}
	expireLease(t, repo, jobID)

	workers := []remote.Worker{owner, other}
	results := make([]*remote.WorkerJob, len(workers))
	errs := make([]error, len(workers))
	var wg sync.WaitGroup
	wg.Add(len(workers))
	for i := range workers {
		i := i
		go func() {
			defer wg.Done()
			results[i], errs[i] = repo.LeaseNextWorkerDerive(ctx, workers[i], time.Minute, domain.LeaseFilter{})
		}()
	}
	wg.Wait()

	won := 0
	for i, job := range results {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if job != nil {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("exactly one worker must win the reclaimed lease, got %d winners", won)
	}
}

// TestProgressReportExtendsLease is Bug 2: the lease used to be 2 minutes,
// fixed, and never renewed, so a 4K derive that legitimately took longer than
// that lost its lease while the work was still running.
func TestProgressReportExtendsLease(t *testing.T) {
	ctx := context.Background()
	repo, jobID, owner, _ := newRemoteOperationsFixture(t, "renew")

	if _, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{}); err != nil {
		t.Fatal(err)
	}
	before, err := repo.GetWorkerJobStatus(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}

	if err := repo.RecordWorkerJobProgress(ctx, jobID, owner.ID, "derive", 25, "progress", "encoding"); err != nil {
		t.Fatal(err)
	}
	after, err := repo.GetWorkerJobStatus(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LeaseExpiresAt.After(before.LeaseExpiresAt) {
		t.Fatalf("a progress report must extend the lease: before=%v after=%v", before.LeaseExpiresAt, after.LeaseExpiresAt)
	}
	// The renewal has to comfortably outlast FFmpegDeriver.Derive
	// (internal/worker/runtime.go), which reports no progress at all between
	// "derive started" and the artifacts it produces -- so whatever window is
	// set here is what actually has to survive the encode, not just the gap
	// between two progress calls.
	if time.Until(after.LeaseExpiresAt) < 5*time.Minute {
		t.Fatalf("renewal window must comfortably outlast a long encode with no progress signal in between, got %v", time.Until(after.LeaseExpiresAt))
	}
}

// TestLapsedLeaseCannotReportSuccessWithNoArtifacts is Bug 3: unlike
// PrepareWorkerArtifact/CommitWorkerArtifact, CompleteWorkerJob used to check
// lease ownership but not expiry. A Worker whose lease lapsed mid-derive
// could have every artifact upload rejected and then still call
// Complete(Succeeded), recording a "succeeded" derive with nothing behind it
// and letting the pipeline chain (analyze/speech_gate) advance on that lie.
func TestLapsedLeaseCannotReportSuccessWithNoArtifacts(t *testing.T) {
	ctx := context.Background()
	repo, jobID, owner, _ := newRemoteOperationsFixture(t, "lapsed-complete")

	if _, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{}); err != nil {
		t.Fatal(err)
	}
	expireLease(t, repo, jobID)

	if err := repo.CompleteWorkerJob(ctx, jobID, owner.ID, domain.JobSucceeded, ""); err == nil {
		t.Fatal("a lapsed lease must not be able to report success")
	}
	status, err := repo.GetWorkerJobStatus(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State == domain.JobSucceeded {
		t.Fatalf("job must not be recorded succeeded off a lapsed lease: %+v", status)
	}
}

// TestExpiredExhaustedWorkerLeaseGoesTerminalWithoutASweeper reproduces a
// Worker that dies on every single attempt: three lease/expire cycles with no
// completion at all, matching what a container that crashes on startup every
// time looks like from the Hub's side. Once attempt_count reaches
// max_attempts the ordinary reclaim predicate correctly refuses to hand the
// job back out, so without reclaimExhaustedLeases it would sit at
// state='running' with a dead lease forever, invisible to both lease paths
// and to RequeueFailedJobs (which only touches state='failed'). There is
// deliberately no background sweeper for this -- it has to happen inline,
// as a side effect of the very next lease attempt from anyone.
func TestExpiredExhaustedWorkerLeaseGoesTerminalWithoutASweeper(t *testing.T) {
	ctx := context.Background()
	repo, jobID, owner, _ := newRemoteOperationsFixture(t, "exhausted-reclaim")

	for i := 0; i < 3; i++ {
		job, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{})
		if err != nil || job == nil {
			t.Fatalf("attempt %d: could not lease job=%+v err=%v", i, job, err)
		}
		expireLease(t, repo, jobID)
	}

	if job, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{}); err != nil {
		t.Fatal(err)
	} else if job != nil {
		t.Fatalf("a job with no attempts left must not be leased again: %+v", job)
	}

	status, err := repo.GetWorkerJobStatus(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != domain.JobFailed {
		t.Fatalf("exhausted+expired running job must go terminal, not stay stuck running forever: %+v", status)
	}
	var terminal int
	if err := repo.db.QueryRowContext(ctx, `SELECT terminal FROM jobs WHERE id=?`, jobID).Scan(&terminal); err != nil {
		t.Fatal(err)
	}
	if terminal != 1 {
		t.Fatal("terminal flag must be set so /progress and RequeueFailedJobs report it correctly")
	}
}

// TestPinnedRequiredJobNotTakenByHubLocalLease is Bug 4: LeaseNextJob (the
// Hub's own local `pipeline run`) has no worker identity that could ever
// equal a real Worker's ID, so a derive job an admin pinned to one specific
// Worker with WorkerAssignmentRequired must stay invisible to it -- otherwise
// the Hub silently overrides routing intent an operator set explicitly.
func TestPinnedRequiredJobNotTakenByHubLocalLease(t *testing.T) {
	ctx := context.Background()
	repo, jobID, owner, _ := newRemoteOperationsFixture(t, "hub-pin")
	if err := repo.SetDeriveWorkerAssignment(ctx, jobID, owner.ID, remote.WorkerAssignmentRequired); err != nil {
		t.Fatal(err)
	}

	job, err := repo.LeaseNextJob(ctx, "local-"+idgen.New(), nil, domain.LeaseFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if job != nil {
		t.Fatalf("hub-local lease must not steal a job pinned with WorkerAssignmentRequired: %+v", job)
	}

	workerJob, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{})
	if err != nil || workerJob == nil {
		t.Fatalf("the assigned worker must still be able to lease its own job: job=%+v err=%v", workerJob, err)
	}
}

// expireLease simulates a lease that died without ever being renewed or
// completed -- a Worker that crashed mid-derive -- by pushing
// lease_expires_at into the past directly, the same effect real time passing
// would have.
func expireLease(t *testing.T, repo *Repository, jobID string) {
	t.Helper()
	if _, err := repo.db.ExecContext(context.Background(), `UPDATE jobs SET lease_expires_at=? WHERE id=?`, formatTime(time.Now().UTC().Add(-time.Minute)), jobID); err != nil {
		t.Fatal(err)
	}
}

// makeLeasableNow clears a scheduled retry's backoff so a test can drive a
// job through repeated failures without sleeping for real wall-clock delay.
func makeLeasableNow(t *testing.T, repo *Repository, jobID string) {
	t.Helper()
	if _, err := repo.db.ExecContext(context.Background(), `UPDATE jobs SET run_after=? WHERE id=?`, formatTime(time.Now().UTC().Add(-time.Second)), jobID); err != nil {
		t.Fatal(err)
	}
}

func TestDeriveWorkerAssignmentRestrictsLeaseAndPreferenceOrders(t *testing.T) {
	ctx := context.Background()
	repo, jobID, owner, other := newRemoteOperationsFixture(t, "assignment")
	if err := repo.SetDeriveWorkerAssignment(ctx, jobID, owner.ID, remote.WorkerAssignmentRequired); err != nil {
		t.Fatal(err)
	}
	if job, err := repo.LeaseNextWorkerDerive(ctx, other, time.Minute, domain.LeaseFilter{}); err != nil {
		t.Fatal(err)
	} else if job != nil {
		t.Fatalf("required assignment leaked to another worker: %+v", job)
	}
	job, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("assigned worker could not lease job=%+v err=%v", job, err)
	}
	if job.AssignedWorkerID != owner.ID {
		t.Fatalf("assignment missing from leased job: %+v", job)
	}

	preferredID := "preferred-worker"
	if err := repo.SetDeriveWorkerAssignment(ctx, jobID, preferredID, remote.WorkerAssignmentPreferred); err == nil {
		t.Fatal("cannot change assignment on a running job")
	}
}

func TestPreferredWorkerSelectionIsPersistedButRemainsSoft(t *testing.T) {
	ctx := context.Background()
	repo, jobID, owner, preferred := newRemoteOperationsFixture(t, "preferred")
	if err := repo.SetDeriveWorkerAssignment(ctx, jobID, preferred.ID, remote.WorkerAssignmentPreferred); err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("soft preference must not block another capable worker: job=%+v err=%v", job, err)
	}
	if job.PreferredWorkerID != preferred.ID || job.AssignedWorkerID != "" {
		t.Fatalf("unexpected worker selection metadata: %+v", job)
	}
}

func newRemoteOperationsFixture(t *testing.T, suffix string) (*Repository, string, remote.Worker, remote.Worker) {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "remote-operations-"+suffix+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-operations','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	assetID := "asset-operations-" + suffix
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`, assetID, "fp-"+suffix, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,1,1,1,?)`, "loc-"+suffix, assetID, "root-operations", suffix+".mp4", "/footage/"+suffix+".mp4", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "derive-"+suffix, 90); err != nil {
		t.Fatal(err)
	}
	var jobID string
	if err := repo.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE asset_id=? AND job_type=?`, assetID, string(domain.JobDerive)).Scan(&jobID); err != nil {
		t.Fatal(err)
	}

	ownerPairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := repo.EnrollWorker(ctx, ownerPairing.Token, remote.WorkerRegistration{
		Name: "owner-" + suffix, Platform: "linux-amd64",
		Capabilities: remote.WorkerCapabilities{Proxy: true, Thumbnail: true, LibraryRoots: []string{"root-operations"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	otherPairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := repo.EnrollWorker(ctx, otherPairing.Token, remote.WorkerRegistration{
		Name: "other-" + suffix, Platform: "linux-amd64",
		Capabilities: remote.WorkerCapabilities{Proxy: true, Thumbnail: true, LibraryRoots: []string{"root-operations"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return repo, jobID, owner, other
}
