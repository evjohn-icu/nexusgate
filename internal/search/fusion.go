package search

import (
	"maps"
)

// WeightedBlend is the classic weighted-sum fusion: Score = sum(weight *
// signal). It is the compatibility strategy (the legacy 0.70/0.30 blend is
// just WeightedBlend over the two legacy signals) and the intent-profile
// strategy for the v2 path when weights are set.
type WeightedBlend struct {
	Weights map[string]float64
}

func (f *WeightedBlend) Name() string { return SignalBlend }

// Fuse scores every unioned candidate by its per-signal weights. Candidates
// with no positive fused score are dropped — a signal must actually rank a
// shot for it to contribute, the same "no evidence, no rank" rule the legacy
// blend applies.
func (f *WeightedBlend) Fuse(results []ChannelResult) []Candidate {
	index := map[string]int{}
	var out []Candidate
	for _, channel := range results {
		for _, candidate := range channel.Candidates {
			idx, ok := index[candidate.ShotID]
			if !ok {
				idx = len(out)
				index[candidate.ShotID] = idx
				// Clone the first sighting's signals so the fused candidate
				// never shares (and later mutates) the channel's own map —
				// same invariant RRF and WeightedRRF keep.
				candidate.Signals = maps.Clone(candidate.Signals)
				out = append(out, candidate)
				continue
			}
			// A candidate seen from several channels keeps every signal: the
			// score is the weighted sum over ALL of them.
			for signal, value := range candidate.Signals {
				out[idx].Signals[signal] = value
			}
		}
	}
	scored := out[:0]
	for _, candidate := range out {
		var score float64
		for signal, weight := range f.Weights {
			score += weight * candidate.Signals[signal]
		}
		if score <= 0 {
			continue
		}
		candidate.Score = score
		scored = append(scored, candidate)
	}
	sortCandidates(scored)
	return scored
}

// RRF is reciprocal-rank fusion: each channel ranks its candidates, and a
// shot scores sum(1/(k+rank)) over the channels that ranked it. RRF cannot
// let one signal's noise ride on another's strong score the way a weighted
// sum can — a shot only scores where a signal actually ranked it. k defaults
// to 60 (the Cormack, Clarke & Buettcher SIGIR 2009 constant).
//
// A candidate keeps every channel's signal: when a shot is ranked by several
// channels, its Signals map holds each channel's real value, so the API
// scores map is per-signal explainable even though the fused Score is the RRF
// sum. The score is the rank evidence; the signals are the per-signal
// evidence behind it.
type RRF struct {
	K int
}

func (f *RRF) Name() string { return SignalRRF }

func (f *RRF) Fuse(results []ChannelResult) []Candidate {
	k := f.K
	if k <= 0 {
		k = DefaultRRFK
	}
	index := map[string]int{}
	var fused []Candidate
	for _, channel := range results {
		for rank, candidate := range channel.Candidates {
			idx, ok := index[candidate.ShotID]
			if !ok {
				idx = len(fused)
				index[candidate.ShotID] = idx
				// Clone the first sighting's signals so the fused candidate
				// never shares (and later mutates) the channel's own map —
				// same invariant WeightedRRF keeps.
				candidate.Signals = maps.Clone(candidate.Signals)
				fused = append(fused, candidate)
			}
			// A candidate seen from several channels keeps every channel's
			// signal: the RRF score is summed over all ranks, and the Signals map
			// carries each channel's real value so the API scores map is
			// per-signal explainable.
			for signal, value := range candidate.Signals {
				fused[idx].Signals[signal] = value
			}
			fused[idx].Score += 1 / (float64(k) + float64(rank) + 1)
		}
	}
	out := fused[:0]
	for _, candidate := range fused {
		if candidate.Score > 0 {
			out = append(out, candidate)
		}
	}
	sortCandidates(out)
	return out
}

// DefaultRRFK is the shared RRF constant (see repository.go's copy — the two
// must never drift apart, which is why the v2 compatibility test pins them).
const DefaultRRFK = 60

var (
	_ FusionStrategy = (*WeightedBlend)(nil)
	_ FusionStrategy = (*RRF)(nil)
)
