-- 0036_multimodal_timeline: independent modality generations and their rows.
--
-- Every collection here is produced by one operation on one asset under one
-- config hash. A generation is staged first and only becomes the head's answer
-- after complete validation, so a failed replacement never erases the previous
-- committed data. All intervals are half-open integer milliseconds relative to
-- the asset, matching domain.TimeInterval.

CREATE TABLE inference_tasks (
    job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    task_key TEXT NOT NULL UNIQUE,
    operation TEXT NOT NULL,
    request_json TEXT NOT NULL,
    model_run_id TEXT REFERENCES model_runs(id) ON DELETE SET NULL,
    query_reference_id TEXT,
    -- staged_json holds the validated inference result minus any biometric
    -- payload. Speaker vectors never appear here; they go to the encrypted
    -- secret store and are referenced from speaker_embedding_staging.
    staged_json TEXT,
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    config_hash TEXT NOT NULL,
    model_revision TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_inference_tasks_operation ON inference_tasks(operation);
CREATE INDEX idx_inference_tasks_created ON inference_tasks(created_at);

-- Assetless inference (text/image queries) has no asset_id, so the legacy
-- UNIQUE(asset_id, job_type, input_hash) never deduplicates it. task_key stays
-- authoritative; this partial index is an additional guard for the same
-- identity reaching the enqueue path twice with different task keys.
CREATE UNIQUE INDEX idx_jobs_assetless_inference_unique
    ON jobs(job_type, input_hash)
    WHERE asset_id IS NULL AND job_type = 'inference';

CREATE TABLE embedding_spaces (
    id TEXT PRIMARY KEY,
    modality TEXT NOT NULL,
    provider TEXT NOT NULL,
    model_id TEXT NOT NULL,
    model_revision TEXT NOT NULL,
    dimension INTEGER NOT NULL CHECK (dimension > 0),
    normalization TEXT NOT NULL,
    preprocessing_json TEXT NOT NULL,
    runtime_digest TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE modality_generations (
    id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    operation TEXT NOT NULL,
    model_run_id TEXT REFERENCES model_runs(id) ON DELETE SET NULL,
    input_hash TEXT NOT NULL,
    config_hash TEXT NOT NULL,
    space_id TEXT REFERENCES embedding_spaces(id),
    -- Visual generations additionally pin the exact canonical shot set they
    -- were computed from. A commit whose shot_set_hash no longer matches the
    -- asset is rejected instead of mixing vectors across boundaries.
    shot_set_hash TEXT,
    state TEXT NOT NULL CHECK (state IN ('staged','committed','failed')),
    processed_intervals_json TEXT NOT NULL DEFAULT '[]',
    failure_intervals_json TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_modality_generations_asset ON modality_generations(asset_id, operation, config_hash);

CREATE TABLE modality_heads (
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    operation TEXT NOT NULL,
    config_hash TEXT NOT NULL,
    generation_id TEXT NOT NULL REFERENCES modality_generations(id) ON DELETE CASCADE,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (asset_id, operation, config_hash)
);

CREATE TABLE audio_events (
    id TEXT PRIMARY KEY,
    generation_id TEXT NOT NULL REFERENCES modality_generations(id) ON DELETE CASCADE,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    start_ms INTEGER NOT NULL,
    end_ms INTEGER NOT NULL,
    event_label TEXT NOT NULL,
    confidence REAL,
    raw_score REAL NOT NULL,
    evidence_status TEXT NOT NULL CHECK (evidence_status IN ('possible','confirmed','unknown')),
    boundary_resolution_ms INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    CHECK (start_ms >= 0 AND end_ms > start_ms)
);
CREATE INDEX idx_audio_events_asset_time ON audio_events(asset_id, start_ms, end_ms);
CREATE INDEX idx_audio_events_label_time ON audio_events(event_label, asset_id, start_ms);

CREATE TABLE audio_embeddings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    generation_id TEXT NOT NULL REFERENCES modality_generations(id) ON DELETE CASCADE,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    space_id TEXT NOT NULL REFERENCES embedding_spaces(id),
    start_ms INTEGER NOT NULL,
    end_ms INTEGER NOT NULL,
    vector BLOB NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (generation_id, start_ms, end_ms, space_id),
    CHECK (start_ms >= 0 AND end_ms > start_ms)
);
CREATE INDEX idx_audio_embeddings_space ON audio_embeddings(space_id, asset_id);

CREATE TABLE visual_embeddings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    generation_id TEXT NOT NULL REFERENCES modality_generations(id) ON DELETE CASCADE,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    shot_id TEXT NOT NULL REFERENCES asset_shots(id) ON DELETE CASCADE,
    space_id TEXT NOT NULL REFERENCES embedding_spaces(id),
    -- -1 is the aggregate record (pooled shot mean or native-video clip);
    -- non-negative indices are individual sampled frames.
    sample_index INTEGER NOT NULL,
    timestamp_ms INTEGER NOT NULL DEFAULT 0,
    strategy TEXT NOT NULL,
    source_hash TEXT NOT NULL DEFAULT '',
    vector BLOB NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (generation_id, shot_id, space_id, sample_index)
);
CREATE INDEX idx_visual_embeddings_space ON visual_embeddings(space_id, asset_id);
CREATE INDEX idx_visual_embeddings_shot ON visual_embeddings(shot_id, space_id);

CREATE TABLE speaker_turns (
    id TEXT PRIMARY KEY,
    generation_id TEXT NOT NULL REFERENCES modality_generations(id) ON DELETE CASCADE,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    start_ms INTEGER NOT NULL,
    end_ms INTEGER NOT NULL,
    local_speaker_id TEXT,
    confidence REAL,
    overlap INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    CHECK (start_ms >= 0 AND end_ms > start_ms)
);
CREATE INDEX idx_speaker_turns_asset_time ON speaker_turns(asset_id, start_ms, end_ms);
CREATE INDEX idx_speaker_turns_generation ON speaker_turns(generation_id);

-- Links are recomputable: they record which transcript unit a diarized turn
-- covers, not a new transcription. word_index uses -1 as the non-null sentinel
-- for segment-only links so the composite prefix key stays usable.
CREATE TABLE speaker_transcript_links (
    turn_id TEXT NOT NULL REFERENCES speaker_turns(id) ON DELETE CASCADE,
    transcript_source TEXT NOT NULL CHECK (transcript_source IN ('asr','aligned')),
    transcript_generation_hash TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    word_index INTEGER NOT NULL DEFAULT -1,
    overlap_ms INTEGER NOT NULL,
    PRIMARY KEY (turn_id, transcript_source, transcript_generation_hash, ordinal, word_index)
);
CREATE INDEX idx_speaker_transcript_links_hash ON speaker_transcript_links(transcript_generation_hash);

CREATE TABLE multimodal_settings (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    settings_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
