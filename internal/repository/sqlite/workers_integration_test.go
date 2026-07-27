package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ev/timingdex/internal/remote"
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
