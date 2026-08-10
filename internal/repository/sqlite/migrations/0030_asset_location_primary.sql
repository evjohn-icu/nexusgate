WITH ranked AS (
    SELECT al.id,
           ROW_NUMBER() OVER (
               PARTITION BY al.asset_id
               ORDER BY CASE
                   WHEN al.exists_now=1 AND lr.health_state='healthy' THEN 0
                   WHEN al.exists_now=1 AND lr.health_state='unknown' THEN 1
                   WHEN al.exists_now=0 AND lr.health_state='healthy' THEN 2
                   WHEN al.exists_now=1 AND lr.health_state='unavailable' THEN 3
                   WHEN al.exists_now=0 AND lr.health_state='unknown' THEN 4
                   WHEN al.exists_now=0 AND lr.health_state='unavailable' THEN 5
                   ELSE 6
               END, al.last_seen_at DESC, lr.created_at, lr.id, al.relative_path, al.id
           ) AS rn
    FROM asset_locations al
    JOIN library_roots lr ON lr.id=al.root_id
)
UPDATE asset_locations
SET is_primary=CASE WHEN id IN (SELECT id FROM ranked WHERE rn=1) THEN 1 ELSE 0 END;

CREATE UNIQUE INDEX uq_asset_locations_one_primary ON asset_locations(asset_id) WHERE is_primary=1;
CREATE INDEX idx_asset_locations_asset_exists_seen ON asset_locations(asset_id,exists_now,last_seen_at DESC);
CREATE INDEX idx_asset_locations_root_exists_asset_seen ON asset_locations(root_id,exists_now,asset_id,last_seen_at DESC);
