-- A Worker (or the Hub's own local pipeline) that dies mid-job used to leave
-- the job at state='running' forever: nothing anywhere transitioned it back
-- once its lease expired. LeaseNextJob and LeaseNextWorkerDerive now also
-- treat 'running' as leasable once lease_expires_at has passed, so
-- idx_jobs_lease_order -- the partial index LeaseNextJob's query names with
-- INDEXED BY -- has to be widened to match, or SQLite refuses to use it and
-- the query fails outright (see the comment on LeaseNextJob for why that
-- failure is deliberate rather than a silent slow-path fallback).
DROP INDEX idx_jobs_lease_order;
CREATE INDEX idx_jobs_lease_order ON jobs(priority DESC, created_at)
    WHERE state IN ('pending','failed','running') AND terminal=0;
