-- 0042_local_model_target: the Hub's own maintenance target becomes the
-- stable literal 'local'. Queueing used to write the queuing process's lease
-- owner (local-...), so Hub-local model work went un-leasable after a restart:
-- the target matched no live owner any more. Only rows whose value is not a
-- persisted Worker ID are rewritten; a genuine Worker named like the legacy
-- sentinel is impossible today (Worker ids are generated UUID-shaped), but the
-- guard keeps the rewrite honest if that ever changes.
UPDATE model_management_tasks
SET target_worker_id = 'local'
WHERE target_worker_id LIKE 'local-%'
  AND target_worker_id NOT IN (SELECT id FROM workers);

ALTER TABLE model_management_tasks ADD COLUMN completed_by TEXT NOT NULL DEFAULT '';
