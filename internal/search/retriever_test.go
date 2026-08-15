package search

import "testing"

func TestSortCandidatesDeterministicAndStable(t *testing.T) {
	candidates := []Candidate{
		{ShotID: "z", Ordinal: 1, Score: 0.4},
		{ShotID: "same", Ordinal: 7, Score: 0.8},
		{ShotID: "b", Ordinal: 2, Score: 0.8},
		{ShotID: "a", Ordinal: 3, Score: 0.8},
		{ShotID: "same", Ordinal: 9, Score: 0.8},
	}

	sortCandidates(candidates)
	wantIDs := []string{"a", "b", "same", "same", "z"}
	for i, want := range wantIDs {
		if candidates[i].ShotID != want {
			t.Fatalf("candidate %d ID = %q, want %q", i, candidates[i].ShotID, want)
		}
	}
	if candidates[2].Ordinal != 7 || candidates[3].Ordinal != 9 {
		t.Fatalf("equal candidates lost stable order: ordinals %d, %d", candidates[2].Ordinal, candidates[3].Ordinal)
	}
}
