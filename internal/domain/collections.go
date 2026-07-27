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
type AssetCollectionFilter struct {
	CapturedFrom *time.Time       `json:"captured_from,omitempty"`
	CapturedTo   *time.Time       `json:"captured_to,omitempty"`
	RegionLabel  string           `json:"region_label,omitempty"`
	CameraModel  string           `json:"camera_model,omitempty"`
	SessionID    string           `json:"session_id,omitempty"`
	Status       ProcessingStatus `json:"status,omitempty"`
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
