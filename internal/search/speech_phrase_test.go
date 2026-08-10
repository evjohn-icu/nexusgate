package search

import (
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

func speechSpans(words ...domain.AlignmentWord) []domain.AlignmentWord { return words }
func sw(start, end int64, text string) domain.AlignmentWord {
	return domain.AlignmentWord{StartMS: start, EndMS: end, Text: text}
}

func TestMatchAlignedSpeechPhrase(t *testing.T) {
	tests := []struct {
		name, phrase string
		spans        []domain.AlignmentWord
		want         bool
	}{
		{"cjk sequence", "我们明天", speechSpans(sw(0, 100, "我们明天")), true},
		{"cjk split", "我们明天", speechSpans(sw(0, 100, "我"), sw(100, 200, "们"), sw(200, 300, "明"), sw(300, 400, "天")), true},
		{"cjk split gap over limit", "我们", speechSpans(sw(0, 100, "我"), sw(5000, 5100, "们")), false},
		{"cjk split gap at limit", "我们", speechSpans(sw(0, 100, "我"), sw(1600, 1700, "们")), true},
		{"cjk packed", "我们明天", speechSpans(sw(0, 100, "我们"), sw(100, 200, "明天")), true},
		{"cjk extra token rejected", "明天", speechSpans(sw(0, 100, "明天出发")), false},
		{"cjk phrase extra token rejected", "我们明天", speechSpans(sw(0, 100, "我们明天出发")), false},
		{"cjk legitimate packed split", "我们明天出发", speechSpans(sw(0, 100, "我们明天"), sw(100, 200, "出发")), true},
		{"cjk missing", "我们明天", speechSpans(sw(0, 100, "我们"), sw(100, 200, "出发")), false},
		{"cjk reordered", "我们明天", speechSpans(sw(0, 100, "明天"), sw(100, 200, "我们")), false},
		{"ascii whole word", "car", speechSpans(sw(0, 100, "car")), true},
		{"ascii not substring", "car", speechSpans(sw(0, 100, "carefree")), false},
		{"ascii intervening", "hello world", speechSpans(sw(0, 100, "hello"), sw(100, 200, "noise"), sw(200, 300, "world")), true},
		{"ascii reorder", "hello world", speechSpans(sw(0, 100, "world"), sw(100, 200, "hello")), false},
		{"mixed", "hello世界", speechSpans(sw(0, 100, "hello"), sw(100, 200, "世界")), true},
		{"gap 1500", "a b", speechSpans(sw(0, 100, "a"), sw(1600, 1700, "b")), true},
		{"gap 1501", "a b", speechSpans(sw(0, 100, "a"), sw(1601, 1700, "b")), false},
		{"empty", "", speechSpans(sw(0, 100, "anything")), false},
		{"shot boundary", "a b", speechSpans(sw(0, 100, "a")), false},
		{"no reuse", "a a", speechSpans(sw(0, 100, "a")), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchAlignedSpeechPhrase(tt.phrase, tt.spans); got != tt.want {
				t.Fatalf("match=%v, want %v", got, tt.want)
			}
		})
	}
}
