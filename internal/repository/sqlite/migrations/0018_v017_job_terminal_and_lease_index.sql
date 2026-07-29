-- A failure the pipeline classified as permanent must be excluded from the
-- lease predicate. The first implementation did that by setting attempt_count
-- to max_attempts, which works but lies: /progress showed a job that ran once
-- as "3/3". terminal records the fact directly so the attempt counter can stay
-- an honest count of how many times the job actually ran.
--
-- Rows parked at max_attempts by that earlier implementation are
-- indistinguishable from genuinely exhausted ones, so they are left as they
-- are. Nothing needs backfilling: both remain unleasable.
ALTER TABLE jobs ADD COLUMN terminal INTEGER NOT NULL DEFAULT 0;

-- LeaseNextJob orders by priority DESC, created_at and no existing index can
-- supply that order: idx_jobs_ready leads with state, and the IN () over two
-- states rules out an ordered scan of any state-leading index. Every lease
-- therefore built a temp b-tree over every leasable row -- measured at 170ms
-- per lease on a 40k-row queue.
--
-- The leasable predicate is baked into the index so the ordered scan is itself
-- the filter and LIMIT 1 stops at the first hit. SQLite's planner does not
-- pick this on cost (it underestimates the sort), so LeaseNextJob names it with
-- INDEXED BY; that is also why the WHERE clause here must stay character-equal
-- to the query's terms.
CREATE INDEX idx_jobs_lease_order ON jobs(priority DESC, created_at)
    WHERE state IN ('pending','failed') AND terminal=0;
