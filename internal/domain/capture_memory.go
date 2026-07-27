package domain

import "time"

// ShootSession is the browse-safe projection of one materialized shoot
// session. Capture coordinates deliberately do not appear here; callers get
// the persisted region label without receiving exact location data.
type ShootSession struct {
	ID          string     `json:"id"`
	RootID      string     `json:"root_id,omitempty"`
	Title       string     `json:"title"`
	State       string     `json:"state"`
	StartsAt    *time.Time `json:"starts_at,omitempty"`
	EndsAt      *time.Time `json:"ends_at,omitempty"`
	RegionLabel string     `json:"region_label,omitempty"`
	CameraLabel string     `json:"camera_label,omitempty"`
	Confidence  float64    `json:"confidence"`
	AssetIDs    []string   `json:"asset_ids,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// ShootSessionFilter contains the stable browse filters supported by the
// repository. Empty strings leave that dimension unconstrained.
type ShootSessionFilter struct {
	RootID       string     `json:"root_id,omitempty"`
	State        string     `json:"state,omitempty"`
	RegionLabel  string     `json:"region_label,omitempty"`
	CameraLabel  string     `json:"camera_label,omitempty"`
	StartsAfter  *time.Time `json:"starts_after,omitempty"`
	StartsBefore *time.Time `json:"starts_before,omitempty"`
	EndsAfter    *time.Time `json:"ends_after,omitempty"`
	EndsBefore   *time.Time `json:"ends_before,omitempty"`
	Limit        int        `json:"limit,omitempty"`
	Offset       int        `json:"offset,omitempty"`
}

// ShootSessionAsset is one persisted session-to-asset membership.
type ShootSessionAsset struct {
	AssetID   string    `json:"asset_id"`
	SessionID string    `json:"session_id"`
	IsPrimary bool      `json:"is_primary"`
	CreatedAt time.Time `json:"created_at"`
}
