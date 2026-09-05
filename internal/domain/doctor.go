package domain

import "time"

// Version is the product release the SYSTEM section of a DoctorReport names.
// The root VERSION file is the release declaration. Release builds copy it
// here with -ldflags; an unreleased source build reports "(devel)".
var Version = "(devel)"

// DoctorReport is the JSON-safe, secret-free diagnostic snapshot `nexusgate
// doctor` collects and prints. Every field is a primitive, a path, or a
// derived status; nothing here can hold a provider key, a token, or a secret
// reference, so the whole value is safe to marshal and ship to an operator.
type DoctorReport struct {
	System      SystemInfo      `json:"system"`
	DB          DBInfo          `json:"db"`
	Storage     StorageInfo     `json:"storage"`
	Roots       []RootInfo      `json:"roots"`
	FFmpeg      FFmpegInfo      `json:"ffmpeg"`
	FFprobe     ToolInfo        `json:"ffprobe"`
	ExifTool    ToolInfo        `json:"exiftool"`
	GPU         GPUInfo         `json:"gpu"`
	Providers   ProviderInfo    `json:"providers"`
	SearchIndex SearchIndexInfo `json:"search_index"`
	Workers     []WorkerInfo    `json:"workers"`
	Queue       QueueInfo       `json:"queue"`
	TempFiles   TempFilesInfo   `json:"temp_files"`
}

// SystemInfo identifies the process running the Hub.
type SystemInfo struct {
	Version  string `json:"version"`
	GoOS     string `json:"goos"`
	GoArch   string `json:"goarch"`
	Hostname string `json:"hostname"`
}

// DBInfo is the SQLite self-check verdict and the migration ledger view.
// SchemaVersion is the filename of the last applied migration (e.g.
// "0028_v028_text_embeddings.sql").
type DBInfo struct {
	Path              string `json:"path"`
	IntegrityOK       bool   `json:"integrity_ok"`
	MigrationsApplied int    `json:"migrations_applied"`
	MigrationsTotal   int    `json:"migrations_total"`
	SchemaVersion     string `json:"schema_version"`
}

// StorageInfo sizes the Hub's own state: the cache directory (derived
// artifacts, staged sources), the database file, and the bytes stuck in stale
// scratch files. FreeDiskBytes is -1 when the filesystem probe failed.
type StorageInfo struct {
	CachePath     string `json:"cache_path"`
	FreeDiskBytes int64  `json:"free_disk_bytes"`
	CacheBytes    int64  `json:"cache_bytes"`
	DBSizeBytes   int64  `json:"db_size_bytes"`
	TempBytes     int64  `json:"temp_bytes"`
}

// RootInfo is one library root as doctor sees it: the persisted health
// verdict (RootHealthState as a string), the filesystem it sits on when the
// mount table is readable, and the same advice RootWarnings renders for a
// registered root.
type RootInfo struct {
	Path           string     `json:"path"`
	State          string     `json:"state"`
	FilesystemType string     `json:"filesystem_type,omitempty"`
	LastHealthyAt  *time.Time `json:"last_healthy_at,omitempty"`
	Warnings       []string   `json:"warnings,omitempty"`
}

// FFmpegInfo carries the first line of `ffmpeg -version` when the binary is
// present, so the console can name the actual build an operator is running.
type FFmpegInfo struct {
	Path    string `json:"path,omitempty"`
	Present bool   `json:"present"`
	Version string `json:"version,omitempty"`
}

// ToolInfo is the presence check for a PATH-looked-up helper binary.
type ToolInfo struct {
	Path    string `json:"path,omitempty"`
	Present bool   `json:"present"`
}

// GPUInfo carries the rendered hardware report, so --json callers get exactly
// the same diagnosis the console section renders (media.FormatHardwareReport).
type GPUInfo struct {
	HardwareReport string `json:"hardware_report"`
}

// ProviderInfo is the configured-vs-healthy view of provider channels.
// Degraded counts routes with a member the pool has retired (401/402/403),
// parked in cooldown, or half-open — the runtime facts no configuration row
// records. See Service.ProviderChannelRuntimeStatus.
type ProviderInfo struct {
	Channels           int `json:"channels"`
	MembersWithSecrets int `json:"members_with_secrets"`
	Degraded           int `json:"degraded"`
	// ConfigValid and ConfigError record the legacy providers.* config
	// validation result. `doctor` deliberately runs even when that config is
	// broken (the pre-dispatch fail-fast is skipped for it), so the report
	// carries the secret-free reason instead of the command refusing to
	// produce any diagnostics.
	ConfigValid bool   `json:"config_valid"`
	ConfigError string `json:"config_error,omitempty"`
}

// SearchIndexInfo is the retrieval-layer health view. FTSReady is the
// cjk_bigram_v1 fts_index_state flag ('ready' true, 'pending' false, and a
// missing row counts as true — no index was needed). EmbeddingsShotCount is
// the number of distinct shots carrying a text-embedding vector.
type SearchIndexInfo struct {
	ShotCount           int  `json:"shot_count"`
	FTSReady            bool `json:"fts_ready"`
	EmbeddingsCount     int  `json:"embeddings_count"`
	EmbeddingsShotCount int  `json:"embeddings_shot_count"`
}

// WorkerInfo is the fleet view: what the Hub has enrolled, what it last said,
// and the derived online/offline verdict ListWorkers already computed.
type WorkerInfo struct {
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	Version    string     `json:"version,omitempty"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
}

// QueueInfo is JobSummary's six disjoint counts, minus Total (derivable).
type QueueInfo struct {
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Terminal  int `json:"terminal"`
	Deferred  int `json:"deferred"`
}

// TempFilesInfo counts stale scratch files in the cache directory: partial
// staging copies, leftover analysis-frames directories, and TLS rotation
// temp files older than 24 hours.
type TempFilesInfo struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// MigrationStatus is the schema_migrations view doctor renders: how many
// embedded migration files this binary knows, how many the library has
// applied, and the filename of the newest applied one ("" when none).
type MigrationStatus struct {
	Applied     int    `json:"applied"`
	Total       int    `json:"total"`
	LastApplied string `json:"last_applied"`
}

// SearchIndexHealth is the repository's read-only search-index view: total
// shot rows, text-embedding vector rows (and distinct shots carrying one), and
// the fts_index_state flag for cjk_bigram_v1 as "ready", "pending" or
// "missing".
type SearchIndexHealth struct {
	ShotCount           int    `json:"shot_count"`
	EmbeddingsCount     int    `json:"embeddings_count"`
	EmbeddingsShotCount int    `json:"embeddings_shot_count"`
	FTSState            string `json:"fts_state"`
}
