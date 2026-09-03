package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/remote"
)

func TestCommitWorkerArtifactRequiresActiveLeaseAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "worker-artifacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-upload','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-upload','upload-fp',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc-upload','asset-upload','root-upload','clip.mp4','/footage/clip.mp4',1,1,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-upload", domain.JobDerive, "upload-input", 90); err != nil {
		t.Fatal(err)
	}
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	worker, _, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{
		Name:     "upload-worker",
		Platform: "linux-amd64",
		Capabilities: remote.WorkerCapabilities{
			Proxy:        true,
			Thumbnail:    true,
			LibraryRoots: []string{"root-upload"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextWorkerDerive(ctx, worker, time.Minute, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease job=%+v err=%v", job, err)
	}

	unauthorized := domain.DerivedArtifact{ID: "artifact-unauthorized", AssetID: job.AssetID, Type: "thumbnail", ProfileHash: "thumb-v1", LocalPath: "/derived/unauthorized.jpg", SizeBytes: 5}
	if _, _, err := repo.CommitWorkerArtifact(ctx, job.JobID, "another-worker", unauthorized, false); err == nil || !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("unauthorized commit err=%v", err)
	}

	first := domain.DerivedArtifact{ID: "artifact-first", AssetID: job.AssetID, Type: "thumbnail", ProfileHash: "thumb-v1", LocalPath: "/derived/first.jpg", SizeBytes: 5}
	stored, reused, err := repo.CommitWorkerArtifact(ctx, job.JobID, worker.ID, first, false)
	if err != nil {
		t.Fatal(err)
	}
	if reused || stored.LocalPath != first.LocalPath {
		t.Fatalf("first commit stored=%+v reused=%t", stored, reused)
	}

	second := domain.DerivedArtifact{ID: "artifact-second", AssetID: job.AssetID, Type: "thumbnail", ProfileHash: "thumb-v1", LocalPath: "/derived/second.jpg", SizeBytes: 99}
	stored, reused, err = repo.CommitWorkerArtifact(ctx, job.JobID, worker.ID, second, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reused || stored.ID != first.ID || stored.LocalPath != first.LocalPath || stored.SizeBytes != first.SizeBytes {
		t.Fatalf("duplicate commit damaged existing artifact: stored=%+v reused=%t", stored, reused)
	}
}
