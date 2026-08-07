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
	if err := repo.SaveAlignment(ctx, "asset-align", "aligner", "m", "hash-1", "{}", first); err != nil {
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
	if err := repo.SaveAlignment(ctx, "asset-align", "aligner", "m", "hash-2", "{}", second); err != nil {
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
