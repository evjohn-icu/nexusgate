package domain

import "time"

// JobIssue is one row of the failure backlog: how many jobs currently sit in
// a category, how many distinct assets they span, whether any of them is
// terminal (permanent — will never be leased again), when the oldest of them
// failed, and — for parked deferrals only — when the next one resumes. The
// representative asset id is the aggregation's example, not a claim that the
// asset itself is the most representative failure.
type JobIssue struct {
	Category     JobFailureCategory `json:"category"`
	Count        int                `json:"count"`
	AssetCount   int                `json:"asset_count"`
	Terminal     int                `json:"terminal"`
	OldestAt     *time.Time         `json:"oldest_at,omitempty"`
	NextRetryAt  *time.Time         `json:"next_retry_at,omitempty"`
	ExampleAsset string             `json:"example_asset,omitempty"`
}
