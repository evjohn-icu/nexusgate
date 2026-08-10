-- v0.28: per-root health state so a disconnected NAS root is distinguishable
-- from a deleted library and missing-file reconciliation can be gated on the
-- root actually being reachable.
--
-- Existing rows default to 'unknown' on purpose: unverified roots must never
-- be treated as healthy, or the first restart after this migration would
-- resume marking assets missing against a root nobody has walked since.
ALTER TABLE library_roots ADD COLUMN health_state TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE library_roots ADD COLUMN last_healthy_at TEXT;
ALTER TABLE library_roots ADD COLUMN last_scan_at TEXT;
