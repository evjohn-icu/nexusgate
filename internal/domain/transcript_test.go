package domain

import "testing"

// Timed() is the timing-trust classification the whole window-slicing logic
// keys on: a 0-0 placeholder segment (Qwen/Volcengine ASR shape) is not a
// timestamp, a real span is. The degenerate-inverted case must never count.
func TestTranscriptTimedClassification(t *testing.T) {
	cases := []struct {
		name     string
		segments []TranscriptSegment
		want     bool
	}{
		{"empty", nil, false},
		{"0-0 placeholder only", []TranscriptSegment{{StartMS: 0, EndMS: 0, Text: "full text"}}, false},
		{"inverted only", []TranscriptSegment{{StartMS: 100, EndMS: 50, Text: "bad"}}, false},
		{"real span", []TranscriptSegment{{StartMS: 10, EndMS: 20, Text: "ok"}}, true},
		{"mixed placeholder plus real", []TranscriptSegment{{StartMS: 0, EndMS: 0, Text: "full"}, {StartMS: 310_000, EndMS: 315_000, Text: "hello"}}, true},
		{"zero-span real-looking", []TranscriptSegment{{StartMS: 5, EndMS: 5, Text: "same"}}, false},
	}
	for _, c := range cases {
		got := (Transcript{Text: "x", Segments: c.segments}).Timed()
		if got != c.want {
			t.Errorf("%s: Timed() = %v, want %v", c.name, got, c.want)
		}
	}
}
