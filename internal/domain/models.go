package domain

import "time"

// RootHealthState is the persisted health verdict of a library root. The scan
// service decides the verdict; this type only carries it across restarts.
type RootHealthState string

const (
	// RootHealthUnknown means the root was never scanned or the state was lost
	// (the default for rows created before health tracking existed).
	RootHealthUnknown RootHealthState = "unknown"
	// RootHealthHealthy means the last scan actually walked a real, mounted root.
	RootHealthHealthy RootHealthState = "healthy"
	// RootHealthUnavailable means the root path or mount was unreachable, so
	// missing-file reconciliation is paused until a scan reaches it again.
	RootHealthUnavailable RootHealthState = "unavailable"
)

type LibraryRoot struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// HealthState gates reconciliation: a scan must not mark assets missing
	// while the root itself is unknown or unavailable.
	HealthState RootHealthState `json:"health_state"`
	// LastHealthyAt is the last scan that confirmed a mounted root; never
	// cleared by an unavailable scan, so the UI can show "last healthy".
	LastHealthyAt *time.Time `json:"last_healthy_at,omitempty"`
	// LastScanAt is the last time any scan walk of this root finished (or
	// failed to start because the root was unreachable).
	LastScanAt *time.Time `json:"last_scan_at,omitempty"`
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
	ID           string `json:"id"`
	AssetID      string `json:"asset_id"`
	RootID       string `json:"root_id"`
	RelativePath string `json:"relative_path"`
	AbsolutePath string `json:"absolute_path"`
	FileID       string `json:"file_id,omitempty"`
	ModifiedNS   int64  `json:"modified_ns"`
	// QuickFingerprint and FileSize are the owning asset's identity, joined
	// into the location by GetPrimaryLocation so the pipeline can derive a
	// path-independent probe input hash from the primary location alone.
	// They describe the content, never the path a file happens to sit at.
	QuickFingerprint string `json:"quick_fingerprint"`
	FileSize         int64  `json:"file_size"`
	// ProbeModifiedNS is asset-level identity state, not the mtime of this
	// particular copy.
	ProbeModifiedNS int64     `json:"-"`
	Exists          bool      `json:"exists"`
	IsPrimary       bool      `json:"is_primary"`
	LastSeenAt      time.Time `json:"last_seen_at"`
}

// ScanResult is what one scan walk reports. Discovered/Linked/Missing count
// video files; the Skipped* fields report the non-video files the walk
// ignored so a scan that finds nothing still says why.
type ScanResult struct {
	Discovered int      `json:"discovered"`
	Linked     int      `json:"linked"`
	Missing    int      `json:"missing"`
	Errors     []string `json:"errors"`
	// SkippedFiles is how many regular non-video files the walk ignored.
	// SkippedExtensions names the first 20 distinct normalized extension types
	// among them; SkippedOther folds every further distinct type so a hostile
	// directory cannot grow an unbounded map. SupportedExtensions is the
	// declaration-ordered list of video extensions the scanner accepts, so a
	// report tells the operator what the root actually held.
	SkippedFiles        int      `json:"skipped_files"`
	SkippedExtensions   []string `json:"skipped_extensions"`
	SkippedOther        int      `json:"skipped_other"`
	SupportedExtensions []string `json:"supported_extensions"`
	// SeenRelativePaths is every video path the walk reached, in walk order.
	// The scanner only reports it; the service decides whether the list may
	// be used as the reconciliation input for MarkUnseenLocationsMissing (the
	// root-health gate in internal/app). Hidden from JSON for the same reason
	// as ChangedAssetIDs: ScanResult is the response body of
	// POST /library-roots/{id}/scan and must keep its documented shape.
	SeenRelativePaths []string `json:"-"`
	// ChangedAssetIDs carries the assets a scan actually changed so the caller
	// can enqueue only those. Hidden from JSON: ScanResult is the response body
	// of POST /library-roots/{id}/scan and must keep its documented shape.
	ChangedAssetIDs []string `json:"-"`
	Complete        bool     `json:"-"`
	RootReachable   bool     `json:"-"`
}

// KnownFile is what a previous scan already recorded for one path in one
// library root: the identity it computed, and the two facts that identity was
// computed from. It exists so a re-scan can decide whether the file on disk is
// still the file behind that identity without opening it — QuickFingerprint
// reads up to 12 MiB per file, which over a NAS is the whole cost of a scan.
//
// Size and ModifiedNS are the comparison; Fingerprint is what may be reused
// when both still match. Nothing here is authoritative — it is a cache of the
// scanner's own previous work, and a miss costs only the read it would have
// done anyway.
type KnownFile struct {
	Fingerprint string
	Size        int64
	ModifiedNS  int64
}

// ScannedFile reports what one scanned file did to the catalog, so the scanner
// can tell a no-op revisit from a change the pipeline needs to re-derive.
type ScannedFile struct {
	AssetID string
	Created bool // a new asset row was inserted
	Changed bool // this scan changed something the pipeline keys on
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
