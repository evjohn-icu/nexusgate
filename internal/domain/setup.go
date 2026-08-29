package domain

// SetupStatus is the first-run environment snapshot behind the /setup guide:
// which helper binaries exist, whether the Hub's own directories are writable,
// how much disk is left, and the single most useful next step. It is a guide,
// not a gate — nothing in the Hub refuses to run because a field here is
// false, and Ready is only the heuristic's own conclusion, never a lock.
type SetupStatus struct {
	FFmpeg          bool  `json:"ffmpeg"`
	FFprobe         bool  `json:"ffprobe"`
	ExifTool        bool  `json:"exiftool"`
	DataDirWritable bool  `json:"data_dir_writable"`
	CacheWritable   bool  `json:"cache_writable"`
	FreeDiskBytes   int64 `json:"free_disk_bytes"`
	FreeDiskOK      bool  `json:"free_disk_ok"`
	DBHealthy       bool  `json:"db_healthy"`
	RootCount       int   `json:"root_count"`
	// HealthyRootCount is how many registered roots are currently healthy
	// (mounted and last-scanned OK), the count that actually makes scanning
	// runnable. RootCount stays as the inventory count for existing clients.
	HealthyRootCount int `json:"healthy_root_count"`
	ProviderCount    int `json:"provider_count"`
	// ProviderReady reports whether a video-understanding route can actually
	// run right now: an enabled, runtime-supported video_analysis channel with
	// an enabled secret-ready member, or an enabled legacy providers.* vision
	// route. ProviderCount stays as the inventory count for existing clients.
	ProviderReady bool `json:"provider_ready"`
	AssetCount    int  `json:"asset_count"`
	// SearchableShotCount is the canonical committed shot count and
	// SearchIndexReady whether the full-text index is ready (or never needed);
	// both come from the repository's search-index health view.
	SearchableShotCount int  `json:"searchable_shot_count"`
	SearchIndexReady    bool `json:"search_index_ready"`
	// NextStep is one of "add_footage", "configure_providers",
	// "scan_or_process", "search" or "ready" — see SetupNextStep* in
	// internal/app for the ordering and why it is a heuristic.
	NextStep  string `json:"next_step"`
	Ready     bool   `json:"ready"`
	AdminAuth string `json:"admin_auth,omitempty"`
}
