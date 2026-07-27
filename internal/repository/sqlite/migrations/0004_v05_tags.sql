CREATE TABLE tag_catalog (
    id TEXT PRIMARY KEY,
    canonical_name TEXT NOT NULL UNIQUE,
    display_name_zh TEXT,
    display_name_en TEXT,
    category TEXT NOT NULL DEFAULT 'general',
    parent_id TEXT REFERENCES tag_catalog(id),
    status TEXT NOT NULL DEFAULT 'active',
    created_by TEXT NOT NULL DEFAULT 'system',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE tag_aliases_v2 (
    alias_normalized TEXT PRIMARY KEY,
    alias_display TEXT NOT NULL,
    canonical_tag_id TEXT NOT NULL REFERENCES tag_catalog(id) ON DELETE CASCADE,
    source TEXT NOT NULL,
    confidence REAL,
    created_at TEXT NOT NULL
);

CREATE TABLE asset_tag_links (
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    raw_tag TEXT NOT NULL,
    normalized_tag TEXT NOT NULL,
    canonical_tag_id TEXT REFERENCES tag_catalog(id),
    tag_type TEXT NOT NULL,
    source TEXT NOT NULL,
    source_run_id TEXT,
    confidence REAL,
    user_confirmed INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(asset_id, normalized_tag, tag_type, source)
);
CREATE INDEX idx_asset_tag_links_canonical ON asset_tag_links(canonical_tag_id, asset_id);
CREATE INDEX idx_asset_tag_links_unresolved ON asset_tag_links(canonical_tag_id, normalized_tag);

CREATE TABLE tag_curation_runs (
    id TEXT PRIMARY KEY,
    state TEXT NOT NULL,
    strategy TEXT NOT NULL,
    input_revision TEXT NOT NULL,
    stats_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    finished_at TEXT
);

CREATE TABLE tag_change_proposals (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES tag_curation_runs(id) ON DELETE CASCADE,
    state TEXT NOT NULL,
    proposal_type TEXT NOT NULL,
    canonical_name TEXT,
    payload_json TEXT NOT NULL,
    confidence REAL NOT NULL,
    reason TEXT NOT NULL,
    affected_assets INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    reviewed_at TEXT,
    review_note TEXT
);
CREATE INDEX idx_tag_proposals_state ON tag_change_proposals(state, created_at);

CREATE TABLE tag_library_summaries (
    canonical_tag_id TEXT PRIMARY KEY REFERENCES tag_catalog(id) ON DELETE CASCADE,
    asset_count INTEGER NOT NULL,
    summary TEXT NOT NULL,
    stats_json TEXT NOT NULL,
    input_revision TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
