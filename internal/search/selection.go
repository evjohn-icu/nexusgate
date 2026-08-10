package search

import "sort"

// Selection separates "what footage is relevant" (retrieval) from "what
// footage fits together" (selection) — the layer editing agents like ChatCut
// depend on. This round it is a greedy, MMR-like pass over the fused pool:
//
//   - near-time duplicates (same asset, start within NearTimeWindowMS of an
//     already-selected shot) are SKIPPED outright when diversity is on — a
//     burst of A001 10.1s/10.9s/11.5s/12.1s shots must not occupy the top-10;
//   - same-asset but far shots and same-session shots take a soft penalty
//     scaled by the pool's top score and the diversity setting;
//   - creative intent raises the default diversity, spreading results across
//     assets, sessions and shot ranges.
//
// Penalties are relative to the pool's max fused score so the same tuning
// works for RRF's tiny absolute scores and a weighted blend's larger ones.
// Order of processing is the input (fused) order, but the output is re-ranked
// by adjusted score, so a penalty demotes a candidate below later, better
// surviving ones — selection genuinely changes the order, it does not just
// truncate it.

// Selection runs the diversity pass over fused candidates.
type Selection struct {
	opts SelectionOptions
}

func NewSelection(opts SelectionOptions) *Selection {
	return &Selection{opts: opts}
}

// Select returns at most limit candidates re-ranked by the diversity pass.
// Diversity <= 0 disables selection entirely (fused order preserved) — the
// compatibility path and gate-only pipelines pass through.
//
// The greedy invariant: candidates are considered in input order, each
// candidate's adjusted score is its fused score minus the penalties accrued
// against the ALREADY-SELECTED shots (near-time duplicates are hard-skipped,
// contributing nothing), and the candidate is inserted into the selected list
// at the position its adjusted score earns. The list therefore stays sorted
// by adjusted score descending, with input order preserved among ties — the
// result is deterministic, and later candidates can rank above earlier ones
// once penalties are applied.
func (s *Selection) Select(candidates []Candidate, limit int) []Candidate {
	if limit <= 0 {
		return []Candidate{}
	}
	if s.opts.Diversity <= 0 {
		if len(candidates) > limit {
			return candidates[:limit]
		}
		return candidates
	}
	poolMax := 0.0
	for _, c := range candidates {
		if c.Score > poolMax {
			poolMax = c.Score
		}
	}
	if poolMax <= 0 {
		return []Candidate{}
	}
	scale := s.opts.Diversity / 0.2 * poolMax
	selected := make([]Candidate, 0, min(limit, len(candidates)))
	for _, candidate := range candidates {
		// No break at len(selected)==limit: a later candidate whose penalty
		// stays low can still outrank an earlier one that was heavily
		// penalized (the same-asset/same-session demotion this pass exists
		// for). The list is capped below by dropping the tail after the
		// insert, so selection size stays bounded while the scan keeps
		// seeing everything.
		penalty := 0.0
		skipped := false
		for _, sel := range selected {
			if sel.AssetID == candidate.AssetID {
				delta := candidate.StartMS - sel.StartMS
				if delta < 0 {
					delta = -delta
				}
				if delta <= s.opts.NearTimeWindowMS {
					// Near-duplicate burst: hard skip, not a downrank. The
					// whole point is that 10.1s/10.9s/11.5s/12.1s must not
					// crowd out other footage.
					skipped = true
					break
				}
				penalty += s.opts.SameAssetPenalty * scale
			} else if sel.SessionID != "" && sel.SessionID == candidate.SessionID {
				penalty += s.opts.SameSessionPenalty * scale
			}
		}
		if skipped {
			continue
		}
		adjusted := candidate.Score - penalty
		// Clamp at zero unconditionally: a candidate whose penalty exceeds
		// its score must rank at the floor, not keep the unpenalized fused
		// score that got it into the pool. The rank is the demotion.
		if adjusted < 0 {
			adjusted = 0
		}
		candidate.Score = adjusted
		// Insert at the first slot holding a strictly lower adjusted score,
		// so equal adjusted scores keep input order (stable) and the list
		// stays sorted descending.
		pos := sort.Search(len(selected), func(i int) bool {
			return selected[i].Score < candidate.Score
		})
		if pos >= limit {
			// The list is full and this candidate cannot beat the tail;
			// it would land exactly at the cut. Skip it without touching
			// the list.
			continue
		}
		selected = append(selected, Candidate{})
		copy(selected[pos+1:], selected[pos:])
		selected[pos] = candidate
		if len(selected) > limit {
			selected = selected[:limit]
		}
	}
	return selected
}
