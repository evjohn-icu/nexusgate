package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
)

// SaveTranscript records a succeeded ASR transcript and materializes its timed
// segments as the shot-level speech source in one transaction. The upsert is
// lease-bound exactly like SaveAlignment and SaveMediaMetadata: the lease
// predicate lives inside the INSERT ... SELECT, so an expired holder's whole
// write — transcript row and asr_segments rows alike — lands nowhere, and a
// caller whose lease expired mid-transcription rolls the whole write back
// instead of overwriting the transcript the current holder just committed.
// Both empty means no lease to check (CLI and fixtures).
//
// The segment materialization replaces any prior transcript's intervals even
// when the new result has no timed segments: an empty re-transcription must
// remove the earlier intervals, not leave stale speech evidence behind. The
// same-input replacement bumps created_at so the latest-succeeded-transcript
// selection every reader uses (newest created_at, then descending id) agrees
// with the intervals that were materialized last.
func (r *Repository) SaveTranscript(ctx context.Context, assetID, provider, model, inputHash string, t domain.Transcript, jobID, owner string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	segments, _ := json.Marshal(t.Segments)
	now := formatTime(time.Now().UTC())
	res, err := tx.ExecContext(ctx, `INSERT INTO transcripts(id,asset_id,provider,model,input_hash,language,full_text,segments_json,raw_response,status,created_at)
SELECT ?,?,?,?,?,?,?,?,?, 'succeeded',? WHERE (?='' OR EXISTS (SELECT 1 FROM jobs WHERE id=? AND asset_id=? AND state='running' AND lease_owner=? AND lease_expires_at>?)) ON CONFLICT(asset_id,input_hash) DO UPDATE SET language=excluded.language,full_text=excluded.full_text,segments_json=excluded.segments_json,raw_response=excluded.raw_response,status='succeeded',created_at=excluded.created_at`,
		idgen.New(), assetID, provider, model, inputHash, t.Language, t.Text, string(segments), t.RawResponse, now, jobID, jobID, assetID, owner, now)
	if err != nil {
		return err
	}
	if jobID != "" {
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			// The lease is gone: the transcript write did not land, and the
			// asr_segments materialization below must not run for a transcript
			// the caller does not own.
			return leaseLostErr(jobID, owner)
		}
	}
	// Materialize the timed ASR segments inside the same transaction,
	// replacing any prior transcript's segments (GetTranscript reads the
	// latest row). The common no-segments path — and the placeholder 0-0
	// segment some ASR providers emit — deletes the prior intervals and
	// inserts nothing; degenerate segments carry no placement and are dropped,
	// the same rule Transcript.Timed and the transcript endpoint apply.
	var timed []domain.TranscriptSegment
	for _, seg := range t.Segments {
		if seg.EndMS > seg.StartMS {
			timed = append(timed, seg)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM asr_segments WHERE asset_id=?`, assetID); err != nil {
		return err
	}
	for i, seg := range timed {
		if _, err := tx.ExecContext(ctx, `INSERT INTO asr_segments(id,asset_id,ordinal,start_ms,end_ms,text,confidence) VALUES(?,?,?,?,?,?,NULL)`, idgen.New(), assetID, i, seg.StartMS, seg.EndMS, seg.Text); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetTranscript returns the asset's latest succeeded transcript. "Latest" is
// newest created_at with the row id as the deterministic tie-break — the same
// selection rule every transcript reader in this package applies.
func (r *Repository) GetTranscript(ctx context.Context, assetID string) (*domain.Transcript, error) {
	var t domain.Transcript
	var segments string
	err := r.db.QueryRowContext(ctx, `SELECT language,full_text,segments_json,raw_response FROM transcripts WHERE asset_id=? AND status='succeeded' ORDER BY created_at DESC, id DESC LIMIT 1`, assetID).Scan(&t.Language, &t.Text, &segments, &t.RawResponse)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(segments), &t.Segments)
	return &t, nil
}
