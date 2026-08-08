package search

import (
	"github.com/evjohn-icu/timingdex/internal/domain"
)

// Retrieval channels. Each channel wraps one ShotStore method and exposes it
// as a CandidateRetriever; the service unions the channels' candidates and
// fuses their per-signal scores. Adding a future channel (text embedding,
// visual embedding, OCR) is a new file here plus a ShotStore method — nothing
// else in the engine changes.

// toCandidate converts a stored shot row into the engine's candidate shape.
// The shot's per-signal fields are mapped into Signals by the caller.
func toCandidate(shot domain.ShotSearchResult) Candidate {
	return Candidate{
		ShotID:      shot.ID,
		AssetID:     shot.AssetID,
		Filename:    shot.Filename,
		Ordinal:     shot.Ordinal,
		StartMS:     shot.StartMS,
		EndMS:       shot.EndMS,
		Description: shot.Description,
		Tags:        shot.Tags,
		Objects:     shot.Objects,
		Actions:     shot.Actions,
		Mood:        shot.Mood,
		Confidence:  shot.Confidence,
		Signals:     map[string]float64{},
	}
}

// sortCandidates orders fused candidates deterministically: score descending,
// then ShotID ascending. Every fusion strategy ends with this order so the
// pipeline and its tests never see a heap-arbitrary tie break.
func sortCandidates(candidates []Candidate) {
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && worse(candidates[j-1], candidates[j]); j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}
}

// worse reports whether a ranks below b (used by the insertion sort above).
func worse(a, b Candidate) bool {
	if a.Score == b.Score {
		return a.ShotID > b.ShotID
	}
	return a.Score < b.Score
}
