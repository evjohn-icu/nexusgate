package sqlite

import (
	"context"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// seedCostEstimate inserts one ledger row via the public method so the tests
// exercise the write path they depend on.
func seedCostEstimate(t *testing.T, repo *Repository, day string, estimate float64) {
	t.Helper()
	err := repo.RecordCostEstimate(context.Background(), domain.CostEntry{
		Day:        day,
		Capability: "video_analysis",
		Provider:   "gemini",
		Model:      "gemini-flash",
		AssetID:    "asset-1",
		Estimate:   estimate,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// CostEstimateForDay must sum exactly the rows attributed to one UTC day,
// nothing more: the day boundary is the budget gate's enforcement point, and
// a sum that bled across days would let a spent day's budget stay spent into
// the next one.
func TestCostEstimateForDaySumsOnlyThatDay(t *testing.T) {
	repo := openTestRepo(t)
	seedCostEstimate(t, repo, "2026-08-09", 25.5)
	seedCostEstimate(t, repo, "2026-08-09", 14.5)
	seedCostEstimate(t, repo, "2026-08-10", 100)

	got, err := repo.CostEstimateForDay(context.Background(), "2026-08-09")
	if err != nil {
		t.Fatal(err)
	}
	if got != 40 {
		t.Fatalf("day sum=%v, want 40", got)
	}
	if got, err := repo.CostEstimateForDay(context.Background(), "2026-08-10"); err != nil || got != 100 {
		t.Fatalf("next-day sum=%v err=%v, want 100", got, err)
	}
}

// CostEstimateForMonth sums every day of the month. The LIKE prefix must not
// bleed across the year-month boundary: "2026-01" must not catch "2026-10",
// and a December row must not leak into January — the monthly gate re-arms on
// the 1st, so an off-by-one in the prefix would re-arm it a month early or
// late.
func TestCostEstimateForMonthSumsWithinTheYearMonthOnly(t *testing.T) {
	repo := openTestRepo(t)
	seedCostEstimate(t, repo, "2026-01-31", 10)
	seedCostEstimate(t, repo, "2026-01-15", 20)
	seedCostEstimate(t, repo, "2026-02-01", 5)
	seedCostEstimate(t, repo, "2026-10-15", 500)
	seedCostEstimate(t, repo, "2025-12-31", 999)

	got, err := repo.CostEstimateForMonth(context.Background(), "2026-01")
	if err != nil {
		t.Fatal(err)
	}
	if got != 30 {
		t.Fatalf("month sum=%v, want 30", got)
	}
	if got, err := repo.CostEstimateForMonth(context.Background(), "2026-10"); err != nil || got != 500 {
		t.Fatalf("October sum=%v err=%v, want 500", got, err)
	}
}

// An empty ledger is zero, not NULL: the budget gate compares sums against
// budgets, and a NULL would never satisfy >=.
func TestCostEstimateSumsAreZeroForAnEmptyLedger(t *testing.T) {
	repo := openTestRepo(t)
	if got, err := repo.CostEstimateForDay(context.Background(), "2026-08-09"); err != nil || got != 0 {
		t.Fatalf("empty-day sum=%v err=%v, want 0", got, err)
	}
	if got, err := repo.CostEstimateForMonth(context.Background(), "2026-08"); err != nil || got != 0 {
		t.Fatalf("empty-month sum=%v err=%v, want 0", got, err)
	}
}

// RecordCostEstimate guards the ledger's invariants: a row must always be
// attributable to a day and a capability, and an estimate must never be
// negative — the append-only ledger is the budget gate's source of truth, and
// a silently dropped field would under-report spend.
func TestRecordCostEstimateRejectsMalformedEntries(t *testing.T) {
	repo := openTestRepo(t)
	for name, entry := range map[string]domain.CostEntry{
		"empty day":        {Day: "", Capability: "video_analysis", Estimate: 1},
		"empty capability": {Day: "2026-08-09", Capability: " ", Estimate: 1},
		"negative cost":    {Day: "2026-08-09", Capability: "video_analysis", Estimate: -1},
	} {
		if err := repo.RecordCostEstimate(context.Background(), entry); err == nil {
			t.Fatalf("%s: malformed entry accepted: %+v", name, entry)
		}
	}
}
