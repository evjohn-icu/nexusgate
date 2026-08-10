package sqlite

import (
	"context"
	"time"
)

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
	res, err := r.db.ExecContext(ctx, `UPDATE jobs SET state='pending',lease_owner=NULL,lease_expires_at=NULL,updated_at=? WHERE state='running' AND lease_expires_at IS NOT NULL AND lease_expires_at<=? AND terminal=0 AND attempt_count<max_attempts`, formatTime(now), formatTime(now))
	if err != nil {
		return 0, 0, err
	}
	n, _ := res.RowsAffected()
	released = int(n)
	res, err = r.db.ExecContext(ctx, `UPDATE jobs SET state='failed',terminal=1,lease_owner=NULL,lease_expires_at=NULL,last_error_message=COALESCE(last_error_message,'lease expired with attempts exhausted'),updated_at=? WHERE state='running' AND lease_expires_at IS NOT NULL AND lease_expires_at<=? AND terminal=0 AND attempt_count>=max_attempts`, formatTime(now), formatTime(now))
	if err != nil {
		return released, 0, err
	}
	n, _ = res.RowsAffected()
	terminal = int(n)
	return released, terminal, nil
}
