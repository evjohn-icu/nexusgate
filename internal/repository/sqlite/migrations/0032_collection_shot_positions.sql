-- v0.30: make collection shot positions contiguous and unique.
CREATE TABLE collection_shots_rebuilt (
    collection_id TEXT NOT NULL REFERENCES asset_collections(id) ON DELETE CASCADE,
    shot_id TEXT NOT NULL REFERENCES asset_shots(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (collection_id, shot_id),
    UNIQUE (collection_id, position)
);
INSERT INTO collection_shots_rebuilt(collection_id, shot_id, position, created_at)
SELECT collection_id, shot_id,
       ROW_NUMBER() OVER (PARTITION BY collection_id ORDER BY position ASC, shot_id ASC) - 1,
       created_at
FROM collection_shots;
DROP TABLE collection_shots;
ALTER TABLE collection_shots_rebuilt RENAME TO collection_shots;
CREATE INDEX idx_collection_shots_collection ON collection_shots(collection_id, position);
