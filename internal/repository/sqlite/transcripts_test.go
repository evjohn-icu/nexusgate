package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

func openTranscriptTestRepo(t *testing.T, name string) *Repository {
	t.Helper()
	repo, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return repo
}

// seedTranscriptAsset inserts the bare asset row the transcripts/asr_segments
// foreign keys require.
func seedTranscriptAsset(t *testing.T, repo *Repository, ctx context.Context, assetID string) {
	t.Helper()
	stamp := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,? ,1,'discovered',?,?)`, assetID, assetID, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

// TestSaveTranscriptEmptyResultRemovesEarlierTimedEvidence pins the
// materialization rule the transcript channel depends on: a later succeeded
// ASR result with no timed segments must delete the earlier transcript's
// intervals in the same transaction that records it, or shots keep reporting
// speech the latest transcript no longer claims.
func TestSaveTranscriptEmptyResultRemovesEarlierTimedEvidence(t *testing.T) {
	ctx := context.Background()
	repo := openTranscriptTestRepo(t, "asr-empty-replace.db")
	seedTranscriptAsset(t, repo, ctx, "asset-asr")

	timed := domain.Transcript{Language: "zh", Text: "both lines", Segments: []domain.TranscriptSegment{
		{StartMS: 0, EndMS: 1200, Text: "句子一"},
		{StartMS: 1500, EndMS: 2600, Text: "句子二"},
		// The 0-0 placeholder some providers emit carries no placement.
		{StartMS: 0, EndMS: 0, Text: "placeholder"},
	}}
	if err := repo.SaveTranscript(ctx, "asset-asr", "stepfun", "stepfun-asr", "hash-1", timed, "", ""); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asr_segments WHERE asset_id='asset-asr'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("segments=%d, want 2 timed intervals", count)
	}

	// A different-input empty result replaces the intervals.
	if err := repo.SaveTranscript(ctx, "asset-asr", "stepfun", "stepfun-asr", "hash-2", domain.Transcript{Language: "zh", Text: "no placement"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asr_segments WHERE asset_id='asset-asr'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("segments after empty result=%d, want 0", count)
	}

	// A same-input empty result (re-delivery of hash-2) stays empty.
	if err := repo.SaveTranscript(ctx, "asset-asr", "stepfun", "stepfun-asr", "hash-2", domain.Transcript{Language: "zh", Text: "no placement"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asr_segments WHERE asset_id='asset-asr'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("segments after same-input empty result=%d, want 0", count)
	}
}

// TestSaveTranscriptReplacementBumpsCreatedAt pins why same-input replacement
// updates created_at: the materialized intervals are written by the newest
// save, so the latest-transcript selection (newest created_at, then id) must
// point at exactly the row whose segments were materialized last — otherwise
// a replaced transcript's text and its intervals can disagree.
func TestSaveTranscriptReplacementBumpsCreatedAt(t *testing.T) {
	ctx := context.Background()
	repo := openTranscriptTestRepo(t, "asr-created-at.db")
	seedTranscriptAsset(t, repo, ctx, "asset-bump")

	first := domain.Transcript{Language: "zh", Text: "first take", Segments: []domain.TranscriptSegment{{StartMS: 100, EndMS: 900, Text: "一"}}}
	if err := repo.SaveTranscript(ctx, "asset-bump", "qwen", "qwen3-asr-flash", "hash-same", first, "", ""); err != nil {
		t.Fatal(err)
	}
	var firstCreated string
	if err := repo.db.QueryRowContext(ctx, `SELECT created_at FROM transcripts WHERE asset_id='asset-bump' AND input_hash='hash-same'`).Scan(&firstCreated); err != nil {
		t.Fatal(err)
	}

	second := domain.Transcript{Language: "zh", Text: "second take", Segments: []domain.TranscriptSegment{{StartMS: 100, EndMS: 950, Text: "一点"}}}
	if err := repo.SaveTranscript(ctx, "asset-bump", "qwen", "qwen3-asr-flash", "hash-same", second, "", ""); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transcripts WHERE asset_id='asset-bump'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("transcript rows=%d, want the same-input replacement to stay one row", rows)
	}
	var secondCreated string
	if err := repo.db.QueryRowContext(ctx, `SELECT created_at FROM transcripts WHERE asset_id='asset-bump' AND input_hash='hash-same'`).Scan(&secondCreated); err != nil {
		t.Fatal(err)
	}
	if secondCreated <= firstCreated {
		t.Fatalf("replacement did not advance created_at: first=%s second=%s", firstCreated, secondCreated)
	}

	got, err := repo.GetTranscript(ctx, "asset-bump")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Text != "second take" {
		t.Fatalf("GetTranscript=%+v, want the replacement's text", got)
	}
	var text string
	if err := repo.db.QueryRowContext(ctx, `SELECT text FROM asr_segments WHERE asset_id='asset-bump' AND ordinal=0`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "一点" {
		t.Fatalf("materialized interval text=%q, want the replacement's segment", text)
	}
}

// TestSaveTranscriptLostLeaseLeavesNoRows pins the lease boundary on the
// combined write: an expired holder's transcript must not land and must not
// touch the current holder's materialized intervals.
func TestSaveTranscriptLostLeaseLeavesNoRows(t *testing.T) {
	ctx := context.Background()
	repo := openTranscriptTestRepo(t, "asr-lease.db")
	seedTranscriptAsset(t, repo, ctx, "asset-lease")

	if err := repo.SaveTranscript(ctx, "asset-lease", "qwen", "qwen3-asr-flash", "hash-1", domain.Transcript{Language: "zh", Text: "kept", Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 500, Text: "kept"}}}, "", ""); err != nil {
		t.Fatal(err)
	}

	err := repo.SaveTranscript(ctx, "asset-lease", "qwen", "qwen3-asr-flash", "hash-2", domain.Transcript{Language: "zh", Text: "stale", Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 500, Text: "stale"}}}, "job-gone", "worker-gone")
	if !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatalf("SaveTranscript error=%v, want lease lost", err)
	}
	var rows int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transcripts WHERE asset_id='asset-lease' AND input_hash='hash-2'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("stale transcript rows=%d, want 0", rows)
	}
	var text string
	if err := repo.db.QueryRowContext(ctx, `SELECT text FROM asr_segments WHERE asset_id='asset-lease'`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "kept" {
		t.Fatalf("materialized interval text=%q, want the earlier transcript's segments to survive", text)
	}
}

// TestGetTranscriptSelectionTieBreak pins the reader selection rule: newest
// created_at, and when two succeeded rows share a created_at (rows written
// before the sortable-timestamp rewrite, or a same-timestamp replacement),
// the descending row id decides — the same ordering the browse join applies.
func TestGetTranscriptSelectionTieBreak(t *testing.T) {
	ctx := context.Background()
	repo := openTranscriptTestRepo(t, "asr-selection.db")
	seedTranscriptAsset(t, repo, ctx, "asset-sel")

	if err := repo.SaveTranscript(ctx, "asset-sel", "qwen", "qwen3-asr-flash", "hash-old", domain.Transcript{Language: "zh", Text: "older"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTranscript(ctx, "asset-sel", "qwen", "qwen3-asr-flash", "hash-new", domain.Transcript{Language: "zh", Text: "newer"}, "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetTranscript(ctx, "asset-sel")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Text != "newer" {
		t.Fatalf("GetTranscript=%+v, want the newest transcript", got)
	}

	// Force a created_at tie and give the older save the lexicographically
	// greater id: only the id tie-break may decide, and it must pick it.
	stamp := formatTime(time.Now())
	if _, err := repo.db.ExecContext(ctx, `UPDATE transcripts SET created_at=? WHERE asset_id='asset-sel'`, stamp); err != nil {
		t.Fatal(err)
	}
	var oldID string
	if err := repo.db.QueryRowContext(ctx, `SELECT id FROM transcripts WHERE input_hash='hash-old'`).Scan(&oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE transcripts SET id='zzzz-older' WHERE input_hash='hash-old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE transcripts SET id=? WHERE input_hash='hash-new'`, oldID); err != nil {
		t.Fatal(err)
	}
	got, err = repo.GetTranscript(ctx, "asset-sel")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Text != "older" {
		t.Fatalf("GetTranscript=%+v, want the tied-timestamp row with the greater id", got)
	}
}
