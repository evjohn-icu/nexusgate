package domain

import "time"

type LibraryRoot struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type AssetState string

const (
	AssetDiscovered AssetState = "discovered"
	AssetMissing    AssetState = "missing"
)

type Asset struct {
	ID               string     `json:"id"`
	QuickFingerprint string     `json:"quick_fingerprint"`
	FullHash         *string    `json:"full_hash,omitempty"`
	FileSize         int64      `json:"file_size"`
	State            AssetState `json:"state"`
	FirstSeenAt      time.Time  `json:"first_seen_at"`
	LastSeenAt       time.Time  `json:"last_seen_at"`
	MissingSince     *time.Time `json:"missing_since,omitempty"`
}

type AssetLocation struct {
	ID           string    `json:"id"`
	AssetID      string    `json:"asset_id"`
	RootID       string    `json:"root_id"`
	RelativePath string    `json:"relative_path"`
	AbsolutePath string    `json:"absolute_path"`
	FileID       string    `json:"file_id,omitempty"`
	ModifiedNS   int64     `json:"modified_ns"`
	Exists       bool      `json:"exists"`
	IsPrimary    bool      `json:"is_primary"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}

type ScanResult struct {
	Discovered int      `json:"discovered"`
	Linked     int      `json:"linked"`
	Missing    int      `json:"missing"`
	Errors     []string `json:"errors"`
}

type CanonicalTag struct {
	ID            string    `json:"id"`
	CanonicalName string    `json:"canonical_name"`
	DisplayNameZH string    `json:"display_name_zh,omitempty"`
	DisplayNameEN string    `json:"display_name_en,omitempty"`
	Category      string    `json:"category"`
	ParentID      string    `json:"parent_id,omitempty"`
	Status        string    `json:"status"`
	UsageCount    int       `json:"usage_count"`
	AliasCount    int       `json:"alias_count"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type UnresolvedTag struct {
	NormalizedTag string   `json:"normalized_tag"`
	DisplayForms  []string `json:"display_forms"`
	UsageCount    int      `json:"usage_count"`
	AssetCount    int      `json:"asset_count"`
	Examples      []string `json:"examples,omitempty"`
}

type TagProposal struct {
	ID             string         `json:"id"`
	RunID          string         `json:"run_id"`
	State          string         `json:"state"`
	ProposalType   string         `json:"proposal_type"`
	CanonicalName  string         `json:"canonical_name,omitempty"`
	Payload        map[string]any `json:"payload"`
	Confidence     float64        `json:"confidence"`
	Reason         string         `json:"reason"`
	AffectedAssets int            `json:"affected_assets"`
	CreatedAt      time.Time      `json:"created_at"`
	ReviewedAt     *time.Time     `json:"reviewed_at,omitempty"`
	ReviewNote     string         `json:"review_note,omitempty"`
}

type TagCurationResult struct {
	RunID             string `json:"run_id"`
	Strategy          string `json:"strategy"`
	UnresolvedScanned int    `json:"unresolved_scanned"`
	ProposalsCreated  int    `json:"proposals_created"`
}

// TagEmbedding is an immutable-ish cache of a provider-generated vector for a
// normalized raw tag. It is used to find curation candidates, never to rank
// footage search results.
type TagEmbedding struct {
	NormalizedTag string    `json:"normalized_tag"`
	Model         string    `json:"model"`
	Vector        []float64 `json:"vector"`
	UsageCount    int       `json:"usage_count"`
	AssetCount    int       `json:"asset_count"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type TagCluster struct {
	ID         string   `json:"id"`
	RunID      string   `json:"run_id"`
	Members    []string `json:"members"`
	Similarity float64  `json:"similarity"`
	State      string   `json:"state"`
}

type TagClusterResult struct {
	RunID            string `json:"run_id"`
	Strategy         string `json:"strategy"`
	TagsEmbedded     int    `json:"tags_embedded"`
	ClustersFound    int    `json:"clusters_found"`
	ProposalsCreated int    `json:"proposals_created"`
}

type LibraryTagStat struct {
	CanonicalName string `json:"canonical_name"`
	Category      string `json:"category"`
	AssetCount    int    `json:"asset_count"`
}

type LibrarySummaryInput struct {
	AssetCount    int              `json:"asset_count"`
	AnalyzedCount int              `json:"analyzed_count"`
	TopTags       []LibraryTagStat `json:"top_tags"`
}

type LibrarySummary struct {
	ID          string              `json:"id"`
	Scope       string              `json:"scope"`
	Summary     string              `json:"summary"`
	Themes      []string            `json:"themes"`
	SuitableFor []string            `json:"suitable_for"`
	Input       LibrarySummaryInput `json:"input"`
	Provider    string              `json:"provider"`
	Model       string              `json:"model"`
	GeneratedAt time.Time           `json:"generated_at"`
}

type LibrarySummaryDraft struct {
	Summary     string   `json:"summary"`
	Themes      []string `json:"themes"`
	SuitableFor []string `json:"suitable_for"`
}
