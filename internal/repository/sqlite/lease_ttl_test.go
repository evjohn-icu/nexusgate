package sqlite

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// openTestRepo opens a fresh in-memory-on-disk repository for tests in this
// package that need real SQLite.
func openTestRepo(t *testing.T) *Repository {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "ttl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return repo
}

// seedAsset inserts the minimum asset row the jobs foreign key requires.
func seedAsset(t *testing.T, repo *Repository, i int) string {
	t.Helper()
	ctx := context.Background()
	id := "asset-ttl-" + itoa(i)
	now := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`, id, "fp-"+id, now, now); err != nil {
		t.Fatalf("insert asset %s: %v", id, err)
	}
	return id
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// TestLeaseAppliesStageSpecificTTL pins the per-stage lease ceilings: a fixed
// short lease is what lets a second executor reclaim expensive work (derive
// encodes, windowed analysis, ASR) mid-flight and burn the same paid calls
// twice. Each stage must get the ceiling its work can plausibly outlive.
func TestLeaseAppliesStageSpecificTTL(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	ttlFor := func(typ domain.JobType) time.Duration {
		switch typ {
		case domain.JobProbe:
			return 2 * time.Minute
		case domain.JobDerive:
			return 30 * time.Minute
		case domain.JobTranscribe:
			return 15 * time.Minute
		case domain.JobAnalyze:
			return 20 * time.Minute
		}
		return 2 * time.Minute
	}
	cases := []struct {
		typ domain.JobType
		ttl time.Duration
	}{
		{domain.JobProbe, 2 * time.Minute},
		{domain.JobDerive, 30 * time.Minute},
		{domain.JobTranscribe, 15 * time.Minute},
		{domain.JobAnalyze, 20 * time.Minute},
		{domain.JobIndex, 2 * time.Minute},
	}
	for i, c := range cases {
		if err := repo.EnqueueJob(ctx, seedAsset(t, repo, i), c.typ, "hash-"+string(c.typ), 50); err != nil {
			t.Fatalf("enqueue %s: %v", c.typ, err)
		}
	}
	for _, c := range cases {
		job, err := repo.LeaseNextJob(ctx, "ttl-test", ttlFor, domain.LeaseFilter{})
		if err != nil || job == nil {
			t.Fatalf("lease %s: job=%+v err=%v", c.typ, job, err)
		}
		got := leaseExpiry(t, ctx, repo, job.ID)
		diff := math.Abs(got.Sub(time.Now().Add(c.ttl)).Seconds())
		if diff > 1.0 {
			t.Errorf("%s: lease expiry %v, want within 1s of now+%v (diff %0.2fs)", c.typ, got, c.ttl, diff)
		}
		if err := repo.CompleteJob(ctx, job.ID, "ttl-test", domain.JobSucceeded, ""); err != nil {
			t.Fatalf("complete %s: %v", c.typ, err)
		}
	}
}

// TestStageTTLJobNotReclaimedBeforeExpiry: a long-TTL lease must keep a second
// holder out until the ceiling passes; the whole point of the TTL is that the
// expensive work is not duplicated while it is plausibly still running.
func TestStageTTLJobNotReclaimedBeforeExpiry(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	if err := repo.EnqueueJob(ctx, seedAsset(t, repo, 0), domain.JobAnalyze, "hash-a", 50); err != nil {
		t.Fatal(err)
	}
	ttlFor := func(domain.JobType) time.Duration { return 20 * time.Minute }
	job, err := repo.LeaseNextJob(ctx, "owner-a", ttlFor, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease as owner-a: job=%+v err=%v", job, err)
	}
	if next, err := repo.LeaseNextJob(ctx, "owner-b", ttlFor, domain.LeaseFilter{}); err != nil || next != nil {
		t.Fatalf("live long-TTL lease reclaimed: next=%+v err=%v", next, err)
	}
}

// TestStageTTLJobReclaimedAfterExpiry: the same job IS reclaimable once the
// ceiling passes — the TTL is a heuristic ceiling, not a guarantee, and a
// genuinely dead holder must not block the queue forever.
func TestStageTTLJobReclaimedAfterExpiry(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	if err := repo.EnqueueJob(ctx, seedAsset(t, repo, 0), domain.JobAnalyze, "hash-a", 50); err != nil {
		t.Fatal(err)
	}
	ttlFor := func(domain.JobType) time.Duration { return 20 * time.Minute }
	job, err := repo.LeaseNextJob(ctx, "owner-a", ttlFor, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease as owner-a: job=%+v err=%v", job, err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=? WHERE id=?`, formatTime(time.Now().Add(-time.Minute)), job.ID); err != nil {
		t.Fatal(err)
	}
	next, err := repo.LeaseNextJob(ctx, "owner-b", ttlFor, domain.LeaseFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if next == nil || next.ID != job.ID {
		t.Fatalf("expired lease not reclaimed: next=%+v", next)
	}
}

// leaseExpiry reads back the stored lease_expires_at, bypassing repository
// methods so the assertion does not depend on the code under test.
func leaseExpiry(t *testing.T, ctx context.Context, repo *Repository, id string) time.Time {
	t.Helper()
	var raw string
	if err := repo.db.QueryRowContext(ctx, `SELECT lease_expires_at FROM jobs WHERE id=?`, id).Scan(&raw); err != nil {
		t.Fatalf("read lease expiry for %s: %v", id, err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("parse lease expiry %q: %v", raw, err)
	}
	return parsed
}
