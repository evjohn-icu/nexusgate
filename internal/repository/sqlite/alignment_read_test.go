package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

func TestGetAlignmentWordsReadsLatestSucceededRun(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-align.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-align','fp',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}

	// No alignment yet: nil, not an error.
	words, err := repo.GetAlignmentWords(ctx, "asset-align")
	if err != nil || words != nil {
		t.Fatalf("no alignment: words=%v err=%v", words, err)
	}

	first := domain.AlignmentResult{Words: []domain.AlignmentWord{
		{StartMS: 0, EndMS: 500, Text: "one", Confidence: floatPtr(0.9)},
		{StartMS: 600, EndMS: 900, Text: "two"},
	}}
	if err := repo.SaveAlignment(ctx, "asset-align", "aligner", "m", "hash-1", "{}", first, "", ""); err != nil {
		t.Fatal(err)
	}
	words, err = repo.GetAlignmentWords(ctx, "asset-align")
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 2 || words[0].StartMS != 0 || words[1].EndMS != 900 || words[1].Text != "two" {
		t.Fatalf("words=%+v", words)
	}
	if words[0].Confidence == nil || *words[0].Confidence != 0.9 || words[1].Confidence != nil {
		t.Fatalf("confidence not round-tripped: %+v", words)
	}

	// A newer run supersedes: only the latest succeeded run's words are read.
	second := domain.AlignmentResult{Words: []domain.AlignmentWord{
		{StartMS: 310_000, EndMS: 315_000, Text: "hello"},
	}}
	if err := repo.SaveAlignment(ctx, "asset-align", "aligner", "m", "hash-2", "{}", second, "", ""); err != nil {
		t.Fatal(err)
	}
	words, err = repo.GetAlignmentWords(ctx, "asset-align")
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 1 || words[0].Text != "hello" || words[0].StartMS != 310_000 {
		t.Fatalf("stale words returned: %+v", words)
	}
}

func floatPtr(v float64) *float64 { return &v }

// TestGetShotFallsBackToASRSegments pins the resolution order of a shot's
// speech. align is optional, so an ASR-only asset is the common case, and
// GetShot used to report only aligned words — an agent reading an empty
// transcript there would conclude the shot is silent and skip usable footage.
func TestGetShotFallsBackToASRSegments(t *testing.T) {
	ctx := context.Background()
	repo, assetID := setupModelRunsTest(t)
	runID, _, err := repo.CreateModelRun(ctx, assetID, "vision", "fixture", "model", "shot-asr", "prompt", "asset-analysis/v2", "{}", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, runID, "raw", "{}", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitAnalysisWithShots(ctx, assetID, runID, "asset-analysis/v2", domain.StructuredAnalysis{Summary: "s"},
		[]domain.AssetShot{{AssetID: assetID, SourceRunID: runID, Ordinal: 0, StartMS: 0, EndMS: 5000, Description: "街景"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	shots, err := repo.ListAssetShots(ctx, assetID)
	if err != nil || len(shots) != 1 {
		t.Fatalf("shots=%v err=%v", shots, err)
	}
	shotID := shots[0].ID

	// No transcript at all: no source claimed either way.
	detail, err := repo.GetShot(ctx, shotID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.TranscriptSource != "" || len(detail.Transcript) != 0 || len(detail.TranscriptSegments) != 0 {
		t.Fatalf("no transcript should claim no source, got source=%q", detail.TranscriptSource)
	}

	// ASR only: sentence segments, labelled "asr" so segment timing is never
	// mistaken for word timing.
	if err := repo.SaveTranscript(ctx, assetID, "fixture", "model", "asr-hash", domain.Transcript{
		Language: "zh", Text: "明天见",
		Segments: []domain.TranscriptSegment{{StartMS: 1000, EndMS: 2000, Text: "明天见"}},
	}, "", ""); err != nil {
		t.Fatal(err)
	}
	detail, err = repo.GetShot(ctx, shotID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.TranscriptSource != "asr" {
		t.Fatalf("transcript_source=%q, want asr", detail.TranscriptSource)
	}
	if len(detail.TranscriptSegments) != 1 || detail.TranscriptSegments[0].Text != "明天见" {
		t.Fatalf("segments=%v, want the ASR sentence", detail.TranscriptSegments)
	}
	if len(detail.Transcript) != 0 {
		t.Fatalf("word-level transcript must stay empty without an alignment, got %v", detail.Transcript)
	}

	// Aligned words win once they exist, and the ASR segments they replace
	// are dropped by SaveAlignment, so exactly one source is ever reported.
	confidence := 0.9
	if err := repo.SaveAlignment(ctx, assetID, "fixture", "model", "align-hash", "{}", domain.AlignmentResult{Words: []domain.AlignmentWord{
		{StartMS: 1000, EndMS: 1500, Text: "明天", Confidence: &confidence},
		{StartMS: 1500, EndMS: 2000, Text: "见", Confidence: &confidence},
	}}, "", ""); err != nil {
		t.Fatal(err)
	}
	detail, err = repo.GetShot(ctx, shotID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.TranscriptSource != "aligned" {
		t.Fatalf("transcript_source=%q, want aligned", detail.TranscriptSource)
	}
	if len(detail.Transcript) != 2 {
		t.Fatalf("words=%v, want the two aligned words", detail.Transcript)
	}
	if len(detail.TranscriptSegments) != 0 {
		t.Fatalf("aligned shots must not also report segments, got %v", detail.TranscriptSegments)
	}
}
