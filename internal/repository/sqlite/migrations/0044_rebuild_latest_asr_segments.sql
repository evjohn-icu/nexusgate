-- Rebuild every asset's materialized ASR speech intervals from exactly one
-- source: the asset's latest succeeded transcript (newest created_at, then
-- descending id — the same selection rule GetTranscript and the browse joins
-- use). Before this migration the materialized asr_segments rows could come
-- from an older transcript revision while transcript reads returned a newer
-- one, so shot-level speech evidence disagreed with transcript reads.
--
-- Delete first, then re-materialize: a newer transcript with no timed
-- segments must leave no stale intervals behind.
--
-- The valid timed-segment rules are the ones 0034's backfill and the
-- SaveTranscript materialization apply: json_type = 'array' skips the
-- placeholder "null" some rows carry, end_ms > start_ms drops degenerate
-- segments, and text IS NOT NULL keeps the NOT NULL column safe against any
-- malformed segment. Forced alignment remains the reader-preferred source.
-- Rebuild ASR even for aligned assets so either source remains current.
DELETE FROM asr_segments;

INSERT INTO asr_segments(id, asset_id, ordinal, start_ms, end_ms, text, confidence)
SELECT 'asr-' || tr.id || '-' || seg.key,
       tr.asset_id,
       CAST(seg.key AS INTEGER),
       CAST(json_extract(seg.value, '$.start_ms') AS INTEGER),
       CAST(json_extract(seg.value, '$.end_ms') AS INTEGER),
       json_extract(seg.value, '$.text'),
       NULL
FROM transcripts tr, json_each(tr.segments_json) AS seg
WHERE tr.status = 'succeeded'
  AND json_type(tr.segments_json) = 'array'
  AND json_extract(seg.value, '$.text') IS NOT NULL
  AND CAST(json_extract(seg.value, '$.end_ms') AS INTEGER) > CAST(json_extract(seg.value, '$.start_ms') AS INTEGER)
  AND tr.id = (
    SELECT tr2.id
    FROM transcripts tr2
    WHERE tr2.asset_id = tr.asset_id AND tr2.status = 'succeeded'
    ORDER BY tr2.created_at DESC, tr2.id DESC
    LIMIT 1
  );
