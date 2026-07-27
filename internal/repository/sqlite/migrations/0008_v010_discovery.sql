CREATE TABLE shot_semantic_vectors (
    shot_id TEXT PRIMARY KEY REFERENCES asset_shots(id) ON DELETE CASCADE,
    model TEXT NOT NULL,
    vector_json TEXT NOT NULL,
    source_text TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_shot_semantic_vectors_model ON shot_semantic_vectors(model);
