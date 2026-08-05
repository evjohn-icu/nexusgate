package domain

import "time"

// ProcessingStatus is the browse-safe operational state derived from the
// asset record, jobs, and derived artifacts. It is intentionally separate
// from AssetState, which describes source availability (discovered/missing).
type ProcessingStatus string

const (
	ProcessingStatusDiscovered ProcessingStatus = "discovered"
	ProcessingStatusQueued     ProcessingStatus = "queued"
	ProcessingStatusProcessing ProcessingStatus = "processing"
	ProcessingStatusReady      ProcessingStatus = "ready"
	ProcessingStatusFailed     ProcessingStatus = "failed"
	ProcessingStatusMissing    ProcessingStatus = "missing"
)

// AssetCollectionFilter is the non-secret, user-facing definition of a
// saved library view. It deliberately contains no filesystem or location
// precision fields.
//
// FacetFilter is embedded anonymously so encoding/json flattens it into the
// same persisted JSON object. Its values are safe to persist here because they
// are the closed normalize vocabularies and an integer duration range — not
// free text and not derived from source paths or coordinates, so they carry no
// path or location precision a saved view must not expose. Rows written before
// the embed have no facet keys; they unmarshal to the zero FacetFilter, which
// matches everything, so an old collection keeps returning what it always did.
type AssetCollectionFilter struct {
	CapturedFrom *time.Time       `json:"captured_from,omitempty"`
	CapturedTo   *time.Time       `json:"captured_to,omitempty"`
	RegionLabel  string           `json:"region_label,omitempty"`
	CameraModel  string           `json:"camera_model,omitempty"`
	SessionID    string           `json:"session_id,omitempty"`
	Status       ProcessingStatus `json:"status,omitempty"`
	FacetFilter
}

// AssetCollection is a persisted named filter, not a copy of assets.
type AssetCollection struct {
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	Description string                `json:"description,omitempty"`
	Filter      AssetCollectionFilter `json:"filter"`
	CreatedAt   time.Time             `json:"created_at"`
	UpdatedAt   time.Time             `json:"updated_at"`
}

// SavedCollection is kept as a semantic alias for callers that use the
// product-facing name.
type SavedCollection = AssetCollection

type AssetProcessingSummary struct {
	Total    int                      `json:"total"`
	ByStatus map[ProcessingStatus]int `json:"by_status"`
}
