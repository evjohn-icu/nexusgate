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
	// a keeps 1.0; b's soft penalty must not erase its advantage over c's 0.9.
	if selected[0].ShotID != "a" || selected[1].ShotID != "b" {
		t.Fatalf("far same-asset shot keeps its rank after soft penalty, got %+v", selected)
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
