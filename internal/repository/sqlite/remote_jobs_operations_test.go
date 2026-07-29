package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ev/timingdex/internal/domain"
	"github.com/ev/timingdex/internal/remote"
)

func TestWorkerProgressEventsRequireLeaseOwnershipAndPersist(t *testing.T) {
	ctx := context.Background()
	repo, jobID, owner, other := newRemoteOperationsFixture(t, "progress")

	job, err := repo.LeaseNextWorkerDerive(ctx, owner, time.Minute, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease job=%+v err=%v", job, err)
	}
	if err := repo.RecordWorkerJobProgress(ctx, jobID, other.ID, "proxy", 25, "progress", "must reject non-owner"); err == nil || !strings.Contains(err.Error(), "worker does not own active job") {
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

func TestCompletedWorkerFailureRemainsVisible(t *testing.T) {
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
	if status.State != domain.JobFailed || status.LastErrorMessage != "proxy unavailable" || status.LastFailureWorkerID != owner.ID || status.LastFailureAt.IsZero() {
		t.Fatalf("terminal failure not visible: %+v", status)
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
