package domain

import "time"

// AssetShot is a time-bounded semantic observation produced by a video
// understanding run. It is intentionally separate from StructuredAnalysis:
// the latter describes the whole asset, while AssetShot is searchable footage
// that a future repurpose plan can reference.
type AssetShot struct {
	ID          string    `json:"id"`
	AssetID     string    `json:"asset_id"`
	SourceRunID string    `json:"source_run_id"`
	Ordinal     int       `json:"ordinal"`
	StartMS     int64     `json:"start_ms"`
	EndMS       int64     `json:"end_ms"`
	Description string    `json:"description"`
	Tags        []string  `json:"tags,omitempty"`
	Objects     []string  `json:"objects,omitempty"`
	Actions     []string  `json:"actions,omitempty"`
	Mood        []string  `json:"mood,omitempty"`
	Confidence  float64   `json:"confidence"`
	CreatedAt   time.Time `json:"created_at"`
}

type ShotSearchResult struct {
	AssetShot
	// Filename is the owning asset's name, joined in by the search query so a
	// shot result can be shown to a human without a second round trip.
	Filename      string  `json:"filename,omitempty"`
	Score         float64 `json:"score"`
	LexicalScore  float64 `json:"lexical_score,omitempty"`
	SemanticScore float64 `json:"semantic_score,omitempty"`
}

// RareShot is a library-relative discovery recommendation. Rarity describes
// how uncommon a shot's semantic signature is inside this library, not an
// absolute quality judgement.
type RareShot struct {
	ShotSearchResult
	RarityScore float64 `json:"rarity_score"`
	Reason      string  `json:"reason"`
}
