package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMigration0044RebuildsLatestTranscriptIntervals upgrades a database whose
// asr_segments rows were materialized by an older Hub from a transcript that
// has since been superseded. The migration must rebuild every asset's
// intervals from only the selected succeeded transcript (newest created_at,
// then id), drop intervals when the selected transcript has no timed
// segments, and preserve the forced-alignment words used preferentially by readers.
func TestMigration0044RebuildsLatestTranscriptIntervals(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "asr-rebuild.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	applyMigrationsThrough(t, ctx, repo, "0040_multimodal_generation_identity.sql")

	now := "2026-01-01T12:00:00.000000000Z"
	for _, asset := range []string{"rebuild-a", "rebuild-b", "rebuild-c"} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'analyzed',?,?)`, asset, asset, now, now); err != nil {
			t.Fatal(err)
		}
	}
	insertTranscript := func(t *testing.T, id, asset, hash string, createdShiftMinutes int, segmentsJSON string) {
		t.Helper()
		stamp := "2026-01-01T10:00:00.000000000Z"
		if createdShiftMinutes > 0 {
			stamp = "2026-01-01T11:00:00.000000000Z"
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO transcripts(id,asset_id,provider,model,input_hash,language,full_text,segments_json,raw_response,status,created_at) VALUES(?,?,?,?,?,NULL,'',?,'','succeeded',?)`, id, asset, "qwen", "qwen3-asr-flash", hash, segmentsJSON, stamp); err != nil {
			t.Fatal(err)
		}
	}
	insertSegment := func(t *testing.T, id, asset string, ordinal int64, startMS, endMS int64, text string) {
		t.Helper()
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asr_segments(id,asset_id,ordinal,start_ms,end_ms,text,confidence) VALUES(?,?,?,?,?,?,NULL)`, id, asset, ordinal, startMS, endMS, text); err != nil {
			t.Fatal(err)
		}
	}

	// rebuild-a: two transcript revisions. The stale materialization came from
	// the older one; only the newer revision's intervals may survive.
	insertTranscript(t, "transcript-a-old", "rebuild-a", "hash-a-old", 0, `[{"start_ms":0,"end_ms":1000,"text":"旧一句"},{"start_ms":1000,"end_ms":2000,"text":"旧二句"}]`)
	insertTranscript(t, "transcript-a-new", "rebuild-a", "hash-a-new", 60, `[{"start_ms":0,"end_ms":1500,"text":"新一句"},{"start_ms":1500,"end_ms":3000,"text":"新二句"}]`)
	insertSegment(t, "asr-stale-a0", "rebuild-a", 0, 0, 1000, "旧一句")
	insertSegment(t, "asr-stale-a1", "rebuild-a", 1, 1000, 2000, "旧二句")

	// rebuild-b: a single transcript whose only segment is the 0-0 placeholder
	// some providers emit, plus a stale interval an older write left behind.
	// The rebuild must leave no intervals: a degenerate segment carries no
	// placement.
	insertTranscript(t, "transcript-b", "rebuild-b", "hash-b", 0, `[{"start_ms":0,"end_ms":0,"text":"placeholder"}]`)
	insertSegment(t, "asr-stale-b0", "rebuild-b", 0, 0, 1000, "陈旧")

	// rebuild-c keeps its alignment words and receives current fallback intervals.
	insertTranscript(t, "transcript-c", "rebuild-c", "hash-c", 0, `[{"start_ms":0,"end_ms":1000,"text":"对齐资产"}]`)
	insertSegment(t, "asr-stale-c0", "rebuild-c", 0, 0, 1000, "陈旧对齐")
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO alignment_runs(id,asset_id,provider,model,input_hash,state,request_json,raw_response,created_at,finished_at) VALUES('run-c','rebuild-c','x','x','hash-c','succeeded','{}','{}',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO transcript_words(alignment_run_id,asset_id,ordinal,start_ms,end_ms,text,confidence) VALUES('run-c','rebuild-c',0,0,1000,'对齐资产',0.9)`); err != nil {
		t.Fatal(err)
	}

	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var aCount int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asr_segments WHERE asset_id='rebuild-a'`).Scan(&aCount); err != nil {
		t.Fatal(err)
	}
	if aCount != 2 {
		t.Fatalf("rebuild-a intervals=%d, want the 2 from the newest transcript only", aCount)
	}
	var text string
	if err := repo.db.QueryRowContext(ctx, `SELECT text FROM asr_segments WHERE asset_id='rebuild-a' AND ordinal=0`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "新一句" {
		t.Fatalf("rebuild-a ordinal 0 text=%q, want the newer transcript's segment", text)
	}
	var bCount int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asr_segments WHERE asset_id='rebuild-b'`).Scan(&bCount); err != nil {
		t.Fatal(err)
	}
	if bCount != 0 {
		t.Fatalf("rebuild-b intervals=%d, want the stale interval removed by the untimed transcript", bCount)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT text FROM asr_segments WHERE asset_id='rebuild-c'`).Scan(&text); err != nil || text != "对齐资产" {
		t.Fatalf("rebuild-c fallback text=%q err=%v, want latest transcript segment", text, err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT text FROM transcript_words WHERE asset_id='rebuild-c'`).Scan(&text); err != nil || text != "对齐资产" {
		t.Fatalf("rebuild-c alignment text=%q err=%v, want preserved word", text, err)
	}
}
