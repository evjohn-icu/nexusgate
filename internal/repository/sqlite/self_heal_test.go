package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
)

func TestHealStaleRunningJobs(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "self-heal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stamp := formatTime(now)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-heal','heal-fp',1,'discovered',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	seed := func(id string, state domain.JobState, attempts int, leaseExpires time.Time) {
		t.Helper()
		lease := sql.NullString{String: formatTime(leaseExpires), Valid: true}
		owner := sql.NullString{String: "owner-" + id, Valid: true}
		if state == domain.JobPending {
			lease = sql.NullString{Valid: false}
			owner = sql.NullString{Valid: false}
		}
		_, err := repo.db.ExecContext(ctx, `INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,lease_owner,lease_expires_at,created_at,updated_at) VALUES(?,?,'analyze',?,10,?,3,?,?,?,?,?,?)`,
			id, "asset-heal", string(state), attempts, formatTime(now.Add(-time.Hour)), "hash-"+id, owner, lease, stamp, stamp)
		if err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	// attempts below max with an expired lease: released, budget preserved.
	seed("job-released", domain.JobRunning, 1, now.Add(-time.Hour))
	// attempts exhausted with an expired lease: terminal, and the real
	// failure diagnostic must survive the sweep — the generic message is
	// only the fallback, never a replacement for what actually went wrong.
	seed("job-terminal", domain.JobRunning, 3, now.Add(-time.Hour))
	if _, err := repo.db.ExecContext(ctx, `UPDATE jobs SET last_error_code='provider_auth',last_error_message='real failure detail' WHERE id='job-terminal'`); err != nil {
		t.Fatal(err)
	}
	// live lease: never touched, whatever the attempt count.
	seed("job-live", domain.JobRunning, 1, now.Add(time.Hour))
	// pending (NULL lease): untouched — a NULL lease_expires_at must not
	// compare as expired (NULL <= x is NULL in SQL, and the predicate's
	// IS NOT NULL keeps it explicit).
	seed("job-pending", domain.JobPending, 0, time.Time{})

	released, terminal, err := repo.HealStaleRunningJobs(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if released != 1 || terminal != 1 {
		t.Fatalf("released=%d terminal=%d, want 1/1", released, terminal)
	}

	check := func(id string, wantState domain.JobState, wantAttempts, wantTerminal int, wantLeaseOwner, wantLeaseExpiry bool) {
		t.Helper()
		var state string
		var attempts, terminalFlag int
		var owner, expiry sql.NullString
		var lastErrCode, lastErrMsg sql.NullString
		if err := repo.db.QueryRowContext(ctx, `SELECT state,attempt_count,terminal,lease_owner,lease_expires_at,COALESCE(last_error_code,''),COALESCE(last_error_message,'') FROM jobs WHERE id=?`, id).Scan(&state, &attempts, &terminalFlag, &owner, &expiry, &lastErrCode, &lastErrMsg); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if domain.JobState(state) != wantState || attempts != wantAttempts || terminalFlag != wantTerminal {
			t.Fatalf("%s = state=%s attempts=%d terminal=%d, want %s/%d/%d", id, state, attempts, terminalFlag, wantState, wantAttempts, wantTerminal)
		}
		if owner.Valid != wantLeaseOwner || expiry.Valid != wantLeaseExpiry {
			t.Fatalf("%s lease owner.valid=%v expiry.valid=%v, want %v/%v", id, owner.Valid, expiry.Valid, wantLeaseOwner, wantLeaseExpiry)
		}
		returned := lastErrCode.String + "/" + lastErrMsg.String
		if id == "job-terminal" && returned != "provider_auth/real failure detail" {
			t.Fatalf("%s terminal error = %q, want the preserved diagnostic provider_auth/real failure detail", id, returned)
		}
		if wantState == domain.JobFailed && id != "job-terminal" && returned != "/lease expired with attempts exhausted" {
			t.Fatalf("%s terminal error = %q, want generic /lease expired with attempts exhausted", id, returned)
		}
	}

	check("job-released", domain.JobPending, 1, 0, false, false)
	check("job-terminal", domain.JobFailed, 3, 1, false, false)
	check("job-live", domain.JobRunning, 1, 0, true, true)
	check("job-pending", domain.JobPending, 0, 0, false, false)
}

// A second sweep is a no-op: released rows are 'pending' with a NULL lease
// and terminal rows are out of the predicate, so repeated startups (or the
// same startup running the sweep twice) never double-release.
func TestHealStaleRunningJobsIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "self-heal-idem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stamp := formatTime(now)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-heal2','heal2-fp',1,'discovered',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	_, err = repo.db.ExecContext(ctx, `INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,lease_owner,lease_expires_at,created_at,updated_at) VALUES(?,?,'analyze','running',10,1,3,?,?,?,?,?,?)`,
		idgen.New(), "asset-heal2", formatTime(now.Add(-time.Hour)), "hash-idem", "owner", formatTime(now.Add(-time.Minute)), stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	if released, terminal, err := repo.HealStaleRunningJobs(ctx, now); err != nil || released != 1 || terminal != 0 {
		t.Fatalf("first sweep released=%d terminal=%d err=%v, want 1/0/nil", released, terminal, err)
	}
	if released, terminal, err := repo.HealStaleRunningJobs(ctx, now); err != nil || released != 0 || terminal != 0 {
		t.Fatalf("second sweep released=%d terminal=%d err=%v, want 0/0/nil", released, terminal, err)
	}
}

// A Hub-local job whose lease owner has no live executor is a crash orphan and
// is released even though its lease is still valid — that is the restart
// case, where the old process's derive/transcribe/analyze leases would
// otherwise stall the queue for the whole TTL. A live executor's row keeps its
// jobs in place whatever the lease age.
func TestHealStaleRunningJobsReclaimsDeadLocalExecutor(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "self-heal-exec.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stamp := formatTime(now)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-exec','exec-fp',1,'discovered',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	seed := func(id, owner string) {
		t.Helper()
		_, err := repo.db.ExecContext(ctx, `INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,lease_owner,lease_expires_at,created_at,updated_at) VALUES(?,?,'derive','running',10,1,3,?,?,?,?,?,?)`,
			id, "asset-exec", formatTime(now.Add(-time.Hour)), "hash-"+id, owner, formatTime(now.Add(time.Hour)), stamp, stamp)
		if err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	// orphan: a valid lease but no executor row at all.
	seed("job-orphan", "local-deadbeef")
	// live: an executor whose heartbeat is fresh — its still-valid lease stays.
	if err := repo.RegisterExecutor(ctx, "local-alive", "hub", now); err != nil {
		t.Fatal(err)
	}
	seed("job-live", "local-alive")
	// stale: an executor whose heartbeat aged out — its job is an orphan too.
	if err := repo.RegisterExecutor(ctx, "local-stale", "hub", now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	seed("job-stale", "local-stale")

	released, terminal, err := repo.HealStaleRunningJobs(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if released != 2 || terminal != 0 {
		t.Fatalf("released=%d terminal=%d, want 2/0 (orphan + stale, live untouched)", released, terminal)
	}
	for _, id := range []string{"job-orphan", "job-stale"} {
		var state string
		var owner sql.NullString
		if err := repo.db.QueryRowContext(ctx, `SELECT state,lease_owner FROM jobs WHERE id=?`, id).Scan(&state, &owner); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if domain.JobState(state) != domain.JobPending || owner.Valid {
			t.Fatalf("%s = state=%s owner.valid=%v, want pending/cleared", id, state, owner.Valid)
		}
	}
	var state string
	var owner sql.NullString
	if err := repo.db.QueryRowContext(ctx, `SELECT state,lease_owner FROM jobs WHERE id='job-live'`).Scan(&state, &owner); err != nil {
		t.Fatal(err)
	}
	if domain.JobState(state) != domain.JobRunning || !owner.Valid || owner.String != "local-alive" {
		t.Fatalf("job-live = state=%s owner=%v, want running/local-alive untouched", state, owner)
	}

	// Unregistering a live executor makes its job an orphan on the next sweep.
	if err := repo.UnregisterExecutor(ctx, "local-alive"); err != nil {
		t.Fatal(err)
	}
	if released, terminal, err := repo.HealStaleRunningJobs(ctx, now); err != nil || released != 1 || terminal != 0 {
		t.Fatalf("after unregister released=%d terminal=%d err=%v, want 1/0/nil", released, terminal, err)
	}
}
