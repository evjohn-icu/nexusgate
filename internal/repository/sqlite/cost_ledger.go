package sqlite

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/idgen"
)

// RecordCostEstimate appends one estimate row to the cost ledger. The day is
// the entry's own (the caller commits it as the UTC day the estimate is
// attributed to); created_at is persisted here so the ledger cannot drift
// from a call-site timestamp.
func (r *Repository) RecordCostEstimate(ctx context.Context, entry domain.CostEntry) error {
	if strings.TrimSpace(entry.Day) == "" {
		return errors.New("cost ledger day is required")
	}
	if strings.TrimSpace(entry.Capability) == "" {
		return errors.New("cost ledger capability is required")
	}
	if entry.Estimate < 0 {
		return errors.New("cost ledger estimate must not be negative")
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO cost_ledger(id,day,capability,provider,model,asset_id,estimate,created_at)
	VALUES(?,?,?,?,?,?,?,?)`, idgen.New(), strings.TrimSpace(entry.Day), strings.TrimSpace(entry.Capability), strings.TrimSpace(entry.Provider), strings.TrimSpace(entry.Model), strings.TrimSpace(entry.AssetID), entry.Estimate, formatTime(time.Now().UTC()))
	return err
}

// CostEstimateForDay sums the ledger rows attributed to one UTC calendar day
// (YYYY-MM-DD). No rows for the day sum to zero.
func (r *Repository) CostEstimateForDay(ctx context.Context, day string) (float64, error) {
	return r.costEstimateSum(ctx, `WHERE day=?`, strings.TrimSpace(day))
}

// CostEstimateForMonth sums the ledger rows attributed to one UTC year-month
// (YYYY-MM) via a day LIKE 'YYYY-MM-%' prefix. No rows for the month sum to
// zero.
func (r *Repository) CostEstimateForMonth(ctx context.Context, yearMonth string) (float64, error) {
	return r.costEstimateSum(ctx, `WHERE day LIKE ?`, strings.TrimSpace(yearMonth)+"-%")
}

// costEstimateSum is the shared SUM over the ledger's day column. COALESCE
// keeps an empty ledger at zero instead of a NULL.
func (r *Repository) costEstimateSum(ctx context.Context, whereClause, arg string) (float64, error) {
	var total float64
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(estimate),0) FROM cost_ledger `+whereClause, arg).Scan(&total)
	return total, err
}
