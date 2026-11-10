-- 0038_search_references: Hub-managed query images for visual search.
--
-- The Hub issues the reference id, decodes and re-encodes the image to strip
-- metadata, and stores it under a generated cache name. Callers never supply a
-- path or URL, and an expired reference takes its dependent query tasks with it.

CREATE TABLE search_references (
    id TEXT PRIMARY KEY,
    content_hash TEXT NOT NULL,
    media_type TEXT NOT NULL CHECK (media_type IN ('image/jpeg','image/png')),
    relative_cache_path TEXT NOT NULL,
    width INTEGER NOT NULL DEFAULT 0,
    height INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE INDEX idx_search_references_expiry ON search_references(expires_at);
CREATE INDEX idx_search_references_content ON search_references(content_hash);

-- 0039_model_management: install/probe work queued through the jobs table.
--
-- These are maintenance jobs, not inference.v1 operations. The row records the
-- catalog identity, manifest digest and licence acceptance that authorized the
-- download; a Worker can only report a probe attestation for a row it owns.
CREATE TABLE model_management_tasks (
    job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    target_worker_id TEXT NOT NULL,
    catalog_id TEXT NOT NULL,
    manifest_digest TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('install','probe')),
    license_accepted_at TEXT,
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- Successful authorized install/probe attestations. Only a locked profile with
-- a matching attestation is routable; a heartbeat may downgrade availability
-- but can never create a row here.
CREATE TABLE model_attestations (
    worker_id TEXT NOT NULL,
    catalog_id TEXT NOT NULL,
    manifest_digest TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('install','probe')),
    verified_at TEXT NOT NULL,
    detail_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY (worker_id, catalog_id, manifest_digest, action)
);
