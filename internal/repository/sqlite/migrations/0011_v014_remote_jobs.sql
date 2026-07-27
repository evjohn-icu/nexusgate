CREATE INDEX idx_jobs_worker_derive ON jobs(job_type, state, run_after, priority);
