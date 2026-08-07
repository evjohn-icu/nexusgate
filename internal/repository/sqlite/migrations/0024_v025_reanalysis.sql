-- 0024: Reanalysis requests audit trail
-- timingdex reanalyze forces a succeeded analyze job to run again (new
-- input hash, new model_run) after prompt/schema/validator changes. Each
-- invocation records who asked and why here; the model runs themselves
-- stay immutable and auditable, and the input hash ties the request to the
-- job and run it produced.
CREATE TABLE IF NOT EXISTS reanalysis_requests (
    id         TEXT PRIMARY KEY,
    asset_id   TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    reason     TEXT NOT NULL,
    input_hash TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reanalysis_requests_asset ON reanalysis_requests(asset_id, created_at);
