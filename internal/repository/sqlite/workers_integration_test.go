package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/remote"
)

func TestWorkerPairingTokenCanEnrollOnlyOnce(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "workers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	pairing, err := repo.CreateWorkerPairing(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	registration := remote.WorkerRegistration{
		Name:         "studio-windows",
		Platform:     "windows-amd64",
		Capabilities: remote.WorkerCapabilities{Proxy: true, MaxParallelProxyJobs: 1, LibraryRoots: []string{"root-a"}},
	}
	worker, token, err := repo.EnrollWorker(ctx, pairing.Token, registration)
	if err != nil {
		t.Fatal(err)
	}
	if worker.ID == "" || token == "" || worker.Status != remote.WorkerOnline {
		t.Fatalf("unexpected enrollment: worker=%+v token=%q", worker, token)
	}
	if _, _, err := repo.EnrollWorker(ctx, pairing.Token, registration); err == nil {
		t.Fatal("reusing a pairing token should fail")
	}
	if _, err := repo.AuthenticateWorker(ctx, token); err != nil {
		t.Fatalf("issued worker token should authenticate: %v", err)
	}
}

// A Worker binary upgrade must be visible on the Hub without re-enrolling:
// the heartbeat carries the running binary's version and the row follows it.
// An old binary that never sends a version must not erase what was recorded.
func TestHeartbeatWorkerPersistsVersion(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "workers-version.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	pairing, err := repo.CreateWorkerPairing(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, token, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{
		Name: "linux", Platform: "linux-amd64", Version: "v0.29.0",
		Capabilities: remote.WorkerCapabilities{Proxy: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if enrolled.Version != "v0.29.0" {
		t.Fatalf("enrollment version=%q, want v0.29.0", enrolled.Version)
	}

	if err := repo.HeartbeatWorker(ctx, enrolled.ID, "v0.30.0", remote.WorkerCapabilities{Proxy: true}); err != nil {
		t.Fatal(err)
	}
	workers, err := repo.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].Version != "v0.30.0" {
		t.Fatalf("heartbeat must update version, workers=%+v", workers)
	}

	if err := repo.HeartbeatWorker(ctx, enrolled.ID, "", remote.WorkerCapabilities{Proxy: true}); err != nil {
		t.Fatal(err)
	}
	workers, err = repo.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].Version != "v0.30.0" {
		t.Fatalf("an empty version must not erase the stored one, workers=%+v", workers)
	}

	authenticated, err := repo.AuthenticateWorker(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.Version != "v0.30.0" {
		t.Fatalf("authenticated worker version=%q, want v0.30.0", authenticated.Version)
	}
}
