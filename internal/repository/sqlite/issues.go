package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// JobIssues aggregates the failure backlog by category in one pass. The
// WHERE arm mirrors the queue's two failure shapes: state='failed' is every
// failure the pipeline has given up on or will retry (terminal or not), and
// the parked-deferral arm — pending with run_after in the future and a defer
// code in last_error_code — is the same predicate ListJobs and JobSummary
// use for a job waiting on provider quota or disk space. Succeeded, skipped
// and running jobs never carry a current issue, and an ordinary retry
// backoff (pending, future run_after, non-defer code) is not an issue
// either: the job will come up on its own.
//
// The defer codes arrive as parameters from the domain constants the
// pipeline writes them with, so the aggregation stays in sync with what
// DeferJob stores without a second enumeration to forget. They are bound
// from the category constants (JobDeferProviderRouteExhausted is spelled
// from JobFailureCategoryProviderRouteExhausted), so the deferral writes and
// the issues grouping cannot drift apart either.
//
// NULL and empty last_error_code both group under 'unknown': NULL is a job
// that failed before classification existed (or whose failure predates the
// code), and a bare comparison on a nullable column would silently drop it
// — `NULL = 'unknown'` is NULL, not true, so the fallback is written with
// COALESCE(NULLIF(...)) rather than in the WHERE clause.
func (r *Repository) JobIssues(ctx context.Context) ([]domain.JobIssue, error) {
	now := formatTime(time.Now())
	rows, err := r.db.QueryContext(ctx, `SELECT
    COALESCE(NULLIF(last_error_code,''),'unknown') AS category,
    COUNT(*) AS count,
    COUNT(DISTINCT asset_id) AS asset_count,
    COALESCE(SUM(CASE WHEN terminal=1 THEN 1 ELSE 0 END),0) AS terminal_count,
    MIN(created_at) AS oldest_at,
    MIN(CASE WHEN state='pending' THEN run_after END) AS next_retry_at,
    MIN(asset_id) AS example_asset
FROM jobs
WHERE state='failed'
   OR (state='pending' AND run_after>? AND last_error_code IN (?,?,?))
GROUP BY category
ORDER BY count DESC, category`, now,
		domain.JobFailureCategoryProviderRouteExhausted, domain.JobFailureCategoryDiskSpaceLow, domain.JobFailureCategoryBudgetExhausted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	issues := []domain.JobIssue{}
	for rows.Next() {
		var (
			issue       domain.JobIssue
			oldestAt    string
			nextRetryAt sql.NullString
			example     sql.NullString
		)
		if err := rows.Scan(&issue.Category, &issue.Count, &issue.AssetCount, &issue.Terminal, &oldestAt, &nextRetryAt, &example); err != nil {
			return nil, err
		}
		// The aggregation stores times as the sortable text format the rest
		// of the queue reads with; parse them back so the API can marshal
		// RFC3339 without each consumer re-deriving the format. A failed
		// job's run_after is in the past (it was set at lease time), so
		// next_retry_at is only populated for parked deferrals by the
		// state='pending' gate in the query.
		if t, err := time.Parse(time.RFC3339Nano, oldestAt); err == nil {
			issue.OldestAt = &t
		}
		if nextRetryAt.Valid {
			if t, err := time.Parse(time.RFC3339Nano, nextRetryAt.String); err == nil {
				issue.NextRetryAt = &t
			}
		}
		if example.Valid {
			issue.ExampleAsset = example.String
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}
