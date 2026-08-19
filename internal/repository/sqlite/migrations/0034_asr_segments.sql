-- ASR transcript timed segments, materialized so the shot-level speech
-- channel can search speech even when the optional forced-alignment stage has
-- not run. An asset's effective speech source is either its alignment words
-- (strongest timing) or these ASR segments, never both; the transcript channel
-- resolves per-asset (aligned assets exclude asr_segments).
--
-- Only timed segments (end_ms > start_ms) are stored — the 0-0 placeholder
-- segment some ASR providers emit carries no placement and is dropped, the
-- same rule Transcript.Timed and the transcript endpoint apply.
CREATE TABLE asr_segments (
    id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    start_ms INTEGER NOT NULL,
    end_ms INTEGER NOT NULL,
    text TEXT NOT NULL,
    confidence REAL
);
CREATE INDEX idx_asr_segments_asset_time ON asr_segments(asset_id, start_ms);

-- Backfill from existing succeeded transcripts so libraries transcribed before
-- this migration become shot-level speech-searchable without re-transcribing.
-- Only assets without alignment words are backfilled: an aligned asset's
-- speech source is its alignment words, and the transcript channel resolves
-- per asset with asr_segments excluded there. Guarded: json_type = 'array'
-- skips the placeholder "null" some rows carry, and the end>start + text
-- IS NOT NULL predicates keep the NOT NULL columns safe against any malformed
-- segment.
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
  AND NOT EXISTS (SELECT 1 FROM transcript_words t WHERE t.asset_id = tr.asset_id);
