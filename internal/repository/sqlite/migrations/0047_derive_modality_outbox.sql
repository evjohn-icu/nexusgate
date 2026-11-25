-- 0047_derive_modality_outbox: durable replay queue for post-derive
-- modality enqueueing.
--
-- A remote derive's completion transaction records one marker here. The
-- serving Hub drains it later by planning the asset's modalities from the
-- derive job's canonical identity and deletes the marker only after every
-- planned task has been queued (or recognized as an already-queued
-- duplicate). Without the marker, a Hub restart between a Worker's derive
-- completion and the modality enqueue silently dropped every audio task the
-- operator had enabled: the completion transaction had already committed,
-- so the jobs were never re-enqueued by anything.
--
-- ON DELETE CASCADE keeps the queue free of orphans: the marker is
-- meaningful only while its derive job row exists to identify the asset and
-- the derive identity that produced the artifacts.
CREATE TABLE derive_modality_outbox (
    derive_job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE
);
