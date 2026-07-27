ALTER TABLE assets ADD COLUMN pipeline_state TEXT NOT NULL DEFAULT 'discovered';
ALTER TABLE assets ADD COLUMN last_error TEXT;

ALTER TABLE media_metadata ADD COLUMN duration_ms INTEGER;
ALTER TABLE media_metadata ADD COLUMN width INTEGER;
ALTER TABLE media_metadata ADD COLUMN height INTEGER;
ALTER TABLE media_metadata ADD COLUMN fps REAL;
ALTER TABLE media_metadata ADD COLUMN video_codec TEXT;
ALTER TABLE media_metadata ADD COLUMN audio_codec TEXT;
ALTER TABLE media_metadata ADD COLUMN has_audio INTEGER NOT NULL DEFAULT 0;
ALTER TABLE media_metadata ADD COLUMN orientation TEXT;
ALTER TABLE media_metadata ADD COLUMN captured_at TEXT;
ALTER TABLE media_metadata ADD COLUMN camera_model TEXT;
ALTER TABLE media_metadata ADD COLUMN latitude REAL;
ALTER TABLE media_metadata ADD COLUMN longitude REAL;

CREATE TABLE speech_classifications (
    asset_id TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
    classification TEXT NOT NULL,
    speech_probability REAL NOT NULL,
    reason TEXT NOT NULL,
    classifier_version TEXT NOT NULL,
    raw_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE transcripts (
    id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    input_hash TEXT NOT NULL,
    language TEXT,
    full_text TEXT NOT NULL,
    segments_json TEXT NOT NULL,
    raw_response TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(asset_id, input_hash)
);

-- Immutable cache/staging records. No model output is written directly into asset_analysis.
CREATE TABLE model_runs (
    id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    capability TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    input_hash TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    schema_version TEXT NOT NULL,
    state TEXT NOT NULL,
    request_json TEXT NOT NULL,
    raw_response TEXT,
    parsed_json TEXT,
    validation_errors TEXT,
    error_code TEXT,
    error_message TEXT,
    token_input INTEGER,
    token_output INTEGER,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    committed_at TEXT,
    UNIQUE(capability, provider, model, input_hash, prompt_version, schema_version)
);
CREATE INDEX idx_model_runs_asset ON model_runs(asset_id, capability, state);
CREATE INDEX idx_model_runs_cache ON model_runs(capability, input_hash, state);

CREATE TABLE asset_analysis (
    asset_id TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
    source_run_id TEXT NOT NULL REFERENCES model_runs(id),
    schema_version TEXT NOT NULL,
    asset_type TEXT NOT NULL,
    shot_size TEXT NOT NULL,
    camera_motion TEXT NOT NULL,
    audio_type TEXT NOT NULL,
    lighting TEXT NOT NULL,
    people_count INTEGER NOT NULL,
    has_speech INTEGER NOT NULL,
    quality TEXT NOT NULL,
    summary TEXT NOT NULL,
    scene_tags_json TEXT NOT NULL,
    subjects_json TEXT NOT NULL,
    mood_tags_json TEXT NOT NULL,
    usable_as_json TEXT NOT NULL,
    quality_flags_json TEXT NOT NULL,
    extra_tags_json TEXT NOT NULL,
    editorial_reason TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE asset_overrides (
    asset_id TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
    asset_type TEXT,
    shot_size TEXT,
    camera_motion TEXT,
    audio_type TEXT,
    lighting TEXT,
    has_speech INTEGER,
    quality TEXT,
    location_name TEXT,
    usable INTEGER,
    updated_at TEXT NOT NULL
);

CREATE TABLE asset_tags (
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    tag_type TEXT NOT NULL,
    tag_value TEXT NOT NULL,
    source TEXT NOT NULL,
    source_run_id TEXT,
    created_at TEXT NOT NULL,
    PRIMARY KEY(asset_id, tag_type, tag_value, source)
);

CREATE TABLE tag_suppressions (
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    tag_type TEXT NOT NULL,
    tag_value TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(asset_id, tag_type, tag_value)
);

CREATE TABLE favorites (
    asset_id TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
    is_favorite INTEGER NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE VIRTUAL TABLE asset_search USING fts5(
    asset_id UNINDEXED,
    filename,
    summary,
    transcript,
    scene_tags,
    subjects,
    mood_tags,
    extra_tags,
    location,
    editorial_reason,
    tokenize = 'unicode61'
);
