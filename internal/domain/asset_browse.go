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
	Facets       FacetFilter
	// IDs restricts the result to this exact set of asset ids, e.g. the hits
	// from a facet-aware /api/v1/search. Empty means unset — it does not mean
	// "match nothing" — so a caller must not send an empty-but-present ids
	// filter when it means "no ids yet". Ordering still follows the card
	// query's own ORDER BY, not the order ids were given in.
	IDs []string
}

// FacetFilter narrows a query by the normalize package's controlled
// vocabulary (asset_type, shot_size, camera_motion, audio_type, quality,
// usable_as) plus a duration range. Values within one field are OR'd — e.g.
// ShotSizes: []string{"wide","medium"} matches either — but the fields
// themselves are AND'd together by the caller's SQL. The zero value matches
// everything, so embedding this in AssetCardFilter cannot change what an
// already-built, pre-existing filter returns.
//
// This package cannot validate the string values against normalize's
// exported *Values lists itself: normalize imports domain, so the reverse
// import would cycle. Callers (internal/app, internal/api) must validate
// before this reaches SQL — an unvalidated typo must not silently compile
// into a WHERE clause that matches nothing and reads as "no footage".
//
// FacetFilter is shared by AssetCardFilter (asset-level browsing) and the
// shot-search repository methods (SearchShotsFiltered, HybridSearchShotsFiltered,
// SimilarShotsFiltered): all six vocabulary fields live only in
// asset_analysis, one row per asset — there is no shot-level asset_type,
// shot_size, camera_motion, audio_type, quality or usable_as column — so a
// shot search resolves the facet through the shot's own asset via a join,
// same as the asset browse query does. Duration, by contrast, means
// different things at each granularity: MinDurationMS/MaxDurationMS bound
// the asset's total duration (media_metadata.duration_ms) when this is
// embedded in AssetCardFilter, but bound the individual shot's own span
// (end_ms-start_ms) when passed to the shot-search methods. Both bounds are
// inclusive.
type FacetFilter struct {
	AssetTypes    []string `json:"asset_types,omitempty"`
	ShotSizes     []string `json:"shot_sizes,omitempty"`
	CameraMotions []string `json:"camera_motions,omitempty"`
	AudioTypes    []string `json:"audio_types,omitempty"`
	Qualities     []string `json:"qualities,omitempty"`
	UsableAs      []string `json:"usable_as,omitempty"`
	MinDurationMS *int64   `json:"min_duration_ms,omitempty"`
	MaxDurationMS *int64   `json:"max_duration_ms,omitempty"`
}
