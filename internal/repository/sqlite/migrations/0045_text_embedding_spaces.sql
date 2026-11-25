-- 0045_text_embedding_spaces: text embeddings carry provenance.
--
-- Before this migration a shot's text embedding was keyed by shot_id alone
-- and its `model` column held whatever display name the provider reported.
-- With provider channels a single embedding operation can fall back from the
-- primary route to a different backend, so vectors written in one rebuild
-- could come from different backends, and a display name cannot prove which
-- endpoint, protocol, request path and model revision actually produced a
-- vector. Scoring a query embedded by the primary backend against vectors a
-- fallback backend produced is cross-space nonsense that nothing in the old
-- schema could detect.
--
-- From now on every embedding batch records the identity of the space that
-- actually served it — `text-space-v1:` plus SHA-256 of provider name,
-- protocol, normalized endpoint including path, request path and model
-- revision; authentication is excluded — in the `model` column, and rows are
-- keyed by (shot_id, model). The primary and a fallback space can therefore
-- coexist for one shot, and every read names the space it scores against.
--
-- Legacy rows have unprovable provenance: their `model` is a display name,
-- not a space identity, and no amount of guessing can reconstruct which
-- backend produced them. They are derived, rebuildable data — the canonical
-- shots are untouched — so the upgrade discards them and the operator
-- rebuilds once with `nexusgate search rebuild-embeddings`. Normal analysis
-- re-populates through the usual post-commit hook; starting the Hub never
-- triggers the rebuild by itself.
DROP TABLE IF EXISTS shot_text_embeddings;

CREATE TABLE shot_text_embeddings (
    shot_id TEXT REFERENCES asset_shots(id) ON DELETE CASCADE,
    model TEXT NOT NULL,
    vector_blob BLOB NOT NULL,
    source_text_hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(shot_id, model)
);
CREATE INDEX idx_shot_text_embeddings_model ON shot_text_embeddings(model);
