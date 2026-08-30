package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// seedAsrOnlyAsset seeds one asset+shot whose speech timing comes only from
// the ASR transcript's timed segments — no forced-alignment run — so the
// speech channel must source from asr_segments.
func seedAsrOnlyAsset(t *testing.T, repo *Repository, id string, shotStart, shotEnd int64) string {
	t.Helper()
	ids := seedOneAsset(t, repo, map[string]string{}, goldenAssetSpec{
		id:       id,
		analysis: domain.StructuredAnalysis{Summary: "asr only " + id},
		shots:    []goldenShotSpec{{startMS: shotStart, endMS: shotEnd, description: "interview"}},
	})
	return ids[id+":0"]
}

// TestSearchV2TranscriptAsrOnlySearchesShots pins the new capability: an
// asset with no forced-alignment run is still searchable at shot level from
// its ASR transcript's timed segments. The spoken phrase pinpoints the shot
// (retrieval + span evidence), and a sub-phrase of a sentence-level segment
// matches because ShotTranscriptSpans expands ASR segments to per-rune spans.
func TestSearchV2TranscriptAsrOnlySearchesShots(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "asr-speech.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	shotID := seedAsrOnlyAsset(t, repo, "asset-asr-only", 90_000, 200_000)
	// One sentence-level ASR segment covering the whole phrase.
	if err := repo.SaveTranscript(ctx, "asset-asr-only", "stepfun", "stepfun-asr", "asr-input-1", domain.Transcript{
		Language: "zh",
		Text:     "我们明天出发去上海",
		Segments: []domain.TranscriptSegment{{StartMS: 95_000, EndMS: 105_000, Text: "我们明天出发去上海"}},
	}, "", ""); err != nil {
		t.Fatal(err)
	}

	// Retrieval: the full phrase hits the shot.
	hits, err := repo.TranscriptRankedShots(ctx, "我们明天出发去上海", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != shotID {
		t.Fatalf("ASR speech must hit the shot, got %+v", hits)
	}
	if hits[0].TranscriptScore <= 0 {
		t.Fatalf("transcript score must be positive, got %v", hits[0].TranscriptScore)
	}

	// Retrieval: a sub-phrase of the sentence-level segment also hits, because
	// ShotTranscriptSpans expands the segment into per-rune spans.
	sub, err := repo.TranscriptRankedShots(ctx, "明天出发", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub) != 1 || sub[0].ID != shotID {
		t.Fatalf("ASR sub-phrase '明天出发' must hit the shot, got %+v", sub)
	}

	// Span evidence: the shot's speech spans come from the expanded ASR
	// segment (per-rune), so the exact-phrase validator can confirm the phrase.
	spans, err := repo.ShotTranscriptSpans(ctx, "asset-asr-only", 90_000, 200_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) < 3 || spans[0].Text != "我" || spans[1].Text != "们" {
		t.Fatalf("spans = %+v, want the ASR segment expanded to per-rune spans", spans)
	}
}

// TestSearchV2TranscriptAlignedAssetIgnoresAsrSegments pins the per-asset
// source rule: when an asset has alignment words, the ASR segments are
// excluded from its speech channel (the aligned words are the authoritative,
// finer-grained timing).
func TestSearchV2TranscriptAlignedAssetIgnoresAsrSegments(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "asr-aligned.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	shotID := seedAsrOnlyAsset(t, repo, "asset-aligned", 90_000, 200_000)
	if err := repo.SaveTranscript(ctx, "asset-aligned", "stepfun", "stepfun-asr", "asr-input-1", domain.Transcript{
		Language: "zh",
		Text:     "完全无关的话",
		Segments: []domain.TranscriptSegment{{StartMS: 90_000, EndMS: 200_000, Text: "完全无关的话"}},
	}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveAlignment(ctx, "asset-aligned", "fixture", "fixture-model", "align-1", "{}", domain.AlignmentResult{Words: []domain.AlignmentWord{
		{StartMS: 100_000, EndMS: 101_000, Text: "我们"},
		{StartMS: 101_000, EndMS: 102_000, Text: "明天"},
		{StartMS: 102_000, EndMS: 103_000, Text: "出发"},
	}}, "", ""); err != nil {
		t.Fatal(err)
	}

	// The alignment phrase hits via the aligned words.
	aligned, err := repo.TranscriptRankedShots(ctx, "我们明天出发", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(aligned) != 1 || aligned[0].ID != shotID {
		t.Fatalf("aligned phrase must hit the shot, got %+v", aligned)
	}
	// A phrase that appears ONLY in the ASR segments must not match — the
	// asset's speech source is its alignment words.
	asrOnly, err := repo.TranscriptRankedShots(ctx, "完全无关", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(asrOnly) != 0 {
		t.Fatalf("ASR-only phrase must not match an aligned asset, got %+v", asrOnly)
	}
}

// TestSearchV2TranscriptAsrRetranscribeReplacesSegments pins the write path:
// re-transcribing an asset replaces its asr_segments, so the latest
// transcript's speech is what the channel sees.
func TestSearchV2TranscriptAsrRetranscribeReplacesSegments(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "asr-retranscribe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	shotID := seedAsrOnlyAsset(t, repo, "asset-retranscribe", 0, 5000)
	if err := repo.SaveTranscript(ctx, "asset-retranscribe", "stepfun", "stepfun-asr", "asr-v1", domain.Transcript{
		Language: "zh",
		Text:     "第一个版本",
		Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 1000, Text: "第一个版本"}},
	}, "", ""); err != nil {
		t.Fatal(err)
	}
	// Re-transcribe with a NEW input hash: the old segments must be replaced.
	if err := repo.SaveTranscript(ctx, "asset-retranscribe", "stepfun", "stepfun-asr", "asr-v2", domain.Transcript{
		Language: "zh",
		Text:     "第二个版本",
		Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 1000, Text: "第二个版本"}},
	}, "", ""); err != nil {
		t.Fatal(err)
	}
	if hits, err := repo.TranscriptRankedShots(ctx, "第一个版本", 10); err != nil {
		t.Fatal(err)
	} else if len(hits) != 0 {
		t.Fatalf("superseded ASR segments must not match, got %+v", hits)
	}
	if hits, err := repo.TranscriptRankedShots(ctx, "第二个版本", 10); err != nil {
		t.Fatal(err)
	} else if len(hits) != 1 || hits[0].ID != shotID {
		t.Fatalf("current ASR segments must hit the shot, got %+v", hits)
	}
}

// TestSearchV2TranscriptAsrWithoutTimingIsNotSearchable pins the Timed()
// contract: a 0-0 placeholder segment carries no placement and is dropped, so
// an ASR provider that emits no real timing contributes no shot-level speech.
func TestSearchV2TranscriptAsrWithoutTimingIsNotSearchable(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "asr-untimed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	seedAsrOnlyAsset(t, repo, "asset-untimed", 0, 5000)
	// Placeholder 0-0 segment (Qwen/Volcengine shape).
	if err := repo.SaveTranscript(ctx, "asset-untimed", "qwen", "qwen-asr", "asr-1", domain.Transcript{
		Language: "zh",
		Text:     "无声的素材",
		Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 0, Text: "无声的素材"}},
	}, "", ""); err != nil {
		t.Fatal(err)
	}
	hits, err := repo.TranscriptRankedShots(ctx, "无声的素材", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("placeholder 0-0 ASR segment must not be shot-searchable, got %+v", hits)
	}
}
