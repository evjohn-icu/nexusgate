PRAGMA foreign_keys=OFF;

CREATE TABLE model_runs_new (
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
    committed_at TEXT
);
INSERT INTO model_runs_new SELECT id,asset_id,capability,provider,model,input_hash,prompt_version,schema_version,state,request_json,raw_response,parsed_json,validation_errors,error_code,error_message,token_input,token_output,started_at,finished_at,committed_at FROM model_runs;
DROP TABLE model_runs;
ALTER TABLE model_runs_new RENAME TO model_runs;

CREATE TABLE asset_analysis_new (
    asset_id TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
    source_run_id TEXT REFERENCES model_runs(id) ON DELETE SET NULL,
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
INSERT INTO asset_analysis_new SELECT asset_id,source_run_id,schema_version,asset_type,shot_size,camera_motion,audio_type,lighting,people_count,has_speech,quality,summary,scene_tags_json,subjects_json,mood_tags_json,usable_as_json,quality_flags_json,extra_tags_json,editorial_reason,updated_at FROM asset_analysis;
DROP TABLE asset_analysis;
ALTER TABLE asset_analysis_new RENAME TO asset_analysis;

CREATE INDEX idx_model_runs_asset ON model_runs(asset_id, capability, state);
CREATE INDEX idx_model_runs_cache ON model_runs(capability, input_hash, state);
CREATE UNIQUE INDEX idx_model_runs_dedup ON model_runs(capability, provider, model, input_hash, prompt_version, schema_version) WHERE state != 'failed';
PRAGMA foreign_keys=ON;
