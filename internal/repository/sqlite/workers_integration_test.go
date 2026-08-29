package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
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

// TestWorkerPairingValidDoesNotRedeem proves WorkerPairingValid is a pure read:
// it reports whether a raw token is unredeemed and unexpired, and repeated
// checks never consume it, so the bootstrap binary download can authorize
// itself repeatedly while the credential survives for the single enrollment
// that redeems it.
func TestWorkerPairingValidDoesNotRedeem(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "workers-pairing-valid.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()

	if valid, err := repo.WorkerPairingValid(ctx, "", now); err != nil || valid {
		t.Fatalf("empty token valid=%v err=%v, want false", valid, err)
	}
	if valid, err := repo.WorkerPairingValid(ctx, "not-a-real-token", now); err != nil || valid {
		t.Fatalf("unknown token valid=%v err=%v, want false", valid, err)
	}

	pairing, err := repo.CreateWorkerPairing(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Repeated validity checks must not consume the token.
	for i := range 3 {
		valid, err := repo.WorkerPairingValid(ctx, pairing.Token, now)
		if err != nil {
			t.Fatal(err)
		}
		if !valid {
			t.Fatalf("check %d: valid pairing token reported invalid", i)
		}
	}

	// The token expires and the check flips to false.
	expired, err := repo.CreateWorkerPairing(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE worker_pairing_tokens SET expires_at=? WHERE token_hash=?`, formatTime(now.Add(-time.Minute)), tokenDigest(expired.Token)); err != nil {
		t.Fatal(err)
	}
	if valid, err := repo.WorkerPairingValid(ctx, expired.Token, now); err != nil || valid {
		t.Fatalf("expired token valid=%v err=%v, want false", valid, err)
	}

	// Enrollment redeems the token exactly once and the check turns false.
	if _, _, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: "validator", Platform: "linux-amd64"}); err != nil {
		t.Fatal(err)
	}
	if valid, err := repo.WorkerPairingValid(ctx, pairing.Token, now); err != nil || valid {
		t.Fatalf("redeemed token valid=%v err=%v, want false", valid, err)
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

func TestHeartbeatWorkerPreservesEnrolledProviderOperations(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "workers-capabilities.db"))
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
	enrolled, _, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{
		Name: "worker", Platform: "linux-amd64",
		Capabilities: remote.WorkerCapabilities{ProviderOperations: []string{"video_analysis"}, LibraryRoots: []string{"root-a"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	heartbeat := remote.WorkerCapabilities{Proxy: true, Thumbnail: true, AudioExtract: true, Hardware: []string{"nvenc"}, MaxParallelProxyJobs: 3, LibraryRoots: []string{"root-b"}}
	if err := repo.HeartbeatWorker(ctx, enrolled.ID, "", heartbeat); err != nil {
		t.Fatal(err)
	}
	workers, err := repo.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 {
		t.Fatalf("workers=%+v", workers)
	}
	got := workers[0].Capabilities
	if len(got.ProviderOperations) != 1 || got.ProviderOperations[0] != "video_analysis" {
		t.Fatalf("nil heartbeat operations changed enrollment trust: %v", got.ProviderOperations)
	}
	if !got.Proxy || !got.Thumbnail || !got.AudioExtract || got.MaxParallelProxyJobs != 3 || len(got.Hardware) != 1 || got.Hardware[0] != "nvenc" || len(got.LibraryRoots) != 1 || got.LibraryRoots[0] != "root-b" {
		t.Fatalf("heartbeat fields were not accepted: %+v", got)
	}

	if err := repo.HeartbeatWorker(ctx, enrolled.ID, "", remote.WorkerCapabilities{ProviderOperations: []string{"asr"}}); err != nil {
		t.Fatal(err)
	}
	workers, err = repo.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got = workers[0].Capabilities; len(got.ProviderOperations) != 1 || got.ProviderOperations[0] != "video_analysis" {
		t.Fatalf("heartbeat escalated provider operations: %v", got.ProviderOperations)
	}
}

// WORKER-001: revoking a worker must make its issued token stop authenticating
// (status='revoked' + cleared token hash), be idempotent, and answer
// domain.ErrWorkerNotFound for an id that names nothing.
func TestRevokeWorkerRejectsTokenAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "workers-revoke.db"))
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
		Name: "revoke-me", Platform: "linux-amd64",
		Capabilities: remote.WorkerCapabilities{Proxy: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := repo.AuthenticateWorker(ctx, token); err != nil {
		t.Fatalf("token must authenticate before revocation: %v", err)
	}

	if err := repo.RevokeWorker(ctx, enrolled.ID); err != nil {
		t.Fatalf("RevokeWorker: %v", err)
	}
	// The issued token must no longer authenticate.
	if _, err := repo.AuthenticateWorker(ctx, token); err == nil {
		t.Fatal("revoked worker token still authenticates")
	}
	// The row's status must be revoked (and stay revoked across ListWorkers).
	workers, err := repo.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].Status != remote.WorkerRevoked {
		t.Fatalf("revoked worker status=%q, want %q", workers[0].Status, remote.WorkerRevoked)
	}

	// Revoking the same id again is a no-op success.
	if err := repo.RevokeWorker(ctx, enrolled.ID); err != nil {
		t.Fatalf("revoking an already-revoked worker should be idempotent: %v", err)
	}

	// An unknown id reports not-found.
	if err := repo.RevokeWorker(ctx, "no-such-worker"); !errors.Is(err, domain.ErrWorkerNotFound) {
		t.Fatalf("RevokeWorker(unknown) = %v, want domain.ErrWorkerNotFound", err)
	}
}
