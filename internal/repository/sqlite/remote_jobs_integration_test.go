package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/remote"
)

func TestLeaseNextWorkerDeriveMatchesLibraryRootAndCapability(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "remote-jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-allowed','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-derive','derive-fp',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc-derive','asset-derive','root-allowed','nested/clip.mp4','/footage/nested/clip.mp4',123,1,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-derive", domain.JobDerive, "derive-input", 90); err != nil {
		t.Fatal(err)
	}

	wrongRoot := remote.Worker{
		ID: "worker-wrong-root",
		Capabilities: remote.WorkerCapabilities{
			Proxy:        true,
			Thumbnail:    true,
			LibraryRoots: []string{"root-other"},
		},
	}
	if leased, err := repo.LeaseNextWorkerDerive(ctx, wrongRoot, time.Minute, domain.LeaseFilter{}); err != nil {
		t.Fatal(err)
	} else if leased != nil {
		t.Fatalf("wrong-root worker leased job: %+v", leased)
	}

	matching := remote.Worker{
		ID: "worker-matching",
		Capabilities: remote.WorkerCapabilities{
			Proxy:        true,
			Thumbnail:    true,
			LibraryRoots: []string{"root-allowed"},
		},
	}
	leased, err := repo.LeaseNextWorkerDerive(ctx, matching, time.Minute, domain.LeaseFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if leased == nil {
		t.Fatal("matching worker did not lease derive job")
	}
	if leased.JobID != "" && leased.AssetID != "asset-derive" {
		t.Fatalf("leased job has unexpected asset: %+v", leased)
	}
	if leased.JobID == "" || leased.RootID != "root-allowed" || leased.RelativePath != "nested/clip.mp4" || leased.ModifiedNS != 123 || leased.JobType != domain.JobDerive {
		t.Fatalf("leased job has unexpected source details: %+v", leased)
	}

	if leasedAgain, err := repo.LeaseNextWorkerDerive(ctx, matching, time.Minute, domain.LeaseFilter{}); err != nil {
		t.Fatal(err)
	} else if leasedAgain != nil {
		t.Fatalf("worker leased the same active job twice: %+v", leasedAgain)
	}

	var state, owner string
	var attempts int
	if err := repo.db.QueryRowContext(ctx, `SELECT state,COALESCE(lease_owner,''),attempt_count FROM jobs WHERE id=?`, leased.JobID).Scan(&state, &owner, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.JobRunning) || owner != matching.ID || attempts != 1 {
		t.Fatalf("job lease state=%q owner=%q attempts=%d", state, owner, attempts)
	}
}

func TestLeaseNextWorkerDeriveUsesEligibleRootLocalAlternate(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "remote-local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at,health_state) VALUES('root-global','/global',?,?, 'healthy'),('root-local','/local',?,?, 'healthy')`, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-local','local-fp',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc-global','asset-local','root-global','global.mp4','/global/global.mp4',1,1,1,?),('loc-local','asset-local','root-local','local.mp4','/local/local.mp4',2,1,0,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-local", domain.JobDerive, "derive-local", 90); err != nil {
		t.Fatal(err)
	}
	worker := remote.Worker{ID: "worker-local", Capabilities: remote.WorkerCapabilities{Proxy: true, Thumbnail: true, LibraryRoots: []string{"root-local"}}}
	job, err := repo.LeaseNextWorkerDerive(ctx, worker, time.Minute, domain.LeaseFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if job == nil {
		t.Fatal("local worker did not lease job")
	}
	if job.RootID != "root-local" || job.RelativePath != "local.mp4" {
		t.Fatalf("leased non-local location: %+v", job)
	}
}

func TestCompleteWorkerDeriveEnqueuesAnalysisWhenNoAudioArtifactExists(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "worker-complete.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-complete','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-complete','complete-fp',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc-complete','asset-complete','root-complete','clip.mp4','/footage/clip.mp4',1,1,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-complete", domain.JobDerive, "derive-input", 90); err != nil {
		t.Fatal(err)
	}
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	worker, _, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: "completion-worker", Platform: "linux", Capabilities: remote.WorkerCapabilities{Proxy: true, LibraryRoots: []string{"root-complete"}}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextWorkerDerive(ctx, worker, time.Minute, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease=%+v err=%v", job, err)
	}
	if err := repo.CompleteWorkerJob(ctx, job.JobID, worker.ID, domain.JobSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	jobs, err := repo.ListJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range jobs {
		if candidate.Type == domain.JobAnalyze && candidate.State == domain.JobPending {
			return
		}
	}
	t.Fatalf("worker derive completion did not enqueue analyze: %+v", jobs)
}
