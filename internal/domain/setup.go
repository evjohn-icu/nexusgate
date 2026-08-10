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
	ProviderCount   int   `json:"provider_count"`
	AssetCount      int   `json:"asset_count"`
	// NextStep is one of "add_footage", "configure_providers",
	// "scan_or_process", "search" or "ready" — see SetupNextStep* in
	// internal/app for the ordering and why it is a heuristic.
	NextStep string `json:"next_step"`
	Ready    bool   `json:"ready"`
}
