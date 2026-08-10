package domain

// CostEntry is one row of the cost ledger: an estimate accumulated when a
// model-run commit succeeds for a provider channel that carries cost
// metadata. Estimates are computed from the channel's configured relative
// unit (per request, per video minute, per audio minute) — the unit is
// unspecified by design, so a ledger sum is a guide for cost tracking, never
// a billing record.
type CostEntry struct {
	// Day is the UTC calendar day (YYYY-MM-DD) the estimate was recorded
	// against. Month queries are a LIKE prefix over it, so it must stay in
	// this exact, zero-padded shape.
	Day string
	// Capability is the provider channel capability the estimate belongs to
	// (e.g. video_analysis, asr).
	Capability string
	// Provider is the provider name as reported by the model-run commit.
	Provider string
	// Model is the model name as reported by the model-run commit; empty
	// when the commit path has no model identity (not currently the case).
	Model string
	// AssetID names the asset whose commit produced the estimate; empty
	// when the estimate is not asset-scoped.
	AssetID string
	// Estimate is the accumulated value in the channel's configured unit.
	Estimate float64
}
