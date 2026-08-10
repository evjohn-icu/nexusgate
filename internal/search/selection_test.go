package search

import (
	"testing"
)

func TestFusionWeightedBlend(t *testing.T) {
	blend := &WeightedBlend{Weights: map[string]float64{SignalLexical: 0.3, SignalHeuristicSemantic: 0.7}}
	results := []ChannelResult{
		{Signal: SignalLexical, Candidates: []Candidate{
			{ShotID: "a", Signals: map[string]float64{SignalLexical: 0.8}},
			{ShotID: "b", Signals: map[string]float64{SignalLexical: 0.2}},
		}},
		{Signal: SignalHeuristicSemantic, Candidates: []Candidate{
			{ShotID: "a", Signals: map[string]float64{SignalHeuristicSemantic: 0.9}},
			{ShotID: "c", Signals: map[string]float64{SignalHeuristicSemantic: 0.5}},
		}},
	}
	fused := blend.Fuse(results)
	if len(fused) != 3 {
		t.Fatalf("want 3 fused candidates, got %d", len(fused))
	}
	if fused[0].ShotID != "a" {
		t.Fatalf("a must rank first (0.87), got %+v", fused)
	}
	// a: 0.3*0.8 + 0.7*0.9 = 0.87; c: 0.35; b: 0.06
	if fused[1].ShotID != "c" || fused[2].ShotID != "b" {
		t.Fatalf("order must be a,c,b got %s,%s,%s", fused[0].ShotID, fused[1].ShotID, fused[2].ShotID)
	}
}

func TestFusionRRFMath(t *testing.T) {
	rrf := &RRF{K: 60}
	results := []ChannelResult{
		{Signal: SignalLexical, Candidates: []Candidate{
			{ShotID: "a"}, {ShotID: "b"},
		}},
		{Signal: SignalHeuristicSemantic, Candidates: []Candidate{
			{ShotID: "b"}, {ShotID: "c"},
		}},
	}
	fused := rrf.Fuse(results)
	score := map[string]float64{}
	for _, c := range fused {
		score[c.ShotID] = c.Score
	}
	// a: 1/61, b: 1/62 + 1/61, c: 1/62
	if diff(score["a"], 1.0/61) > 1e-12 {
		t.Fatalf("a score = %v, want %v", score["a"], 1.0/61)
	}
	if diff(score["b"], 1.0/62+1.0/61) > 1e-12 {
		t.Fatalf("b score = %v, want %v", score["b"], 1.0/62+1.0/61)
	}
	if len(fused) != 3 || fused[0].ShotID != "b" {
		t.Fatalf("b must rank first, got %+v", fused)
	}
}

func TestFusionRRFMergesAllSignals(t *testing.T) {
	rrf := &RRF{K: 60}
	results := []ChannelResult{
		{Signal: SignalLexical, Candidates: []Candidate{
			{ShotID: "shotX", Signals: map[string]float64{SignalLexical: 0.9}},
		}},
		{Signal: SignalTranscript, Candidates: []Candidate{
			{ShotID: "shotX", Signals: map[string]float64{SignalTranscript: 0.7}},
		}},
		{Signal: SignalTextEmbedding, Candidates: []Candidate{
			{ShotID: "shotX", Signals: map[string]float64{SignalTextEmbedding: 0.5}},
		}},
	}
	fused := rrf.Fuse(results)
	if len(fused) != 1 {
		t.Fatalf("want 1 fused candidate, got %d: %+v", len(fused), fused)
	}
	c := fused[0]
	if c.ShotID != "shotX" {
		t.Fatalf("want shotX, got %s", c.ShotID)
	}
	// All three channels' real signals must survive, not just the first's.
	for _, signal := range []string{SignalLexical, SignalTranscript, SignalTextEmbedding} {
		if _, ok := c.Signals[signal]; !ok {
			t.Fatalf("candidate %s missing signal %s: %+v", c.ShotID, signal, c.Signals)
		}
	}
	if diff(c.Signals[SignalLexical], 0.9) > 1e-12 {
		t.Fatalf("lexical signal = %v, want 0.9", c.Signals[SignalLexical])
	}
	if diff(c.Signals[SignalTranscript], 0.7) > 1e-12 {
		t.Fatalf("transcript signal = %v, want 0.7", c.Signals[SignalTranscript])
	}
	if diff(c.Signals[SignalTextEmbedding], 0.5) > 1e-12 {
		t.Fatalf("text_embedding signal = %v, want 0.5", c.Signals[SignalTextEmbedding])
	}
	// Score must be the RRF sum over the three ranks (ranks 0, 0, 0).
	want := 3.0 / 61.0
	if diff(c.Score, want) > 1e-12 {
		t.Fatalf("score = %v, want %v", c.Score, want)
	}
}

func TestFusionRRFDeterministicTieBreak(t *testing.T) {
	rrf := &RRF{K: 60}
	results := []ChannelResult{
		{Signal: SignalLexical, Candidates: []Candidate{
			{ShotID: "b"}, {ShotID: "a"},
		}},
	}
	first := rrf.Fuse(results)
	second := rrf.Fuse(results)
	// b ranks 0 (1/61) and a ranks 1 (1/62): b wins the rank.
	if first[0].ShotID != second[0].ShotID || first[0].ShotID != "b" {
		t.Fatalf("RRF must be deterministic with ID tie-break, got %+v / %+v", first, second)
	}
}

func diff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

func TestSelectionNearDuplicateHardSkip(t *testing.T) {
	sel := NewSelection(DefaultSelectionOptions())
	candidates := []Candidate{
		{ShotID: "a", AssetID: "A001", StartMS: 10_100, Score: 0.9},
		{ShotID: "b", AssetID: "A001", StartMS: 10_900, Score: 0.89}, // near-duplicate burst
		{ShotID: "c", AssetID: "A001", StartMS: 11_500, Score: 0.88},
		{ShotID: "d", AssetID: "B001", StartMS: 0, Score: 0.3},
	}
	selected := sel.Select(candidates, 10)
	if len(selected) != 2 {
		t.Fatalf("near-duplicate burst must collapse to one + one other, got %d: %+v", len(selected), selected)
	}
	if selected[0].ShotID != "a" || selected[1].ShotID != "d" {
		t.Fatalf("want [a, d], got %+v", selected)
	}
}

func TestSelectionDisabledPreservesOrder(t *testing.T) {
	sel := NewSelection(SelectionOptions{Diversity: 0})
	candidates := []Candidate{
		{ShotID: "a", AssetID: "A001", StartMS: 10_100, Score: 0.5},
		{ShotID: "b", AssetID: "A001", StartMS: 10_200, Score: 0.4},
	}
	selected := sel.Select(candidates, 10)
	if len(selected) != 2 || selected[0].ShotID != "a" {
		t.Fatalf("diversity 0 must pass through, got %+v", selected)
	}
}

func TestSelectionSameAssetSoftPenalty(t *testing.T) {
	sel := NewSelection(DefaultSelectionOptions())
	candidates := []Candidate{
		{ShotID: "a", AssetID: "A001", StartMS: 0, Score: 1.0},
		{ShotID: "b", AssetID: "A001", StartMS: 60_000, Score: 0.95}, // same asset, far
		{ShotID: "c", AssetID: "B001", StartMS: 0, Score: 0.9},
	}
	selected := sel.Select(candidates, 10)
	// FIXED: this test previously asserted b keeps its rank — the old
	// implementation only ever removed candidates or truncated the fused
	// order, so b (0.95 - 0.25 = 0.70) stayed above c (0.90). Selection is
	// now genuinely greedy: after a's penalty b drops below c, so the output
	// is re-ranked to a, c, b.
	if len(selected) != 3 || selected[0].ShotID != "a" || selected[1].ShotID != "c" || selected[2].ShotID != "b" {
		t.Fatalf("far same-asset shot must be demoted below the later higher-scoring shot, got %+v", selected)
	}
}

func TestSelectionReordersAfterSameAssetPenalty(t *testing.T) {
	sel := NewSelection(DefaultSelectionOptions())
	// scale = 0.2/0.2 * 1.00 = 1.00, so B's same-asset penalty is 0.25 and its
	// adjusted score 0.92 - 0.25 = 0.67 < C's 0.90: greedy re-ranking must
	// surface C before B even though B arrived first in fused order.
	candidates := []Candidate{
		{ShotID: "A", AssetID: "asset1", StartMS: 0, Score: 1.00},
		{ShotID: "B", AssetID: "asset1", StartMS: 60_000, Score: 0.92}, // same asset, far
		{ShotID: "C", AssetID: "asset2", StartMS: 0, Score: 0.90},
	}
	selected := sel.Select(candidates, 10)
	if len(selected) != 3 || selected[0].ShotID != "A" || selected[1].ShotID != "C" || selected[2].ShotID != "B" {
		t.Fatalf("greedy re-ranking must output A, C, B, got %+v", selected)
	}
	// Near-time differences still hard-skip: B (3s after A, same asset) is
	// within NearTimeWindowMS and must vanish even though re-ranking is on.
	sel2 := NewSelection(DefaultSelectionOptions())
	burst := []Candidate{
		{ShotID: "A", AssetID: "asset1", StartMS: 0, Score: 1.00},
		{ShotID: "B", AssetID: "asset1", StartMS: 3_000, Score: 0.95}, // near duplicate
		{ShotID: "C", AssetID: "asset2", StartMS: 0, Score: 0.90},
	}
	burstSelected := sel2.Select(burst, 10)
	if len(burstSelected) != 2 || burstSelected[0].ShotID != "A" || burstSelected[1].ShotID != "C" {
		t.Fatalf("near-time difference must hard-skip, got %+v", burstSelected)
	}
}

func TestSelectionLimit(t *testing.T) {
	sel := NewSelection(DefaultSelectionOptions())
	candidates := []Candidate{
		{ShotID: "a", AssetID: "A", Score: 1.0},
		{ShotID: "b", AssetID: "B", Score: 0.9},
		{ShotID: "c", AssetID: "C", Score: 0.8},
	}
	selected := sel.Select(candidates, 2)
	if len(selected) != 2 {
		t.Fatalf("limit 2 must hold, got %d", len(selected))
	}
}

func TestSelectionDisplacesPenalizedCandidatePastLimit(t *testing.T) {
	// Regression: the old loop broke as soon as len(selected)==limit, so a
	// later candidate that outranks an earlier heavily-penalized one was
	// never seen. B (same asset as A) drops to 0.70 after the penalty; C
	// (0.90, other asset) must displace it even though the list is already
	// full at [A, B].
	sel := NewSelection(DefaultSelectionOptions())
	candidates := []Candidate{
		{ShotID: "A", AssetID: "asset1", StartMS: 0, Score: 1.00},
		{ShotID: "B", AssetID: "asset1", StartMS: 60_000, Score: 0.95},
		{ShotID: "C", AssetID: "asset2", StartMS: 0, Score: 0.90},
	}
	selected := sel.Select(candidates, 2)
	if len(selected) != 2 {
		t.Fatalf("limit 2 must hold, got %d: %+v", len(selected), selected)
	}
	if selected[0].ShotID != "A" || selected[1].ShotID != "C" {
		t.Fatalf("C must displace the penalized B at the limit, got %+v", selected)
	}
	if selected[1].Score != 0.90 {
		t.Fatalf("C keeps its unpenalized score, got %v", selected[1].Score)
	}
}

func TestSelectionClampsPenaltyToZero(t *testing.T) {
	// Regression: a candidate whose penalty exceeds its score kept the
	// unpenalized fused score and its original rank. It must rank at 0.
	// scale = 0.2/0.2 * 1.00 = 1.00; B's penalty = 0.25 > its 0.20 score.
	sel := NewSelection(DefaultSelectionOptions())
	candidates := []Candidate{
		{ShotID: "A", AssetID: "asset1", StartMS: 0, Score: 1.00},
		{ShotID: "B", AssetID: "asset1", StartMS: 60_000, Score: 0.20},
	}
	selected := sel.Select(candidates, 10)
	if len(selected) != 2 {
		t.Fatalf("want both candidates, got %d: %+v", len(selected), selected)
	}
	if selected[1].Score != 0.0 {
		t.Fatalf("penalized candidate must rank at 0, got %v", selected[1].Score)
	}
}

func TestWeightedBlendDoesNotMutateChannelSignals(t *testing.T) {
	// The fused candidate's Signals map must be a clone: writing a later
	// channel's signal into it must never leak back into the channel's own
	// candidate.
	channelLex := []Candidate{{ShotID: "x", Signals: map[string]float64{SignalLexical: 0.9}}}
	channelSem := []Candidate{{ShotID: "x", Signals: map[string]float64{SignalHeuristicSemantic: 0.7}}}
	blend := &WeightedBlend{Weights: map[string]float64{SignalLexical: 0.5, SignalHeuristicSemantic: 0.5}}
	fused := blend.Fuse([]ChannelResult{
		{Signal: SignalLexical, Candidates: channelLex},
		{Signal: SignalHeuristicSemantic, Candidates: channelSem},
	})
	if len(fused) != 1 || len(fused[0].Signals) != 2 {
		t.Fatalf("fused candidate must merge both signals, got %+v", fused)
	}
	if len(channelLex[0].Signals) != 1 {
		t.Fatalf("channel's own candidate must be untouched, got %+v", channelLex[0].Signals)
	}
	if _, ok := channelLex[0].Signals[SignalHeuristicSemantic]; ok {
		t.Fatalf("semantic signal leaked into the lexical channel's candidate: %+v", channelLex[0].Signals)
	}
}
