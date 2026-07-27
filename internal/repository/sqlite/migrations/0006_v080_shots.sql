CREATE TABLE asset_shots (
    id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    source_run_id TEXT REFERENCES model_runs(id) ON DELETE SET NULL,
    ordinal INTEGER NOT NULL,
    start_ms INTEGER NOT NULL,
    end_ms INTEGER NOT NULL,
    description TEXT NOT NULL,
    tags_json TEXT NOT NULL,
    objects_json TEXT NOT NULL,
    actions_json TEXT NOT NULL,
    mood_json TEXT NOT NULL,
    confidence REAL NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(asset_id, source_run_id, ordinal)
);
CREATE INDEX idx_asset_shots_asset_time ON asset_shots(asset_id, start_ms, end_ms);

CREATE VIRTUAL TABLE asset_shot_search USING fts5(
    shot_id UNINDEXED,
    asset_id UNINDEXED,
    description,
    tags,
    objects,
    actions,
    mood,
    tokenize = 'unicode61'
);
