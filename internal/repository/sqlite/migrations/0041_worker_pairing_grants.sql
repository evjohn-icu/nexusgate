-- v0.22 administrator-granted worker operations.
--
-- Pairing tokens gain the administrator's grant set. The default '{}' keeps
-- every outstanding pre-upgrade token derive-only: an old token that was
-- minted before this column existed never carried administrator-granted
-- operations, and redemption must not invent any.
ALTER TABLE worker_pairing_tokens ADD COLUMN grants_json TEXT NOT NULL DEFAULT '{}';

-- Worker capability rows minted before the grants API carry whatever the
-- enrollment body supplied at the time. No administrator granted those
-- operations, so they must not survive as live authorization: strip the
-- inference inventory and both grant lists while preserving every other key
-- (hardware, library roots, speed class, provider operations). Such a node
-- stays derive-only until an administrator re-enrolls it through the grants
-- API. json_remove ignores paths that are absent, so plain capability rows
-- pass through unchanged.
UPDATE workers SET capabilities_json = json_remove(
  capabilities_json,
  '$.inference',
  '$.inference_operations',
  '$.model_management_operations'
);
