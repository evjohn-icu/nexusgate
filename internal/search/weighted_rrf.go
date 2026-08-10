package search

import "maps"

// SignalWeightedRRF is the signal key the fused score is stored under when
// WeightedRRF is the active fusion strategy. It is a local constant: the
// Signal family in query.go is owned by another agent and must not be edited
// here.
const SignalWeightedRRF = "weighted_rrf"

// WeightedRRF is the weighted form of reciprocal-rank fusion: a shot scores
// sum(weight[channel] / (k + rank + 1)) over the channels that ranked it.
// Unlike plain RRF it honors ChannelWeights, so the profile's differentiated
// weights are effective in production rather than only under WeightedBlend.
//
// With equal weights across every channel it degenerates exactly to plain RRF
// — the fused ordering and scores are identical (each contribution is
// weight / (k + rank + 1) with weight = 1) — which is the invariant that lets
// the default auto/fact path stay byte-identical to today's ranking while the
// other intents use their differentiated weights.
//
// A channel whose signal has no entry in Weights contributes nothing
// (weight 0). The caller — the integrator wiring the default pipeline — can
// therefore skip zero-weight channels entirely at construction; WeightedRRF
// itself treats a missing weight as 0 rather than erroring.
type WeightedRRF struct {
	// Weights maps a channel signal to its RRF contribution weight. A signal
	// absent from the map has weight 0 and contributes nothing.
	Weights map[string]float64
	// K is the RRF constant; DefaultRRFK (60) applies when K <= 0.
	K int
}

func (f *WeightedRRF) Name() string { return SignalWeightedRRF }

// Fuse ranks every unioned candidate by weighted reciprocal rank. Candidates
// with no positive fused score are dropped, mirroring RRF's "no evidence, no
// rank" rule — a shot only scores where a channel actually ranked it.
//
// A candidate seen from several channels keeps every signal: the Score is
// the weighted rank evidence, while its Signals map holds each channel's real
// value so the API score map stays per-signal explainable.
func (f *WeightedRRF) Fuse(results []ChannelResult) []Candidate {
	k := f.K
	if k <= 0 {
		k = DefaultRRFK
	}
	index := map[string]int{}
	var fused []Candidate
	for _, channel := range results {
		weight := f.Weights[channel.Signal]
		if weight <= 0 {
			// Zero-weight (or unknown) channel: skip the whole channel's
			// work — its contribution is 0 by definition.
			continue
		}
		for rank, candidate := range channel.Candidates {
			idx, ok := index[candidate.ShotID]
			if !ok {
				idx = len(fused)
				index[candidate.ShotID] = idx
				// Copy the signal map so later merges never mutate the
				// channel's own candidate objects.
				candidate.Signals = maps.Clone(candidate.Signals)
				fused = append(fused, candidate)
			}
			fused[idx].Score += weight / (float64(k) + float64(rank) + 1)
			// A candidate seen from several channels keeps every signal: the
			// score is the weighted rank sum over ALL of them.
			for signal, value := range candidate.Signals {
				fused[idx].Signals[signal] = value
			}
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

var (
	_ FusionStrategy = (*WeightedRRF)(nil)
)
