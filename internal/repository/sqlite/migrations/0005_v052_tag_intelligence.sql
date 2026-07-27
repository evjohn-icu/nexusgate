CREATE TABLE tag_embeddings (
    normalized_tag TEXT NOT NULL,
    model TEXT NOT NULL,
    vector_json TEXT NOT NULL,
    usage_count INTEGER NOT NULL,
    asset_count INTEGER NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(normalized_tag, model)
);

CREATE TABLE tag_embedding_runs (
    id TEXT PRIMARY KEY,
    state TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    similarity_threshold REAL NOT NULL,
    input_revision TEXT NOT NULL,
    stats_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    finished_at TEXT
);

CREATE TABLE tag_clusters (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES tag_embedding_runs(id) ON DELETE CASCADE,
    state TEXT NOT NULL DEFAULT 'candidate',
    member_tags_json TEXT NOT NULL,
    average_similarity REAL NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_tag_clusters_run ON tag_clusters(run_id);

CREATE TABLE library_summaries (
    id TEXT PRIMARY KEY,
    scope TEXT NOT NULL DEFAULT 'all',
    summary TEXT NOT NULL,
    themes_json TEXT NOT NULL,
    suitable_for_json TEXT NOT NULL,
    input_json TEXT NOT NULL,
    input_revision TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    generated_at TEXT NOT NULL
);
CREATE INDEX idx_library_summaries_scope ON library_summaries(scope, generated_at DESC);
