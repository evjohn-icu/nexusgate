package domain

import "time"

// AssetCardFilter is the library-facing capture filter. Precise coordinates
// are intentionally absent: browsing operates on coarse region labels.
type AssetCardFilter struct {
	Limit        int
	Offset       int
	CapturedFrom *time.Time
	CapturedTo   *time.Time
	RegionLabel  string
	CameraModel  string
	SessionID    string
	Status       ProcessingStatus
}
