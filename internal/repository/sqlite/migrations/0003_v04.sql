CREATE TABLE provider_files (
    id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    artifact_type TEXT NOT NULL,
    profile_hash TEXT NOT NULL,
    provider TEXT NOT NULL,
    remote_name TEXT,
    remote_uri TEXT NOT NULL,
    mime_type TEXT NOT NULL,
    state TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    last_used_at TEXT NOT NULL,
    expires_at TEXT,
    error_message TEXT,
    UNIQUE(asset_id, artifact_type, profile_hash, provider)
);
CREATE INDEX idx_provider_files_lookup ON provider_files(asset_id, provider, state);

CREATE TABLE alignment_runs (
    id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    transcript_id TEXT,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    input_hash TEXT NOT NULL,
    state TEXT NOT NULL,
    request_json TEXT NOT NULL,
    raw_response TEXT,
    error_message TEXT,
    created_at TEXT NOT NULL,
    finished_at TEXT,
    UNIQUE(asset_id, provider, model, input_hash)
);

CREATE TABLE transcript_words (
    alignment_run_id TEXT NOT NULL REFERENCES alignment_runs(id) ON DELETE CASCADE,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    start_ms INTEGER NOT NULL,
    end_ms INTEGER NOT NULL,
    text TEXT NOT NULL,
    confidence REAL,
    PRIMARY KEY(alignment_run_id, ordinal)
);
CREATE INDEX idx_transcript_words_asset_time ON transcript_words(asset_id, start_ms);
