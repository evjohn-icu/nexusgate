-- pipeline_executors is the liveness registry for processes that run the
-- Hub-local pipeline (`serve`, and `timingdex pipeline run`). Each process
-- registers one row on startup (executor_id == its lease owner) and refreshes
-- last_seen_at on a short heartbeat for as long as it is alive. HealStaleRunningJobs
-- consults the registry to tell a live process's in-flight jobs (fresh row) from
-- the orphans a killed process left behind (missing or stale row), so a Hub
-- restart reclaims the previous process's jobs immediately instead of waiting
-- out the whole lease TTL (30 minutes for derive).
CREATE TABLE pipeline_executors (
  executor_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  last_seen_at TEXT NOT NULL
) WITHOUT ROWID;
