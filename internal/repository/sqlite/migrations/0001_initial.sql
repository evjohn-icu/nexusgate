CREATE TABLE library_roots (
    id TEXT PRIMARY KEY,
    path TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE assets (
    id TEXT PRIMARY KEY,
    quick_fingerprint TEXT NOT NULL,
    full_hash TEXT,
    file_size INTEGER NOT NULL,
    state TEXT NOT NULL,
    first_seen_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    missing_since TEXT
);
CREATE INDEX idx_assets_quick_fingerprint ON assets(quick_fingerprint, file_size);
CREATE INDEX idx_assets_state ON assets(state);

CREATE TABLE asset_locations (
    id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    root_id TEXT NOT NULL REFERENCES library_roots(id) ON DELETE CASCADE,
    relative_path TEXT NOT NULL,
    absolute_path TEXT NOT NULL,
    file_id TEXT,
    modified_ns INTEGER NOT NULL,
    exists_now INTEGER NOT NULL DEFAULT 1,
    is_primary INTEGER NOT NULL DEFAULT 1,
    last_seen_at TEXT NOT NULL,
    UNIQUE(root_id, relative_path)
);
CREATE INDEX idx_asset_locations_asset ON asset_locations(asset_id);
CREATE INDEX idx_asset_locations_exists ON asset_locations(root_id, exists_now);

CREATE TABLE media_metadata (
    asset_id TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
    ffprobe_json TEXT NOT NULL,
    exiftool_json TEXT NOT NULL,
    normalized_json TEXT NOT NULL,
    probe_version TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE derived_artifacts (
    id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    artifact_type TEXT NOT NULL,
    profile_hash TEXT NOT NULL,
    local_path TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(asset_id, artifact_type, profile_hash)
);

CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    asset_id TEXT REFERENCES assets(id) ON DELETE CASCADE,
    job_type TEXT NOT NULL,
    state TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    run_after TEXT NOT NULL,
    lease_owner TEXT,
    lease_expires_at TEXT,
    input_hash TEXT NOT NULL,
    last_error_code TEXT,
    last_error_message TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(asset_id, job_type, input_hash)
);
CREATE INDEX idx_jobs_ready ON jobs(state, run_after, priority);
