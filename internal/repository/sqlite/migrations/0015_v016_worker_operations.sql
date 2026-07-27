-- v0.16 worker execution visibility and manual routing.
ALTER TABLE jobs ADD COLUMN current_stage TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN progress REAL NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN last_failure_at TEXT;
ALTER TABLE jobs ADD COLUMN last_failure_worker_id TEXT REFERENCES workers(id) ON DELETE SET NULL;
ALTER TABLE jobs ADD COLUMN last_failure_stage TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN preferred_worker_id TEXT REFERENCES workers(id) ON DELETE SET NULL;
ALTER TABLE jobs ADD COLUMN assigned_worker_id TEXT REFERENCES workers(id) ON DELETE SET NULL;

ALTER TABLE job_events ADD COLUMN stage TEXT NOT NULL DEFAULT '';
ALTER TABLE job_events ADD COLUMN progress REAL NOT NULL DEFAULT 0;
ALTER TABLE job_events ADD COLUMN error_code TEXT NOT NULL DEFAULT '';
ALTER TABLE job_events ADD COLUMN retryable INTEGER NOT NULL DEFAULT 0;

CREATE INDEX idx_jobs_assigned_worker ON jobs(assigned_worker_id, job_type, state, run_after);
CREATE INDEX idx_jobs_preferred_worker ON jobs(preferred_worker_id, job_type, state, run_after);
CREATE INDEX idx_job_events_job_created ON job_events(job_id, created_at);
