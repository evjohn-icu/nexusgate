ALTER TABLE assets ADD COLUMN probe_modified_ns TEXT;

UPDATE assets
SET probe_modified_ns = COALESCE(
    (SELECT CAST(l.modified_ns AS TEXT)
     FROM asset_locations l
     WHERE l.asset_id = assets.id AND l.exists_now = 1
     ORDER BY l.last_seen_at ASC, l.id ASC LIMIT 1),
    (SELECT CAST(l.modified_ns AS TEXT)
     FROM asset_locations l
     WHERE l.asset_id = assets.id
     ORDER BY l.last_seen_at ASC, l.id ASC LIMIT 1)
)
WHERE probe_modified_ns IS NULL;
