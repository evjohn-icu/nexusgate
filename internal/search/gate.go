package search

import (
	"context"
)

// EvidenceGate is the fact-intent enforcement of the "claims may not be
// fuzzy" principle. For fact and negative queries the gate refuses to let a
// shot into the top results purely on similarity: a must constraint with
// contradictory evidence drops the shot, a must with no confirmed evidence
// drops it too, and partial support downranks rather than pretends. For every
// other intent the gate still computes evidence (the response reports it) but
// never filters — mood queries are similarity questions, not fact claims.
//
// The gate never asserts absence: a MustNot constraint only excludes a shot
// whose own evidence shows the forbidden thing (structured or explicit
// description mention); every other shot is kept and its evidence is unknown.
type EvidenceGate struct {
	opts Options
}

func NewEvidenceGate(opts Options) *EvidenceGate {
	return &EvidenceGate{opts: opts}
}

// Gate applies the gate to fused candidates and returns the survivors plus
// the per-candidate evidence map. Evidence is computed for the pool whether
// or not the gate filters, so the response can always explain a result.
// When filter is false (non-fact intents, or the gate disabled) the evidence
// is still produced but nothing is dropped.
//
// The gate rejects a candidate only when a must constraint has NO evidence at
// all (unknown, or actively contradicted). A description-only mention is
// "possible" — weak, and reported as such — but the golden corpus's own model
// observations live in shot descriptions, so dropping everything below
// "confirmed" would kill recall; the downrank factor below still punishes
// partial support.
//
// Transcript spans are fetched for the whole candidate pool through the
// store's batch contract. The pool is bounded (the service caps it before
// gating), and the per-candidate evidence work stays in memory.
func (g *EvidenceGate) Gate(ctx context.Context, store ShotStore, q SearchQuery, candidates []Candidate, filter bool) ([]Candidate, map[string][]Evidence, error) {
	evidenceByID := make(map[string][]Evidence, len(candidates))
	if len(candidates) == 0 {
		return []Candidate{}, evidenceByID, nil
	}
	requests := make([]TranscriptSpanRequest, 0, len(candidates))
	for _, candidate := range candidates {
		requests = append(requests, TranscriptSpanRequest{
			ShotID:  candidate.ShotID,
			AssetID: candidate.AssetID,
			StartMS: candidate.StartMS,
			EndMS:   candidate.EndMS,
		})
	}
	spansByID, err := store.ShotTranscriptSpansBatch(ctx, requests)
	if err != nil {
		return nil, nil, err
	}
	active := q.Intent == IntentFact && filter
	out := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		evidence := evaluateConstraints(q, candidate, spansByID[candidate.ShotID])
		evidenceByID[candidate.ShotID] = evidence
		if active && g.opts.GateFact {
			verdict := evidenceForGate(q, evidence)
			// A pure-negative query has no positive musts to confirm; the
			// mustNot observation check alone governs it. A must with no
			// evidence at all rejects the shot even when another must is
			// confirmed — the spec's core rule: a must constraint without
			// any reliable evidence must not reach the top results.
			if verdict.contradicted || verdict.observedMustNot || (verdict.totalMusts > 0 && verdict.confirmedMusts+verdict.possibleMusts != verdict.totalMusts) {
				continue
			}
			// Partial support downranks instead of pretending: a shot whose
			// musts are only half confirmed loses half the margin between
			// its score and the neutral line.
			if verdict.totalMusts > 0 {
				candidate.Score *= 0.5 + 0.5*float64(verdict.confirmedMusts)/float64(verdict.totalMusts)
			}
		}
		out = append(out, candidate)
	}
	return out, evidenceByID, nil
}
