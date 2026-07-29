-- Data-layer performance fixes (single-user library, thousands to low tens of
-- thousands of assets, so these stay plain indexes/mapping tables rather than
-- anything sharded or cached).

-- asset_search and asset_shot_search declare asset_id UNINDEXED (see
-- 0002_pipeline.sql and 0006_v080_shots.sql), so FTS5 builds no secondary
-- index on it: `DELETE FROM asset_search WHERE asset_id=?` and
-- `DELETE FROM asset_shot_search WHERE asset_id=?` each scan the whole
-- shadow table, and those deletes run for every asset finishing analyze.
-- These mapping tables let a delete resolve to the FTS5 rowid first, so the
-- actual delete is `WHERE rowid=?` / `WHERE rowid IN (...)`, which FTS5 can
-- serve without a full scan. Kept in sync on every insert/delete path in
-- repository.go.
CREATE TABLE asset_search_rowids (
    asset_id TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
    search_rowid INTEGER NOT NULL
);

CREATE TABLE asset_shot_search_rowids (
    shot_id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL,
    search_rowid INTEGER NOT NULL
);
CREATE INDEX idx_asset_shot_search_rowids_asset ON asset_shot_search_rowids(asset_id);

-- Backfill from whatever is already in the shadow tables. If a pending CJK
-- bigram FTS rebuild (fts_index_state='cjk_bigram_v1') runs after this
-- migration, it repopulates both mapping tables itself from the fresh
-- rebuild, so a stale backfill here is harmless.
INSERT INTO asset_search_rowids(asset_id, search_rowid)
SELECT asset_id, rowid FROM asset_search;

INSERT INTO asset_shot_search_rowids(shot_id, asset_id, search_rowid)
SELECT shot_id, asset_id, rowid FROM asset_shot_search;

-- Search() filters asset_tag_links on normalized_tag directly (see
-- repository.go Search()). None of the existing indexes
-- (PRIMARY KEY(asset_id, normalized_tag, tag_type, source),
-- idx_asset_tag_links_canonical, idx_asset_tag_links_unresolved) lead with
-- that column, so the lookup scans the table.
CREATE INDEX idx_asset_tag_links_normalized ON asset_tag_links(normalized_tag);

-- Redundant: idx_derived_artifacts_asset_type(asset_id, artifact_type) only
-- duplicates the leading columns of the automatic index that already backs
-- UNIQUE(asset_id, artifact_type, profile_hash) from 0001_initial.sql, so it
-- costs write maintenance for no read benefit.
DROP INDEX idx_derived_artifacts_asset_type;
