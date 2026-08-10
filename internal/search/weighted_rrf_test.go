package search

import (
	"testing"
)

// equalWeightsMap returns the all-1.0 weight map: WeightedRRF with it must
// degenerate to plain RRF over the same channels.
func equalWeightsMap(signals ...string) map[string]float64 {
	w := map[string]float64{}
	for _, s := range signals {
		w[s] = 1.0
	}
	return w
}

func TestWeightedRRFEqualWeightsMatchesPlainRRF(t *testing.T) {
	results := []ChannelResult{
		{Signal: SignalLexical, Candidates: []Candidate{
			{ShotID: "a", Signals: map[string]float64{SignalLexical: 0.8}},
			{ShotID: "b", Signals: map[string]float64{SignalLexical: 0.2}},
			{ShotID: "d", Signals: map[string]float64{SignalLexical: 0.1}},
		}},
		{Signal: SignalHeuristicSemantic, Candidates: []Candidate{
			{ShotID: "b", Signals: map[string]float64{SignalHeuristicSemantic: 0.9}},
			{ShotID: "c", Signals: map[string]float64{SignalHeuristicSemantic: 0.5}},
			{ShotID: "a", Signals: map[string]float64{SignalHeuristicSemantic: 0.4}},
		}},
		{Signal: SignalTranscript, Candidates: []Candidate{
			{ShotID: "c", Signals: map[string]float64{SignalTranscript: 0.7}},
			{ShotID: "d", Signals: map[string]float64{SignalTranscript: 0.3}},
		}},
	}
	plain := (&RRF{K: DefaultRRFK}).Fuse(results)
	weighted := (&WeightedRRF{K: DefaultRRFK, Weights: equalWeightsMap(
		SignalLexical, SignalHeuristicSemantic, SignalTranscript)}).Fuse(results)

	if len(plain) != len(weighted) {
		t.Fatalf("equal weights: fused %d, plain RRF fused %d", len(weighted), len(plain))
	}
	for i := range plain {
		if weighted[i].ShotID != plain[i].ShotID {
			t.Fatalf("rank %d: equal-weight order %s, plain RRF order %s", i, weighted[i].ShotID, plain[i].ShotID)
		}
		if diff(weighted[i].Score, plain[i].Score) > 1e-12 {
			t.Fatalf("rank %d (%s): equal-weight score %v, plain RRF score %v", i, weighted[i].ShotID, weighted[i].Score, plain[i].Score)
		}
	}
}

func TestWeightedRRFName(t *testing.T) {
	if got := (&WeightedRRF{}).Name(); got != SignalWeightedRRF {
		t.Fatalf("Name() = %q, want %q", got, SignalWeightedRRF)
	}
}

func TestWeightedRRFWeightsShiftRanking(t *testing.T) {
	// Transcript channel ranked a transcript hit last but weights give
	// transcript evidence far more pull than lexical — the transcript hit
	// must outrank the lexical hit.
	results := []ChannelResult{
		{Signal: SignalLexical, Candidates: []Candidate{
			{ShotID: "lexical-hit", Signals: map[string]float64{SignalLexical: 0.9}},
			{ShotID: "transcript-hit", Signals: map[string]float64{SignalLexical: 0.1}},
		}},
		{Signal: SignalTranscript, Candidates: []Candidate{
			{ShotID: "transcript-hit", Signals: map[string]float64{SignalTranscript: 0.95}},
		}},
	}
	weighted := (&WeightedRRF{K: DefaultRRFK, Weights: map[string]float64{
		SignalTranscript: 1.0,
		SignalLexical:    0.1,
	}}).Fuse(results)

	if len(weighted) != 2 {
		t.Fatalf("want 2 fused candidates, got %d", len(weighted))
	}
	if weighted[0].ShotID != "transcript-hit" {
		t.Fatalf("transcript hit must rank first, got %+v", weighted)
	}
	// transcript-hit: 0.1/62 (lexical rank 1) + 1.0/61 (transcript rank 0)
	want := 0.1/62 + 1.0/61
	if diff(weighted[0].Score, want) > 1e-12 {
		t.Fatalf("transcript-hit score = %v, want %v", weighted[0].Score, want)
	}
}

func TestWeightedRRFZeroWeightChannelContributesNothing(t *testing.T) {
	results := []ChannelResult{
		{Signal: SignalLexical, Candidates: []Candidate{
			{ShotID: "a", Signals: map[string]float64{SignalLexical: 0.9}},
		}},
		{Signal: SignalTranscript, Candidates: []Candidate{
			{ShotID: "b", Signals: map[string]float64{SignalTranscript: 0.9}},
		}},
	}
	fused := (&WeightedRRF{K: DefaultRRFK, Weights: map[string]float64{
		SignalLexical:    1.0,
		SignalTranscript: 0.0,
	}}).Fuse(results)

	if len(fused) != 1 || fused[0].ShotID != "a" {
		t.Fatalf("zero-weight channel must contribute nothing, got %+v", fused)
	}
	if diff(fused[0].Score, 1.0/61) > 1e-12 {
		t.Fatalf("a score = %v, want %v", fused[0].Score, 1.0/61)
	}
}

func TestWeightedRRFMissingWeightTreatedAsZero(t *testing.T) {
	results := []ChannelResult{
		{Signal: SignalLexical, Candidates: []Candidate{
			{ShotID: "a", Signals: map[string]float64{SignalLexical: 0.9}},
		}},
		{Signal: SignalMetadata, Candidates: []Candidate{
			{ShotID: "b", Signals: map[string]float64{SignalMetadata: 0.9}},
		}},
	}
	// Metadata has no entry at all: treated as weight 0.
	fused := (&WeightedRRF{K: DefaultRRFK, Weights: map[string]float64{
		SignalLexical: 1.0,
	}}).Fuse(results)

	if len(fused) != 1 || fused[0].ShotID != "a" {
		t.Fatalf("missing weight must act as zero, got %+v", fused)
	}
}

func TestWeightedRRFMergesSignalsAcrossChannels(t *testing.T) {
	results := []ChannelResult{
		{Signal: SignalLexical, Candidates: []Candidate{
			{ShotID: "a", Signals: map[string]float64{SignalLexical: 0.8}},
		}},
		{Signal: SignalHeuristicSemantic, Candidates: []Candidate{
			{ShotID: "a", Signals: map[string]float64{SignalHeuristicSemantic: 0.9}},
		}},
	}
	fused := (&WeightedRRF{K: DefaultRRFK, Weights: equalWeightsMap(
		SignalLexical, SignalHeuristicSemantic)}).Fuse(results)

	if len(fused) != 1 {
		t.Fatalf("want 1 fused candidate, got %d", len(fused))
	}
	signals := fused[0].Signals
	if signals[SignalLexical] != 0.8 || signals[SignalHeuristicSemantic] != 0.9 {
		t.Fatalf("candidate must keep both signals, got %+v", signals)
	}
}

func TestWeightedRRFZeroScoreCandidatesRemoved(t *testing.T) {
	results := []ChannelResult{
		{Signal: SignalLexical, Candidates: []Candidate{
			{ShotID: "a", Signals: map[string]float64{SignalLexical: 0.8}},
		}},
		{Signal: SignalMetadata, Candidates: []Candidate{
			{ShotID: "b", Signals: map[string]float64{SignalMetadata: 0.9}},
		}},
	}
	// b is ranked only by the zero-weight metadata channel: score 0, dropped.
	fused := (&WeightedRRF{K: DefaultRRFK, Weights: map[string]float64{
		SignalLexical:  1.0,
		SignalMetadata: 0.0,
	}}).Fuse(results)

	if len(fused) != 1 || fused[0].ShotID != "a" {
		t.Fatalf("zero-score candidates must be removed, got %+v", fused)
	}
}

func TestWeightedRRFKDefaultsTo60(t *testing.T) {
	results := []ChannelResult{
		{Signal: SignalLexical, Candidates: []Candidate{
			{ShotID: "a", Signals: map[string]float64{SignalLexical: 0.8}},
		}},
	}
	fused := (&WeightedRRF{Weights: map[string]float64{SignalLexical: 1.0}}).Fuse(results)
	if diff(fused[0].Score, 1.0/(DefaultRRFK+1)) > 1e-12 {
		t.Fatalf("K must default to %d, score = %v", DefaultRRFK, fused[0].Score)
	}
}
