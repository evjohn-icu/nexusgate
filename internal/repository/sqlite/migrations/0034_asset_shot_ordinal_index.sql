-- Neighbor and ordinal-range lookups are ordered by ordinal, not media time.
-- Keep the existing time index for overlap retrieval and add the access path
-- used by Search context batching.
CREATE INDEX idx_asset_shots_asset_ordinal
    ON asset_shots(asset_id, ordinal);
