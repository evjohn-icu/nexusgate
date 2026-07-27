CREATE TABLE repurpose_plan_revisions (
    id TEXT PRIMARY KEY,
    plan_id TEXT NOT NULL REFERENCES repurpose_plans(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL,
    state TEXT NOT NULL,
    snapshot_json TEXT NOT NULL,
    editor_note TEXT NOT NULL,
    created_at TEXT NOT NULL,
    approved_at TEXT,
    UNIQUE(plan_id, revision)
);
CREATE INDEX idx_repurpose_plan_revisions_plan ON repurpose_plan_revisions(plan_id, revision DESC);
