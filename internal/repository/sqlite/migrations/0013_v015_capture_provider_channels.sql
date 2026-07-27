-- v0.15 keeps source capture facts separate from model analysis.  The raw
-- provider secret is intentionally never persisted in SQLite: members carry
-- only a reference resolved by the Hub-only secret store.

CREATE TABLE capture_metadata (
    asset_id TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
    vendor TEXT NOT NULL DEFAULT '',
    make TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    device_serial TEXT NOT NULL DEFAULT '',
    device_class TEXT NOT NULL DEFAULT '',
    captured_at TEXT,
    timezone_offset_minutes INTEGER,
    capture_time_source TEXT NOT NULL DEFAULT '',
    capture_time_confidence REAL NOT NULL DEFAULT 0,
    latitude REAL,
    longitude REAL,
    location_source TEXT NOT NULL DEFAULT '',
    location_precision TEXT NOT NULL DEFAULT '',
    region_label TEXT NOT NULL DEFAULT '',
    reel TEXT NOT NULL DEFAULT '',
    clip TEXT NOT NULL DEFAULT '',
    card TEXT NOT NULL DEFAULT '',
    scene TEXT NOT NULL DEFAULT '',
    take TEXT NOT NULL DEFAULT '',
    source_timecode TEXT NOT NULL DEFAULT '',
    session_marker TEXT NOT NULL DEFAULT '',
    source_color TEXT NOT NULL DEFAULT '',
    color_profile TEXT NOT NULL DEFAULT '',
    raw_format TEXT NOT NULL DEFAULT '',
    preview_status TEXT NOT NULL DEFAULT '',
    lut_id TEXT NOT NULL DEFAULT '',
    lut_version TEXT NOT NULL DEFAULT '',
    normalized_json TEXT NOT NULL DEFAULT '{}',
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_capture_metadata_time ON capture_metadata(captured_at);
CREATE INDEX idx_capture_metadata_camera ON capture_metadata(vendor, model, device_serial);
CREATE INDEX idx_capture_metadata_region ON capture_metadata(region_label);

CREATE TABLE capture_evidence (
    id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    field_name TEXT NOT NULL,
    observed_value TEXT NOT NULL,
    source TEXT NOT NULL,
    source_key TEXT NOT NULL DEFAULT '',
    priority INTEGER NOT NULL,
    confidence REAL NOT NULL DEFAULT 0,
    conflict INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_capture_evidence_asset_field ON capture_evidence(asset_id, field_name, priority DESC);

CREATE TABLE capture_sidecars (
    id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    relative_path TEXT NOT NULL,
    sidecar_kind TEXT NOT NULL,
    match_rule TEXT NOT NULL,
    evidence_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    UNIQUE(asset_id, relative_path)
);

CREATE TABLE shoot_sessions (
    id TEXT PRIMARY KEY,
    root_id TEXT REFERENCES library_roots(id) ON DELETE SET NULL,
    title TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'automatic',
    starts_at TEXT,
    ends_at TEXT,
    region_label TEXT NOT NULL DEFAULT '',
    camera_label TEXT NOT NULL DEFAULT '',
    confidence REAL NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_shoot_sessions_browse ON shoot_sessions(starts_at DESC, region_label, camera_label);

CREATE TABLE asset_shoot_sessions (
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL REFERENCES shoot_sessions(id) ON DELETE CASCADE,
    is_primary INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    PRIMARY KEY(asset_id, session_id)
);
CREATE INDEX idx_asset_shoot_sessions_session ON asset_shoot_sessions(session_id, is_primary DESC);

CREATE TABLE provider_channels (
    id TEXT PRIMARY KEY,
    capability TEXT NOT NULL,
    label TEXT NOT NULL,
    provider_name TEXT NOT NULL,
    protocol TEXT NOT NULL DEFAULT '',
    endpoint TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL DEFAULT 1,
    route_order INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(capability, label)
);
CREATE INDEX idx_provider_channels_route ON provider_channels(capability, enabled, route_order);

CREATE TABLE provider_channel_members (
    id TEXT PRIMARY KEY,
    channel_id TEXT NOT NULL REFERENCES provider_channels(id) ON DELETE CASCADE,
    label TEXT NOT NULL,
    secret_ref TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    weight INTEGER NOT NULL DEFAULT 1,
    max_inflight INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(channel_id, label),
    UNIQUE(secret_ref)
);
CREATE INDEX idx_provider_channel_members_ready ON provider_channel_members(channel_id, enabled);

CREATE TABLE provider_channel_events (
    id TEXT PRIMARY KEY,
    channel_id TEXT NOT NULL REFERENCES provider_channels(id) ON DELETE CASCADE,
    member_id TEXT REFERENCES provider_channel_members(id) ON DELETE SET NULL,
    event_type TEXT NOT NULL,
    error_class TEXT NOT NULL DEFAULT '',
    http_status INTEGER,
    latency_ms INTEGER,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_provider_channel_events_recent ON provider_channel_events(channel_id, created_at DESC);
