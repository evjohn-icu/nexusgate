CREATE TABLE repurpose_plans (
    id TEXT PRIMARY KEY,
    brief TEXT NOT NULL,
    duration_ms INTEGER NOT NULL,
    style TEXT NOT NULL,
    audience TEXT NOT NULL,
    title TEXT NOT NULL,
    status TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    plan_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_repurpose_plans_created ON repurpose_plans(created_at DESC);
