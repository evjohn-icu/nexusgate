-- v0.16 saved library collections contain only non-secret browse filters.
-- Filesystem paths, exact coordinates, provider credentials, and raw media
-- metadata are intentionally outside this table.
CREATE TABLE asset_collections (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    filter_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_asset_collections_updated ON asset_collections(updated_at DESC);

CREATE INDEX idx_jobs_asset_state ON jobs(asset_id, state);
CREATE INDEX idx_derived_artifacts_asset_type ON derived_artifacts(asset_id, artifact_type);
