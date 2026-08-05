package textindex

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestCJKBigramSegmentationAndMixedQueryPreservePhraseOrder(t *testing.T) {
	if got, want := Segment("雨夜街道 street"), "雨夜 夜街 街道 street"; got != want {
		t.Fatalf("segment=%q want %q", got, want)
	}
	if got, want := FTSQuery("雨夜街道 street"), `"雨夜 夜街 街道" AND "street"`; got != want {
		t.Fatalf("query=%q want %q", got, want)
	}
}

// TestCJKAdjacentToASCIIWordCharsStillBigrams pins the recall bug: a CJK run
// immediately preceded by an ASCII letter, digit or underscore must still be
// segmented into its own bigrams. unicode.IsLetter is true for CJK
// characters too, so the ASCII word-scan in Chunks previously kept eating
// past the CJK boundary instead of yielding to the CJK branch, producing one
// unsplittable token with no bigrams — and FTSQuery always emits a bigram
// phrase, so a query for the CJK text could never match what got indexed.
func TestCJKAdjacentToASCIIWordCharsStillBigrams(t *testing.T) {
	got := Segment("clip_雨夜街道_final.mp4")
	tokens := make(map[string]bool)
	for _, tok := range strings.Fields(got) {
		tokens[tok] = true
	}
	// Checking whole-token membership, not strings.Contains(got, want): the
	// buggy segmenter merges everything into one unsplit token like
	// "clip_雨夜街道_final", which contains "雨夜" as a raw substring without
	// it ever existing as its own bigram token — a Contains check would pass
	// on that broken output for the wrong reason.
	for _, want := range []string{"雨夜", "夜街", "街道"} {
		if !tokens[want] {
			t.Fatalf("segment=%q missing bigram token %q, got tokens %v", got, want, tokens)
		}
	}
}

// TestCJKSegmentsJapaneseAndKorean confirms that hiragana, katakana, hangul
// and CJK compatibility ideographs are all recognised as CJK and bigrammed,
// so Japanese and Korean text is searchable in the same FTS5 index as Chinese.
func TestCJKSegmentsJapaneseAndKorean(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string // bigram tokens that must appear
	}{
		{
			name:  "hiragana",
			input: "ありがとう",
			want:  []string{"あり", "りが", "がと", "とう"},
		},
		{
			name:  "katakana",
			input: "テスト",
			want:  []string{"テス", "スト"},
		},
		{
			name:  "hangul",
			input: "감사합니다",
			want:  []string{"감사", "사합", "합니", "니다"},
		},
		{
			name:  "mixed japanese",
			input: "東京タワー",
			want:  []string{"東京", "京タ", "タワ", "ワー"},
		},
		{
			name:  "cjk compatible",
			input: "金屬",
			want:  []string{"金屬"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Segment(tt.input)
			tokens := make(map[string]bool)
			for _, tok := range strings.Fields(got) {
				tokens[tok] = true
			}
			for _, want := range tt.want {
				if !tokens[want] {
					t.Fatalf("segment=%q missing bigram token %q, got tokens %v", got, want, tokens)
				}
			}
		})
	}
}

// TestCJKAdjacentToASCIIMatchesInRealFTS5 is the end-to-end proof: index the
// same filename the way the Hub repository does (asset_search's own
// tokenize='unicode61', see migrations/0002_pipeline.sql) into a real FTS5
// table, then MATCH it with the same FTSQuery a search request would send.
// A string-level assertion on Segment's output cannot catch a mismatch
// between how the writer and the query side of textindex tokenize the same
// input; only round-tripping through SQLite can.
func TestCJKAdjacentToASCIIMatchesInRealFTS5(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE VIRTUAL TABLE asset_search USING fts5(filename, tokenize = 'unicode61')`); err != nil {
		t.Fatalf("create fts5 table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO asset_search(filename) VALUES (?)`, Segment("clip_雨夜街道_final.mp4")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var count int
	query := FTSQuery("雨夜街道")
	if err := db.QueryRow(`SELECT count(*) FROM asset_search WHERE asset_search MATCH ?`, query).Scan(&count); err != nil {
		t.Fatalf("match query=%q: %v", query, err)
	}
	if count != 1 {
		t.Fatalf("match query=%q got %d rows, want 1 (asset should be findable by its CJK segment)", query, count)
	}
}
