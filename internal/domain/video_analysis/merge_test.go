package video_analysis

import (
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// The model is shown one window at a time and answers in window-relative time.
// Forgetting to shift is the failure that would place every shot of a long
// asset in its first few minutes, silently and without any error.
func TestMergeWindowResultsShiftsShotsOntoTheAssetTimeline(t *testing.T) {
	merged := MergeWindowResults([]WindowResult{
		{StartMS: 0, EndMS: 300_000, Result: Result{Shots: []Shot{{StartMS: 10_000, EndMS: 20_000, Description: "harbour at dusk"}}}},
		{StartMS: 300_000, EndMS: 600_000, Result: Result{Shots: []Shot{{StartMS: 10_000, EndMS: 20_000, Description: "market street"}}}},
	})
	if len(merged.Shots) != 2 {
		t.Fatalf("expected 2 shots, got %d: %+v", len(merged.Shots), merged.Shots)
	}
	var market Shot
	for _, s := range merged.Shots {
		if s.Description == "market street" {
			market = s
		}
	}
	if market.StartMS != 310_000 || market.EndMS != 320_000 {
		t.Fatalf("second window's shot was not shifted: %+v", market)
	}
}

// Windows overlap on purpose so an event on a cut survives whole in one of
// them. Without de-duplication that same overlap would double every event
// inside it.
func TestMergeWindowResultsDeduplicatesAcrossTheOverlap(t *testing.T) {
	merged := MergeWindowResults([]WindowResult{
		{StartMS: 0, EndMS: 300_000, Result: Result{Shots: []Shot{
			{StartMS: 290_000, EndMS: 300_000, Description: "drone lifts over the ridge", Tags: []string{"drone"}},
		}}},
		{StartMS: 295_000, EndMS: 600_000, Result: Result{Shots: []Shot{
			{StartMS: 0, EndMS: 12_000, Description: "Drone lifts over the ridge", Tags: []string{"aerial"}},
		}}},
	})
	if len(merged.Shots) != 1 {
		t.Fatalf("the same observation from two windows must collapse to one, got %d: %+v", len(merged.Shots), merged.Shots)
	}
	shot := merged.Shots[0]
	// The wider sighting wins, and tags from both are kept.
	if shot.StartMS != 295_000 || shot.EndMS != 307_000 {
		t.Fatalf("expected the wider of the two sightings, got %d-%d", shot.StartMS, shot.EndMS)
	}
	if len(shot.Tags) != 2 {
		t.Fatalf("tags from both windows must be kept, got %v", shot.Tags)
	}
}

// Two genuinely different shots that happen to overlap in time must survive, or
// a long take gets flattened into one entry.
func TestMergeWindowResultsKeepsDistinctOverlappingShots(t *testing.T) {
	merged := MergeWindowResults([]WindowResult{
		{StartMS: 0, EndMS: 300_000, Result: Result{Shots: []Shot{
			{StartMS: 290_000, EndMS: 300_000, Description: "wide of the valley"},
		}}},
		{StartMS: 295_000, EndMS: 600_000, Result: Result{Shots: []Shot{
			{StartMS: 0, EndMS: 10_000, Description: "close up on a cyclist"},
		}}},
	})
	if len(merged.Shots) != 2 {
		t.Fatalf("different descriptions must not be merged, got %d: %+v", len(merged.Shots), merged.Shots)
	}
}

// A window's answer must never describe footage outside that window; times past
// the end would otherwise overrun the asset duration and fail shot validation
// downstream.
func TestMergeWindowResultsClampsTimesToTheWindow(t *testing.T) {
	merged := MergeWindowResults([]WindowResult{
		{StartMS: 600_000, EndMS: 900_000, Result: Result{Shots: []Shot{
			{StartMS: 250_000, EndMS: 999_000, Description: "overruns the window"},
			{StartMS: -5_000, EndMS: 4_000, Description: "starts before the window"},
		}}},
	})
	for _, shot := range merged.Shots {
		if shot.StartMS < 600_000 || shot.EndMS > 900_000 {
			t.Fatalf("shot escaped its window: %+v", shot)
		}
	}
}

func TestMergeWindowResultsUnionsTagsAndTakesTheBusiestPeopleCount(t *testing.T) {
	merged := MergeWindowResults([]WindowResult{
		{StartMS: 0, EndMS: 300_000, Result: Result{
			Summary:  "Empty harbour.",
			Mood:     []string{"calm"},
			Analysis: domain.StructuredAnalysis{SceneTags: []string{"harbour"}, PeopleCount: 0, CameraMotion: "static", HasSpeech: false},
		}},
		{StartMS: 300_000, EndMS: 600_000, Result: Result{
			Summary:  "A crowd arrives.",
			Mood:     []string{"busy"},
			Analysis: domain.StructuredAnalysis{SceneTags: []string{"crowd"}, PeopleCount: 40, CameraMotion: "pan", HasSpeech: true},
		}},
	})
	if merged.Analysis.PeopleCount != 40 {
		t.Errorf("people count must reflect the busiest window, got %d", merged.Analysis.PeopleCount)
	}
	if !merged.Analysis.HasSpeech {
		t.Error("speech in any window means the asset has speech")
	}
	if len(merged.Analysis.SceneTags) != 2 {
		t.Errorf("scene tags must be unioned, got %v", merged.Analysis.SceneTags)
	}
	if merged.Analysis.CameraMotion != "static" {
		t.Errorf("single-valued fields take the first answer, got %q", merged.Analysis.CameraMotion)
	}
	// Summary is single-valued like CameraMotion above: the first window's
	// answer wins rather than every window's restatement being joined into
	// one wall of near-duplicate sentences.
	if merged.Summary != "Empty harbour." {
		t.Errorf("summary = %q, want first window's summary only", merged.Summary)
	}
	if len(merged.Mood) != 2 {
		t.Errorf("mood must be unioned, got %v", merged.Mood)
	}
}

// The single-window case is the common one and must not be disturbed by the
// merge path — but it still needs shifting, since a window may start at 0 yet
// report times the model invented past the end.
func TestMergeWindowResultsPassesASingleWindowThrough(t *testing.T) {
	original := Result{
		Summary:  "One clip.",
		Shots:    []Shot{{StartMS: 1_000, EndMS: 5_000, Description: "a shot"}},
		Analysis: domain.StructuredAnalysis{AssetType: "b-roll"},
	}
	merged := MergeWindowResults([]WindowResult{{StartMS: 0, EndMS: 60_000, Result: original}})
	if merged.Summary != "One clip." || merged.Analysis.AssetType != "b-roll" {
		t.Fatalf("single window was altered: %+v", merged)
	}
	if len(merged.Shots) != 1 || merged.Shots[0].StartMS != 1_000 || merged.Shots[0].EndMS != 5_000 {
		t.Fatalf("single-window shots changed: %+v", merged.Shots)
	}
}

// When two adjacent windows report the same observation with different
// confidence values, the merge must keep the higher one — the same way
// mergeObjects does. The wider time range is already kept; confidence was
// the one field that was silently dropped.
func TestMergeWindowResultsPreservesHighestConfidenceOnDedupe(t *testing.T) {
	merged := MergeWindowResults([]WindowResult{
		{StartMS: 0, EndMS: 300_000, Result: Result{Shots: []Shot{
			{StartMS: 290_000, EndMS: 300_000, Confidence: 0.6, Description: "drone shot"},
		}}},
		{StartMS: 295_000, EndMS: 600_000, Result: Result{Shots: []Shot{
			{StartMS: 0, EndMS: 12_000, Confidence: 0.9, Description: "drone shot"},
		}}},
	})
	if len(merged.Shots) != 1 {
		t.Fatalf("expected 1 deduplicated shot, got %d", len(merged.Shots))
	}
	if merged.Shots[0].Confidence != 0.9 {
		t.Fatalf("confidence should be max of the two (0.9), got %v", merged.Shots[0].Confidence)
	}
}

func TestMergeWindowResultsHandlesNoWindows(t *testing.T) {
	if got := MergeWindowResults(nil); got.Summary != "" || len(got.Shots) != 0 {
		t.Fatalf("empty input must produce an empty result, got %+v", got)
	}
}

// Three windows, each with its own distinct summary (the common shape for a
// long asset that was split three or more ways): only the first window's
// account survives. Joining all three would read as three restatements of
// "this video", not one description of the asset.
func TestMergeWindowResultsSummaryTakesOnlyTheFirstWindowAcrossThreeWindows(t *testing.T) {
	merged := MergeWindowResults([]WindowResult{
		{StartMS: 0, EndMS: 300_000, Result: Result{Summary: "First-person cycling through an alley."}},
		{StartMS: 300_000, EndMS: 600_000, Result: Result{Summary: "First-person cycling past a KFC."}},
		{StartMS: 600_000, EndMS: 900_000, Result: Result{Summary: "First-person cycling on a bike lane."}},
	})
	if merged.Summary != "First-person cycling through an alley." {
		t.Errorf("summary = %q, want only the first window's summary", merged.Summary)
	}
}

// A window that answered with everything else but an empty summary must not
// blank the asset's summary — the next window with a real answer wins, the
// same way firstNonEmpty behaves for every other single-valued field.
func TestMergeWindowResultsSummarySkipsAnEmptyFirstWindow(t *testing.T) {
	merged := MergeWindowResults([]WindowResult{
		{StartMS: 0, EndMS: 300_000, Result: Result{Shots: []Shot{{StartMS: 1_000, EndMS: 2_000, Description: "x"}}}},
		{StartMS: 300_000, EndMS: 600_000, Result: Result{Summary: "A crowd arrives."}},
	})
	if merged.Summary != "A crowd arrives." {
		t.Errorf("summary = %q, want the first window that actually answered", merged.Summary)
	}
}

// A response can carry its summary in the nested legacy Analysis field
// instead of the top-level one (see multiframe.Provider.Analyze, which
// accepts either) — the merge must not treat that window as having no
// summary just because the field it checked first was empty.
func TestMergeWindowResultsSummaryReadsTheNestedAnalysisFieldToo(t *testing.T) {
	merged := MergeWindowResults([]WindowResult{
		{StartMS: 0, EndMS: 300_000, Result: Result{Analysis: domain.StructuredAnalysis{Summary: "Nested-field summary."}}},
		{StartMS: 300_000, EndMS: 600_000, Result: Result{Summary: "Top-level summary."}},
	})
	if merged.Summary != "Nested-field summary." {
		t.Errorf("summary = %q, want the first window's nested Analysis.Summary", merged.Summary)
	}
}
