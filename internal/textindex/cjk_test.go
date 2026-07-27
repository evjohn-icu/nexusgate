package textindex

import "testing"

func TestCJKBigramSegmentationAndMixedQueryPreservePhraseOrder(t *testing.T) {
	if got, want := Segment("雨夜街道 street"), "雨夜 夜街 街道 street"; got != want {
		t.Fatalf("segment=%q want %q", got, want)
	}
	if got, want := FTSQuery("雨夜街道 street"), `"雨夜 夜街 街道" AND "street"`; got != want {
		t.Fatalf("query=%q want %q", got, want)
	}
}
