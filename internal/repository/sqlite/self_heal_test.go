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

// cancelJobSeed describes one jobs row for the cancellation tests. Zero lease
// owner/expiry insert NULLs; zero expiry time inserts NULL as well.
type cancelJobSeed struct {
	id        string
	assetID   string
	state     domain.JobState
	terminal  int
	attempts  int
	progress  float64
	stage     string
	owner     string
	expires   time.Time
	errorCode string
	errorMsg  string
}

func seedCancelJob(t *testing.T, repo *Repository, seed cancelJobSeed) {
	t.Helper()
	ctx := context.Background()
	stamp := formatTime(time.Now())
	var owner, expiry any
	if seed.owner != "" {
		owner = seed.owner
	}
	if !seed.expires.IsZero() {
		expiry = formatTime(seed.expires)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,lease_owner,lease_expires_at,terminal,progress,current_stage,last_error_code,last_error_message,created_at,updated_at)
		VALUES(?,?,?,? ,10,?,3,?,?,?,?,?,?,?,?,?,?,?)`,
		seed.id, seed.assetID, string(domain.JobInference), string(seed.state), seed.attempts, stamp, "hash-"+seed.id, owner, expiry,
		seed.terminal, seed.progress, seed.stage,
		nullString(seed.errorCode), nullString(seed.errorMsg), stamp, stamp); err != nil {
		t.Fatalf("seed job %s: %v", seed.id, err)
	}
}

func seedCancelTask(t *testing.T, repo *Repository, jobID string, cancelRequested int) {
	t.Helper()
	_, err := repo.db.ExecContext(context.Background(), `INSERT INTO inference_tasks(job_id,task_key,operation,request_json,config_hash,model_revision,cancel_requested,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		jobID, "key-"+jobID, string(domain.OpVisualEmbedding), `{"schema_version":"inference.v1"}`, "cfg-hash", "revision-1", cancelRequested, formatTime(time.Now()), formatTime(time.Now()))
	if err != nil {
		t.Fatalf("seed task %s: %v", jobID, err)
	}
}

// cancelJobState reads back the fields the cancellation policy constrains.
func cancelJobState(t *testing.T, repo *Repository, id string) (state domain.JobState, terminal int, progress float64, stage, errorCode, errorMsg string, owner, expiry sql.NullString) {
	t.Helper()
	var code, msg sql.NullString
	err := repo.db.QueryRowContext(context.Background(), `SELECT state,terminal,progress,current_stage,last_error_code,last_error_message,lease_owner,lease_expires_at FROM jobs WHERE id=?`, id).
		Scan(&state, &terminal, &progress, &stage, &code, &msg, &owner, &expiry)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return state, terminal, progress, stage, code.String, msg.String, owner, expiry
}

func cancelTaskFlagged(t *testing.T, repo *Repository, jobID string) int {
	t.Helper()
	var flagged int
	if err := repo.db.QueryRowContext(context.Background(), `SELECT cancel_requested FROM inference_tasks WHERE job_id=?`, jobID).Scan(&flagged); err != nil {
		t.Fatalf("read task %s: %v", jobID, err)
	}
	return flagged
}

// cancelInferenceTasksTx must terminalize every nonterminal job (pending,
// running, retryable) bound to the selected tasks under the cancellation
// policy — terminal 'cancelled', no lease, progress untouched — while
// preserving succeeded and already-terminal jobs entirely.
func TestCancelInferenceTasksTxTerminalizesNonterminalJobs(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepo(t)
	seedAsset(t, repo, 1)
	now := time.Now()

	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-1", id: "job-c-pending", state: domain.JobPending})
	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-1", id: "job-c-running", state: domain.JobRunning, owner: "worker-a", expires: now.Add(time.Hour), progress: 40, stage: "embedding"})
	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-1", id: "job-c-retryable", state: domain.JobFailed, errorCode: "provider_busy", errorMsg: "busy"})
	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-1", id: "job-c-succeeded", state: domain.JobSucceeded, progress: 100, stage: "complete"})
	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-1", id: "job-c-terminal", state: domain.JobFailed, terminal: 1, errorCode: "provider_auth", errorMsg: "real failure detail"})
	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-1", id: "job-c-derive", state: domain.JobPending})
	for _, id := range []string{"job-c-pending", "job-c-running", "job-c-retryable", "job-c-succeeded", "job-c-terminal"} {
		seedCancelTask(t, repo, id, 0)
	}

	ids := []string{"job-c-pending", "job-c-running", "job-c-retryable", "job-c-succeeded", "job-c-terminal", "job-c-derive"}
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	flagged, terminalized, err := cancelInferenceTasksTx(ctx, tx, now, "cancelled by request", "job_id IN (?,?,?,?,?,?)",
		ids[0], ids[1], ids[2], ids[3], ids[4], ids[5])
	if err != nil {
		t.Fatal(err)
	}
	if flagged != 5 || terminalized != 3 {
		t.Fatalf("flagged=%d terminalized=%d, want 5/3 (succeeded and terminal preserved, derive has no task)", flagged, terminalized)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	state, terminal, progress, stage, code, msg, owner, expiry := cancelJobState(t, repo, "job-c-pending")
	if state != domain.JobFailed || terminal != 1 || stage != "cancelled" || code != "cancelled" || msg != "cancelled by request" || owner.Valid || expiry.Valid {
		t.Fatalf("job-c-pending = %s/%d/stage=%s/code=%s/msg=%q, want terminal cancelled with cleared lease", state, terminal, stage, code, msg)
	}
	state, terminal, progress, stage, code, msg, owner, expiry = cancelJobState(t, repo, "job-c-running")
	if state != domain.JobFailed || terminal != 1 || progress != 40 || stage != "cancelled" || code != "cancelled" || owner.Valid || expiry.Valid {
		t.Fatalf("job-c-running = %s/%d/progress=%v/stage=%s, want terminal cancelled, progress 40 untouched, lease cleared", state, terminal, progress, stage)
	}
	state, terminal, progress, stage, code, msg, owner, expiry = cancelJobState(t, repo, "job-c-retryable")
	if state != domain.JobFailed || terminal != 1 || code != "cancelled" || msg != "cancelled by request" {
		t.Fatalf("job-c-retryable = %s/%d/code=%s/msg=%q, want retriable failure terminalized 'cancelled'", state, terminal, code, msg)
	}
	state, terminal, progress, stage, code, msg, owner, expiry = cancelJobState(t, repo, "job-c-succeeded")
	if state != domain.JobSucceeded || terminal != 0 || progress != 100 || stage != "complete" || code != "" || msg != "" || owner.Valid || expiry.Valid {
		t.Fatalf("job-c-succeeded = %s/%d/progress=%v/stage=%s/code=%s/msg=%q, want succeeded preserved untouched", state, terminal, progress, stage, code, msg)
	}
	state, terminal, _, _, code, msg, _, _ = cancelJobState(t, repo, "job-c-terminal")
	if state != domain.JobFailed || terminal != 1 || code != "provider_auth" || msg != "real failure detail" {
		t.Fatalf("job-c-terminal = %s/code=%s/msg=%q, want already-terminal job preserved", state, code, msg)
	}
	state, _, _, _, _, _, owner, _ = cancelJobState(t, repo, "job-c-derive")
	if state != domain.JobPending || owner.Valid {
		t.Fatalf("job-c-derive = %s/owner=%v, want untouched derive job", state, owner.Valid)
	}
	for _, id := range ids[:5] {
		if cancelTaskFlagged(t, repo, id) != 1 {
			t.Fatalf("task %s not flagged cancel_requested", id)
		}
	}

	// An unknown task flags nothing and changes nothing; a blank predicate is
	// rejected outright so a mistyped caller cannot cancel every task.
	tx, err = repo.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if flagged, terminalized, err := cancelInferenceTasksTx(ctx, tx, now, "cancelled", "job_id = ?", "job-ghost"); err != nil || flagged != 0 || terminalized != 0 {
		t.Fatalf("unknown task = (%d,%d,%v), want 0/0/nil", flagged, terminalized, err)
	}
	if _, _, err := cancelInferenceTasksTx(ctx, tx, now, "cancelled", "   "); err == nil {
		t.Fatal("blank cancellation predicate was accepted")
	}
}

// CancelInferenceTasksForReference must take running and retryable query work
// with the reference, not just pending rows, and must leave other references
// and succeeded work alone.
func TestCancelInferenceTasksForReferenceCoversRunningAndRetryable(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepo(t)
	seedAsset(t, repo, 2)
	now := time.Now()

	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-2", id: "job-r-pending", state: domain.JobPending})
	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-2", id: "job-r-running", state: domain.JobRunning, owner: "worker-b", expires: now.Add(time.Hour), progress: 15, stage: "embedding"})
	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-2", id: "job-r-retryable", state: domain.JobFailed})
	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-2", id: "job-r-succeeded", state: domain.JobSucceeded, progress: 100, stage: "complete"})
	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-2", id: "job-r-other", state: domain.JobPending})
	for _, id := range []string{"job-r-pending", "job-r-running", "job-r-retryable", "job-r-succeeded"} {
		seedCancelTask(t, repo, id, 0)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE inference_tasks SET query_reference_id=? WHERE job_id IN (?,?,?,?)`, "ref-x", "job-r-pending", "job-r-running", "job-r-retryable", "job-r-succeeded"); err != nil {
		t.Fatal(err)
	}
	seedCancelTask(t, repo, "job-r-other", 0)
	if _, err := repo.db.ExecContext(ctx, `UPDATE inference_tasks SET query_reference_id=? WHERE job_id=?`, "ref-y", "job-r-other"); err != nil {
		t.Fatal(err)
	}

	if err := repo.CancelInferenceTasksForReference(ctx, "ref-x"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"job-r-pending", "job-r-running", "job-r-retryable"} {
		state, terminal, _, stage, code, msg, owner, _ := cancelJobState(t, repo, id)
		if state != domain.JobFailed || terminal != 1 || stage != "cancelled" || code != "cancelled" || msg != "query reference deleted" || owner.Valid {
			t.Fatalf("%s = %s/%d/stage=%s/msg=%q/owner=%v, want terminal 'cancelled' with cleared lease", id, state, terminal, stage, msg, owner.Valid)
		}
	}
	if state, terminal, _, stage, code, _, _, _ := cancelJobState(t, repo, "job-r-succeeded"); state != domain.JobSucceeded || terminal != 0 || stage != "complete" || code != "" {
		t.Fatalf("job-r-succeeded = %s/%d/stage=%s/code=%s, want succeeded preserved", state, terminal, stage, code)
	}
	if state, _, _, _, _, _, _, _ := cancelJobState(t, repo, "job-r-other"); state != domain.JobPending {
		t.Fatalf("job-r-other = %s, want other reference untouched", state)
	}

	// A blank reference id is a deliberate no-op, not a whole-table cancel.
	if err := repo.CancelInferenceTasksForReference(ctx, "   "); err != nil {
		t.Fatalf("blank reference id: %v", err)
	}
	if state, _, _, _, _, _, _, _ := cancelJobState(t, repo, "job-r-other"); state != domain.JobPending {
		t.Fatalf("job-r-other = %s after blank no-op, want untouched", state)
	}
}

// Escalation and healing must never revive cancelled work: a running row whose
// task is already flagged cancel_requested goes terminal 'cancelled' through
// the sweep HealStaleRunningJobs composes first — never back to pending, and
// never re-labelled as a lease expiry — while an unflagged sibling is released
// as usual. A cancelled row with an exhausted budget is likewise labelled
// 'cancelled', not 'lease expired with attempts exhausted'.
func TestHealStaleRunningJobsNeverRevivesCancelled(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepo(t)
	seedAsset(t, repo, 3)
	now := time.Now()
	expired := now.Add(-time.Hour)

	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-3", id: "job-h-cancelled", state: domain.JobRunning, owner: "worker-c", expires: expired, attempts: 1})
	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-3", id: "job-h-normal", state: domain.JobRunning, owner: "worker-d", expires: expired, attempts: 1})
	seedCancelJob(t, repo, cancelJobSeed{assetID: "asset-ttl-3", id: "job-h-cancelled-exhausted", state: domain.JobRunning, owner: "worker-e", expires: expired, attempts: 3})
	for _, spec := range []struct {
		jobID   string
		flagged int
	}{
		{"job-h-cancelled", 1},
		{"job-h-normal", 0},
		{"job-h-cancelled-exhausted", 1},
	} {
		seedCancelTask(t, repo, spec.jobID, spec.flagged)
	}

	released, terminal, err := repo.HealStaleRunningJobs(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if released != 1 || terminal != 0 {
		t.Fatalf("released=%d terminal=%d, want 1/0 (only the unflagged job is released; the sweep terminalized the cancelled ones)", released, terminal)
	}

	state, terminal, _, stage, code, msg, owner, expiry := cancelJobState(t, repo, "job-h-cancelled")
	if state != domain.JobFailed || terminal != 1 || stage != "cancelled" || code != "cancelled" || msg != "cancelled" || owner.Valid || expiry.Valid {
		t.Fatalf("job-h-cancelled = %s/%d/stage=%s/code=%s/msg=%q, want terminal 'cancelled', never revived", state, terminal, stage, code, msg)
	}
	state, _, _, _, _, _, owner, _ = cancelJobState(t, repo, "job-h-normal")
	if state != domain.JobPending || owner.Valid {
		t.Fatalf("job-h-normal = %s/owner=%v, want released to pending", state, owner.Valid)
	}
	state, terminal, _, _, code, msg, _, _ = cancelJobState(t, repo, "job-h-cancelled-exhausted")
	if state != domain.JobFailed || terminal != 1 || code != "cancelled" || msg != "cancelled" {
		t.Fatalf("job-h-cancelled-exhausted = %s/code=%s/msg=%q, want 'cancelled', not the lease-expiry label", state, code, msg)
	}

	// A second sweep must be a no-op: terminal cancelled rows are out of every
	// predicate, so repeated heartbeats cannot double-write or revive them.
	if released, terminal, err := repo.HealStaleRunningJobs(ctx, now); err != nil || released != 0 || terminal != 0 {
		t.Fatalf("second sweep released=%d terminal=%d err=%v, want 0/0/nil", released, terminal, err)
	}
}

// A freshly crashed executor's heartbeat survives the initial sweep and every
// sweep until the 90-second stale threshold passes, and only then is its work
// released — while a competing executor that keeps heartbeating retains its
// own in-flight task the whole time.
func TestHealStaleRunningJobsStaleThresholdAndCompetingOwner(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepo(t)
	assetID := seedAsset(t, repo, 4)
	start := time.Now()

	if err := repo.RegisterExecutor(ctx, "local-crashed", "hub", start.Add(-5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := repo.RegisterExecutor(ctx, "local-live", "hub", start); err != nil {
		t.Fatal(err)
	}
	seed := func(id, owner string) {
		t.Helper()
		stamp := formatTime(start)
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,lease_owner,lease_expires_at,created_at,updated_at) VALUES(?,?,'derive','running',10,1,3,?,?,?,?,?,?)`,
			id, assetID, formatTime(start.Add(-time.Hour)), "hash-"+id, owner, formatTime(start.Add(30*time.Minute)), stamp, stamp); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("job-th-crashed", "local-crashed")
	seed("job-th-live", "local-live")

	// Initial sweep: the crashed executor's heartbeat is seconds old, so both
	// executors look live and both jobs survive.
	if released, terminal, err := repo.HealStaleRunningJobs(ctx, start); err != nil || released != 0 || terminal != 0 {
		t.Fatalf("initial sweep released=%d terminal=%d err=%v, want 0/0/nil", released, terminal, err)
	}

	// Half a minute later the crashed executor has still not heartbeated, but
	// 35 seconds is inside the stale threshold; the live executor keeps its
	// own heartbeat going.
	mid := start.Add(30 * time.Second)
	if err := repo.TouchExecutor(ctx, "local-live", mid); err != nil {
		t.Fatal(err)
	}
	if released, terminal, err := repo.HealStaleRunningJobs(ctx, mid); err != nil || released != 0 || terminal != 0 {
		t.Fatalf("mid sweep released=%d terminal=%d err=%v, want 0/0/nil before the threshold", released, terminal, err)
	}
	if state, _, _, _, _, _, owner, _ := cancelJobState(t, repo, "job-th-crashed"); state != domain.JobRunning || !owner.Valid {
		t.Fatalf("job-th-crashed = %s/owner=%v before the threshold, want still running under its owner", state, owner.Valid)
	}

	// Past 90 seconds without a heartbeat the crashed executor's work is an
	// orphan and is released; the live executor's task stays put.
	late := start.Add(95 * time.Second)
	if err := repo.TouchExecutor(ctx, "local-live", late); err != nil {
		t.Fatal(err)
	}
	released, terminal, err := repo.HealStaleRunningJobs(ctx, late)
	if err != nil {
		t.Fatal(err)
	}
	if released != 1 || terminal != 0 {
		t.Fatalf("late sweep released=%d terminal=%d, want 1/0 (crashed released, live retained)", released, terminal)
	}
	if state, _, _, _, _, _, owner, expiry := cancelJobState(t, repo, "job-th-crashed"); state != domain.JobPending || owner.Valid || expiry.Valid {
		t.Fatalf("job-th-crashed = %s/owner=%v/expiry=%v after the threshold, want pending with cleared lease", state, owner.Valid, expiry.Valid)
	}
	if state, _, _, _, _, _, owner, _ := cancelJobState(t, repo, "job-th-live"); state != domain.JobRunning || !owner.Valid || owner.String != "local-live" {
		t.Fatalf("job-th-live = %s/owner=%v, want the competing live executor to retain its task", state, owner)
	}
}
