package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

func issuesRepo(t *testing.T) (*Repository, context.Context) {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "issues.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	for _, asset := range []string{"asset-quota", "asset-auth", "asset-exhausted-a", "asset-exhausted-b", "asset-budget", "asset-unknown", "asset-running", "asset-skipped"} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, asset, "fp-"+asset, now, now); err != nil {
			t.Fatal(err)
		}
	}
	return repo, ctx
}

// insertIssueJob writes a job row directly with exactly the failure columns
// the aggregation reads, so a category change under test is a change to the
// rows, not to how the row was manufactured.
func insertIssueJob(t *testing.T, repo *Repository, ctx context.Context, id, assetID, state string, terminal bool, runAfter, createdAt time.Time, code *string) {
	t.Helper()
	var codeVal any
	if code != nil {
		codeVal = *code
	}
	term := 0
	if terminal {
		term = 1
	}
	now := formatTime(createdAt)
	_, err := repo.db.ExecContext(ctx, `INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,terminal,last_error_code,created_at,updated_at) VALUES(?,?,?,?,10,1,3,?,?,?,?,?,?)`,
		id, assetID, string(domain.JobProbe), state, formatTime(runAfter), "hash-"+id, term, codeVal, now, now)
	if err != nil {
		t.Fatal(err)
	}
}

func strPtr(s string) *string { return &s }

// TestJobIssuesAggregatesFailuresByCategory pins the whole shape at once:
// which states count as an issue, which codes land where, what the NULL
// fallback bucket is, and which rows must never appear.
func TestJobIssuesAggregatesFailuresByCategory(t *testing.T) {
	repo, ctx := issuesRepo(t)
	future := time.Now().Add(5 * time.Hour)
	past := time.Now().Add(-1 * time.Hour)

	quota := string(domain.JobFailureCategoryProviderQuota)
	auth := string(domain.JobFailureCategoryProviderAuth)
	exhausted := string(domain.JobFailureCategoryProviderRouteExhausted)
	budget := string(domain.JobFailureCategoryBudgetExhausted)

	// One failed non-terminal job on provider quota.
	insertIssueJob(t, repo, ctx, "job-quota", "asset-quota", string(domain.JobFailed), false, past, time.Now().Add(-3*time.Hour), &quota)
	// One failed terminal job on provider auth.
	insertIssueJob(t, repo, ctx, "job-auth", "asset-auth", string(domain.JobFailed), true, past, time.Now().Add(-2*time.Hour), &auth)
	// Two parked deferrals on provider route exhausted, distinct assets, the
	// second one created earlier so OldestAt picks it deterministically.
	insertIssueJob(t, repo, ctx, "job-exhausted-b", "asset-exhausted-b", string(domain.JobPending), false, future, time.Now().Add(-4*time.Hour), &exhausted)
	insertIssueJob(t, repo, ctx, "job-exhausted-a", "asset-exhausted-a", string(domain.JobPending), false, future, time.Now().Add(-1*time.Hour), &exhausted)
	// One parked deferral on budget exhausted: must surface like the other
	// defer codes, not as an ordinary pending job.
	insertIssueJob(t, repo, ctx, "job-budget", "asset-budget", string(domain.JobPending), false, future, time.Now().Add(-2*time.Hour), &budget)
	// A succeeded job whose last failure was provider quota: not an issue.
	insertIssueJob(t, repo, ctx, "job-succeeded", "asset-quota", string(domain.JobSucceeded), false, past, time.Now().Add(-1*time.Hour), &quota)
	// A failed job with no code at all: must surface as 'unknown', not vanish.
	insertIssueJob(t, repo, ctx, "job-unknown", "asset-unknown", string(domain.JobFailed), false, past, time.Now().Add(-30*time.Minute), nil)

	// Rows that must never appear: a running job, an ordinary pending job
	// with a future run_after but no defer code, and a skipped job.
	running := string(domain.JobFailureCategoryProviderQuota)
	insertIssueJob(t, repo, ctx, "job-running", "asset-running", string(domain.JobRunning), false, past, time.Now().Add(-30*time.Minute), &running)
	ordinaryPending := string(domain.JobFailureCategoryProviderQuota)
	insertIssueJob(t, repo, ctx, "job-pending-retry", "asset-running", string(domain.JobPending), false, future, time.Now().Add(-30*time.Minute), &ordinaryPending)
	insertIssueJob(t, repo, ctx, "job-skipped", "asset-skipped", string(domain.JobSkipped), false, past, time.Now().Add(-30*time.Minute), nil)

	issues, err := repo.JobIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byCategory := make(map[domain.JobFailureCategory]domain.JobIssue, len(issues))
	for _, issue := range issues {
		byCategory[issue.Category] = issue
	}

	quotaIssue, ok := byCategory[domain.JobFailureCategoryProviderQuota]
	if !ok {
		t.Fatalf("provider_quota missing from %+v", issues)
	}
	if quotaIssue.Count != 1 || quotaIssue.AssetCount != 1 || quotaIssue.Terminal != 0 {
		t.Fatalf("provider_quota issue=%+v, want count 1 / 1 asset / 0 terminal", quotaIssue)
	}
	if quotaIssue.OldestAt == nil || quotaIssue.NextRetryAt != nil {
		t.Fatalf("provider_quota issue=%+v, want oldest_at set and next_retry_at nil", quotaIssue)
	}
	if quotaIssue.ExampleAsset != "asset-quota" {
		t.Fatalf("provider_quota example_asset=%q, want asset-quota", quotaIssue.ExampleAsset)
	}

	authIssue, ok := byCategory[domain.JobFailureCategoryProviderAuth]
	if !ok {
		t.Fatalf("provider_auth missing from %+v", issues)
	}
	if authIssue.Count != 1 || authIssue.Terminal != 1 {
		t.Fatalf("provider_auth issue=%+v, want count 1 with 1 terminal", authIssue)
	}

	exhaustedIssue, ok := byCategory[domain.JobFailureCategoryProviderRouteExhausted]
	if !ok {
		t.Fatalf("provider_route_exhausted missing from %+v", issues)
	}
	if exhaustedIssue.Count != 2 || exhaustedIssue.AssetCount != 2 || exhaustedIssue.Terminal != 0 {
		t.Fatalf("provider_route_exhausted issue=%+v, want count 2 / 2 assets / 0 terminal", exhaustedIssue)
	}
	if exhaustedIssue.NextRetryAt == nil {
		t.Fatalf("provider_route_exhausted issue=%+v, want next_retry_at set", exhaustedIssue)
	}
	if !exhaustedIssue.NextRetryAt.After(time.Now()) {
		t.Fatalf("provider_route_exhausted next_retry_at=%v, want in the future", exhaustedIssue.NextRetryAt)
	}

	budgetIssue, ok := byCategory[domain.JobFailureCategoryBudgetExhausted]
	if !ok {
		t.Fatalf("budget_exhausted missing from %+v", issues)
	}
	if budgetIssue.Count != 1 || budgetIssue.AssetCount != 1 || budgetIssue.Terminal != 0 {
		t.Fatalf("budget_exhausted issue=%+v, want count 1 / 1 asset / 0 terminal", budgetIssue)
	}
	if budgetIssue.NextRetryAt == nil || !budgetIssue.NextRetryAt.After(time.Now()) {
		t.Fatalf("budget_exhausted next_retry_at=%v, want set in the future", budgetIssue.NextRetryAt)
	}

	unknownIssue, ok := byCategory[domain.JobFailureCategoryUnknown]
	if !ok {
		t.Fatalf("unknown missing from %+v", issues)
	}
	if unknownIssue.Count != 1 || unknownIssue.Terminal != 0 {
		t.Fatalf("unknown issue=%+v, want count 1 with 0 terminal", unknownIssue)
	}

	if len(issues) != 5 {
		t.Fatalf("got %d categories %+v, want exactly 5", len(issues), issues)
	}
}

// TestJobIssuesEmptyQueueIsEmptySlice pins the JSON contract: an empty
// backlog marshals as [] rather than null, because the browser UI treats the
// two differently.
func TestJobIssuesEmptyQueueIsEmptySlice(t *testing.T) {
	repo, ctx := issuesRepo(t)
	issues, err := repo.JobIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if issues == nil || len(issues) != 0 {
		t.Fatalf("issues=%v, want empty non-nil slice", issues)
	}
}
