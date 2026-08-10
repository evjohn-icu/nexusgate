package sqlite

import (
	"context"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// MarkRootScanStarted records that a scan walk of the root began. It is the
// "the scanner is alive and reaching this root" heartbeat: the scan service
// calls it before the walk and on completion paths, so LastScanAt reflects the
// last attempt regardless of the verdict.
func (r *Repository) MarkRootScanStarted(ctx context.Context, rootID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE library_roots SET last_scan_at=? WHERE id=?`, formatTime(at), rootID)
	return err
}

// MarkRootHealthy records that a scan actually walked a mounted, reachable
// root. Only the scan service's verdict gate calls this -- a root that failed
// its walk must be MarkRootUnavailable instead, or the missing-file
// reconciliation gate would trust a health flag nobody verified.
func (r *Repository) MarkRootHealthy(ctx context.Context, rootID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE library_roots SET health_state=?, last_healthy_at=?, last_scan_at=? WHERE id=?`, string(domain.RootHealthHealthy), formatTime(at), formatTime(at), rootID)
	return err
}

// MarkRootUnavailable records that the walk itself failed (path or mount
// unreachable). It deliberately leaves last_healthy_at untouched: the UI's
// "last healthy" display must keep the most recent verified scan, so an
// outage does not erase the last evidence the root ever worked.
func (r *Repository) MarkRootUnavailable(ctx context.Context, rootID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE library_roots SET health_state=?, last_scan_at=? WHERE id=?`, string(domain.RootHealthUnavailable), formatTime(at), rootID)
	return err
}
