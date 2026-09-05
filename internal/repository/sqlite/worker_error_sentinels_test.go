package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

// The Worker surface's refusals used to be bare errors.New, which left
// internal/app classifying an enrollment failure with
// strings.Contains(err.Error(), "pairing token") — the one thing this
// repository forbids everywhere else — and left an unknown job id answering
// 500. Real sqlite.Open + Migrate, because what these functions return on a
// miss is a property of the query, not of a fake.
func TestWorkerRefusalsCarrySentinels(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "worker-sentinels.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	valid := remote.WorkerRegistration{Name: "n", Platform: "linux-amd64"}

	t.Run("unknown pairing token", func(t *testing.T) {
		_, _, err := repo.EnrollWorker(ctx, "not-a-token", valid)
		if !errors.Is(err, domain.ErrPairingTokenInvalid) {
			t.Fatalf("err = %v, want ErrPairingTokenInvalid", err)
		}
	})

	t.Run("redeemed pairing token", func(t *testing.T) {
		pairing, err := repo.CreateWorkerPairing(ctx, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := repo.EnrollWorker(ctx, pairing.Token, valid); err != nil {
			t.Fatal(err)
		}
		_, _, err = repo.EnrollWorker(ctx, pairing.Token, valid)
		if !errors.Is(err, domain.ErrPairingTokenInvalid) {
			t.Fatalf("err = %v, want ErrPairingTokenInvalid", err)
		}
	})

	t.Run("expired pairing token", func(t *testing.T) {
		// CreateWorkerPairing clamps a non-positive TTL to 15 minutes, so the
		// only way to reach the expiry branch is to backdate the row.
		pairing, err := repo.CreateWorkerPairing(ctx, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `UPDATE worker_pairing_tokens SET expires_at=? WHERE token_hash=?`,
			formatTime(time.Now().UTC().Add(-time.Minute)), tokenDigest(pairing.Token)); err != nil {
			t.Fatal(err)
		}
		_, _, err = repo.EnrollWorker(ctx, pairing.Token, valid)
		if !errors.Is(err, domain.ErrPairingTokenInvalid) {
			t.Fatalf("err = %v, want ErrPairingTokenInvalid", err)
		}
		// Expiry keeps its own words in the message even though it shares the
		// sentinel: the operator needs to know a fresh token will work.
		if got := err.Error(); got == domain.ErrPairingTokenInvalid.Error() {
			t.Fatalf("expired token is indistinguishable from an unknown one: %q", got)
		}
	})

	// A valid token with an incomplete registration is the caller describing
	// itself wrongly, not a Hub fault. It must not be reported as an invalid
	// pairing token either — that would send the operator to regenerate a
	// token that was fine.
	for _, tc := range []struct {
		name         string
		registration remote.WorkerRegistration
	}{
		{"no name", remote.WorkerRegistration{Platform: "linux-amd64"}},
		{"no platform", remote.WorkerRegistration{Name: "n"}},
		{"blank name", remote.WorkerRegistration{Name: "   ", Platform: "linux-amd64"}},
	} {
		t.Run("invalid registration: "+tc.name, func(t *testing.T) {
			pairing, err := repo.CreateWorkerPairing(ctx, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = repo.EnrollWorker(ctx, pairing.Token, tc.registration)
			if !errors.Is(err, domain.ErrInvalidWorkerRegistration) {
				t.Fatalf("err = %v, want ErrInvalidWorkerRegistration", err)
			}
			if errors.Is(err, domain.ErrPairingTokenInvalid) {
				t.Fatal("an incomplete registration must not read as a bad pairing token")
			}
		})
	}

	t.Run("unknown worker job", func(t *testing.T) {
		_, err := repo.GetWorkerJobStatus(ctx, "job-that-does-not-exist")
		if !errors.Is(err, domain.ErrWorkerJobNotFound) {
			t.Fatalf("err = %v, want ErrWorkerJobNotFound", err)
		}
	})
}
