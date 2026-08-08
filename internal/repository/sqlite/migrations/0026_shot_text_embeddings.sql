-- v0.28: text-embedding storage for the Search v2 retrieval layer.
--
-- shot_text_embeddings holds DERIVED representations only: the canonical
-- truth stays in asset_shots (written by VLM analysis). One row per shot,
-- exactly like shot_semantic_vectors: switching the embedding model
-- overwrites the per-shot row (the new model's vectors), and the rebuild is
-- a re-embedding of the derived text — never a re-analysis. The vector is
-- stored as a float32 little-endian blob: embedding dimensions (256-1536)
-- make JSON text unbearable for a full-library cosine scan.
--
-- source_text_hash is the rebuild trigger: an incremental rebuild embeds
-- only shots whose derived text (description+tags+objects+actions+mood)
-- hash changed, so a normal analysis commit re-embeds at most a handful of
-- shots.
CREATE TABLE shot_text_embeddings (
    shot_id TEXT PRIMARY KEY REFERENCES asset_shots(id) ON DELETE CASCADE,
    model TEXT NOT NULL,
    vector_blob BLOB NOT NULL,
    source_text_hash TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_shot_text_embeddings_model ON shot_text_embeddings(model);
