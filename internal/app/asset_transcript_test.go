package app

import (
	"context"
	"errors"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// transcriptFallbackRepo answers only the two reads AssetTranscript uses —
// GetAlignmentWords and GetTranscript — and panics on anything else via the
// embedded nil Repository, keeping the fake honest about how narrow the
// exercised surface is.
type transcriptFallbackRepo struct {
	Repository
	words    []domain.AlignmentWord
	full     *domain.Transcript
	wordsErr error
	fullErr  error
}

func (r *transcriptFallbackRepo) GetAlignmentWords(_ context.Context, _ string) ([]domain.AlignmentWord, error) {
	return r.words, r.wordsErr
}

func (r *transcriptFallbackRepo) GetTranscript(_ context.Context, _ string) (*domain.Transcript, error) {
	return r.full, r.fullErr
}

func floatPtr(v float64) *float64 { return &v }

func TestAssetTranscriptPrefersAlignmentWords(t *testing.T) {
	words := []domain.AlignmentWord{
		{StartMS: 0, EndMS: 260, Text: "你好", Confidence: floatPtr(0.94)},
		{StartMS: 260, EndMS: 620, Text: "世界"},
	}
	repo := &transcriptFallbackRepo{
		words: words,
		full: &domain.Transcript{
			Language: "zh",
			Text:     "ASR text",
			Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 1200, Text: "ASR sentence"}},
		},
	}
	out, err := (&Service{repo: repo}).AssetTranscript(context.Background(), "asset-1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != "aligned" {
		t.Fatalf("source = %q, want aligned", out.Source)
	}
	if len(out.Words) != 2 || out.Words[0].Text != "你好" || out.Words[0].Confidence == nil || *out.Words[0].Confidence != 0.94 {
		t.Fatalf("words = %+v, want the alignment stream intact", out.Words)
	}
	// text/segments are promoted from the word stream, not the ASR sentence.
	if out.Text != "你好 世界" {
		t.Fatalf("text = %q, want promoted '你好 世界'", out.Text)
	}
	if len(out.Segments) != 2 || out.Segments[0].StartMS != 0 || out.Segments[1].StartMS != 260 {
		t.Fatalf("segments = %+v, want per-word placement", out.Segments)
	}
	// language is taken from the ASR transcript row, which is independent of
	// the alignment run.
	if out.Language != "zh" {
		t.Fatalf("language = %q, want zh from the ASR transcript row", out.Language)
	}
}

func TestAssetTranscriptAlignmentWithNoAsrLeavesLanguageEmpty(t *testing.T) {
	words := []domain.AlignmentWord{{StartMS: 0, EndMS: 260, Text: "你好"}}
	repo := &transcriptFallbackRepo{words: words}
	out, err := (&Service{repo: repo}).AssetTranscript(context.Background(), "asset-1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != "aligned" || out.Language != "" {
		t.Fatalf("out = %+v, want source=aligned and empty language", out)
	}
}

func TestAssetTranscriptFallsBackToAsrWithoutWords(t *testing.T) {
	repo := &transcriptFallbackRepo{
		full: &domain.Transcript{
			Language: "zh",
			Text:     "完整文本",
			Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 1200, Text: "句子"}},
		},
	}
	out, err := (&Service{repo: repo}).AssetTranscript(context.Background(), "asset-1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != "asr" {
		t.Fatalf("source = %q, want asr", out.Source)
	}
	if out.Words != nil {
		t.Fatalf("words = %+v, want omitted for asr", out.Words)
	}
	if out.Text != "完整文本" || len(out.Segments) != 1 || out.Segments[0].Text != "句子" {
		t.Fatalf("out = %+v, want the ASR transcript verbatim", out)
	}
}

func TestAssetTranscriptNeitherAnswersNotFound(t *testing.T) {
	repo := &transcriptFallbackRepo{}
	_, err := (&Service{repo: repo}).AssetTranscript(context.Background(), "asset-1")
	if !errors.Is(err, ErrTranscriptNotFound) {
		t.Fatalf("err = %v, want ErrTranscriptNotFound", err)
	}
}

func TestAssetTranscriptPropagatesRepositoryErrors(t *testing.T) {
	sentinel := errors.New("db down")
	repo := &transcriptFallbackRepo{wordsErr: sentinel}
	if _, err := (&Service{repo: repo}).AssetTranscript(context.Background(), "asset-1"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the repository error", err)
	}
	repo = &transcriptFallbackRepo{fullErr: sentinel}
	if _, err := (&Service{repo: repo}).AssetTranscript(context.Background(), "asset-1"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the repository error", err)
	}
}
