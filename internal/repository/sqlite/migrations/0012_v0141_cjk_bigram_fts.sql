CREATE TABLE IF NOT EXISTS fts_index_state (
    name TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

INSERT INTO fts_index_state(name, value, updated_at)
VALUES('cjk_bigram_v1', 'pending', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
ON CONFLICT(name) DO UPDATE SET value='pending', updated_at=excluded.updated_at;

DELETE FROM asset_search;
DELETE FROM asset_shot_search;
