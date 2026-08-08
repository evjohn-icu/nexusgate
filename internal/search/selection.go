package search

// Selection separates "what footage is relevant" (retrieval) from "what
// footage fits together" (selection) — the layer editing agents like ChatCut
// depend on. This round it is a light greedy pass over the fused pool:
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

// Selection runs the diversity pass over fused candidates.
type Selection struct {
	opts SelectionOptions
}

func NewSelection(opts SelectionOptions) *Selection {
	return &Selection{opts: opts}
}

// Select returns at most limit candidates in fused order, re-ranked by the
// diversity pass. Diversity <= 0 disables selection entirely (fused order
// preserved) — the compatibility path and gate-only pipelines pass through.
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
		if len(selected) >= limit {
			break
		}
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
		if adjusted > 0 {
			candidate.Score = adjusted
		}
		selected = append(selected, candidate)
	}
	return selected
}
