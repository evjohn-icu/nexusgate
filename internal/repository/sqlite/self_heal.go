package sqlite

import (
	"context"
	"time"
)

// executorStaleAfter bounds how stale a pipeline executor's heartbeat may be
// before HealStaleRunningJobs treats its in-flight jobs as orphans and hands
// them back to the queue. It must stay comfortably above the executor's own
// heartbeat interval (15s in the Hub service) so a GC pause or a delayed tick
// can never cost a live pass its work.
const executorStaleAfter = 90 * time.Second

// RegisterExecutor records that a process is (about to be) running the
// Hub-local pipeline. executorID is the same id the process uses as its lease
// owner, so the sweep can map a 'running' job back to its executor.
func (r *Repository) RegisterExecutor(ctx context.Context, executorID, kind string, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO pipeline_executors(executor_id,kind,last_seen_at) VALUES(?,?,?)
		ON CONFLICT(executor_id) DO UPDATE SET kind=excluded.kind,last_seen_at=excluded.last_seen_at`,
		executorID, kind, formatTime(now))
	return err
}

// TouchExecutor refreshes an executor's heartbeat. It is called on a short
// tick for the whole lifetime of the process, including while a single long
// job (a derive encode, an ASR call) is blocking the pipeline pass.
func (r *Repository) TouchExecutor(ctx context.Context, executorID string, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE pipeline_executors SET last_seen_at=? WHERE executor_id=?`, formatTime(now), executorID)
	return err
}

// UnregisterExecutor removes an executor's liveness row on clean shutdown, so
// a successor process's sweep does not wait for the heartbeat to go stale.
func (r *Repository) UnregisterExecutor(ctx context.Context, executorID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM pipeline_executors WHERE executor_id=?`, executorID)
	return err
}

// HealStaleRunningJobs is the crash-recovery sweep a Hub runs once at
// startup: 'running' rows whose lease has expired are crash artifacts, not
// work in flight — nothing is executing yet, so nothing can still be
// finishing them. Releasing them back to 'pending' is what lets the queue
// drain after a restart instead of showing them stuck on /progress forever
// (the lazy reclaims in LeaseNextJob and LeaseNextWorkerDerive only fire
// when someone actually asks for work, and a Hub that is only serving the
// UI never does).
//
// The attempt budget is preserved on purpose: the lease already spent its
// attempt (LeaseNextJob increments attempt_count when it hands the job out),
// so zeroing it would give the job more attempts than it is entitled to —
// the same argument reclaimExhaustedLeases documents for refusing to hand
// exhausted jobs back out. Rows that exhausted their attempts go terminal
// instead, mirroring reclaimExhaustedLeases so a job that keeps dying never
// restarts its budget.
//
// Rows with a still-valid lease are never touched: a live Worker, a second
// Hub process, or the Hub's own still-running pipeline pass may be mid-work
// on them, and this sweep running at startup of one process must not yank a
// lease another process is legitimately holding.
//
// The one deliberate exception is the Hub-local pipeline's own leases. A
// killed process leaves 'running' rows whose lease is still valid (derive
// leases run 30 minutes), so expiry alone would stall the queue for the whole
// TTL after a restart. Every Hub-local executor (serve, pipeline run) keeps a
// heartbeat row in pipeline_executors; a 'running' job whose lease owner is a
// local executor with no live heartbeat is a crash orphan, not work in flight
// — nothing is executing it, so nothing can still be finishing it, and it is
// released immediately. Local leases carry no assigned_worker_id, so this can
// never yank a job pinned to a remote Worker.
//
// The timestamp comparison mirrors every other lease query in this package:
// lease_expires_at is RFC3339Nano text, written and compared through
// formatTime so lexical order is chronological order (see timestamps_test.go
// for the invariant).
//
// assigned_worker_id is deliberately NOT cleared here. The assignment pins a
// job to the node that staged its media (staging-locality: the Worker's
// local cache holds the source, and re-running elsewhere means re-copying);
// a job whose assigned worker is dead waits for that worker to return and
// claim it, exactly as it does under the lazy reclaims. The sweep recovers
// Hub-local jobs (no assignment) and jobs whose worker comes back — it does
// not silently re-route a dead node's work to a different node.
func (r *Repository) HealStaleRunningJobs(ctx context.Context, now time.Time) (released int, terminal int, err error) {
	staleBefore := formatTime(now.Add(-executorStaleAfter))
	res, err := r.db.ExecContext(ctx, `UPDATE jobs SET state='pending',lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE state='running' AND terminal=0 AND attempt_count<max_attempts AND (lease_expires_at IS NOT NULL AND lease_expires_at<=? OR lease_owner LIKE 'local-%' AND NOT EXISTS (SELECT 1 FROM pipeline_executors pe WHERE pe.executor_id=jobs.lease_owner AND pe.last_seen_at>=?))`, formatTime(now), formatTime(now), staleBefore)
	if err != nil {
		return 0, 0, err
	}
	n, _ := res.RowsAffected()
	released = int(n)
	res, err = r.db.ExecContext(ctx, `UPDATE jobs SET state='failed',terminal=1,lease_owner=NULL,lease_expires_at=NULL,last_error_message=COALESCE(last_error_message,'lease expired with attempts exhausted'),updated_at=? WHERE state='running' AND terminal=0 AND attempt_count>=max_attempts AND (lease_expires_at IS NOT NULL AND lease_expires_at<=? OR lease_owner LIKE 'local-%' AND NOT EXISTS (SELECT 1 FROM pipeline_executors pe WHERE pe.executor_id=jobs.lease_owner AND pe.last_seen_at>=?))`, formatTime(now), formatTime(now), staleBefore)
	if err != nil {
		return released, 0, err
	}
	n, _ = res.RowsAffected()
	terminal = int(n)
	return released, terminal, nil
}
