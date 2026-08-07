package video_analysis

import (
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

func TestResultToStructuredAnalysisPreservesUnifiedVideoSemantics(t *testing.T) {
	result := Result{
		Summary: "夜晚城市街道，人群经过商业街",
		Scenes:  []Scene{{Description: "城市夜景", Tags: []string{"urban_night"}}},
		Objects: []Object{{Name: "person", Count: 3}},
		Actions: []Action{{Name: "walking"}},
		Mood:    []string{"busy", "modern"},
		RawTags: []string{"night city", "commercial street"},
		Analysis: domain.StructuredAnalysis{
			AssetType:    "b_roll",
			Quality:      "usable",
			CameraMotion: "handheld",
		},
	}

	got := result.ToStructuredAnalysis()
	if got.Summary != result.Summary || got.AssetType != "b_roll" || got.Quality != "usable" {
		t.Fatalf("legacy fields were not preserved: %+v", got)
	}
	if !contains(got.SceneTags, "urban_night") || !contains(got.SceneTags, "night city") {
		t.Fatalf("scene tags did not merge unified tags: %#v", got.SceneTags)
	}
	if !contains(got.Subjects, "person") || !contains(got.ExtraTags, "walking") || !contains(got.MoodTags, "busy") {
		t.Fatalf("semantic fields were not mapped: %+v", got)
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// TestToAssetShotsNoWholeAssetFallback pins the shot-truth rule: a shot that
// did not observe an object/action/mood keeps empty lists even when the
// asset-global result mentions them. The old behaviour copied the whole-asset
// lists into every shot missing its own, so a car that only appears at 08:20
// of a ten-minute asset polluted every shot's FTS row and semantic vector.
func TestToAssetShotsNoWholeAssetFallback(t *testing.T) {
	result := Result{
		Objects: []Object{{Name: "car"}, {Name: "person"}},
		Actions: []Action{{Name: "driving"}},
		Mood:    []string{"tense", "busy"},
		Shots: []Shot{
			{StartMS: 0, EndMS: 10_000, Description: "empty street at dawn"},
			{StartMS: 20_000, EndMS: 30_000, Description: "red car crossing", Objects: []string{"car"}},
		},
	}
	shots := result.ToAssetShots("asset-1", "run-1")
	if len(shots) != 2 {
		t.Fatalf("expected 2 shots, got %d", len(shots))
	}
	if len(shots[0].Objects) != 0 || len(shots[0].Actions) != 0 || len(shots[0].Mood) != 0 {
		t.Fatalf("shot without observations inherited asset-global metadata: objects=%v actions=%v mood=%v",
			shots[0].Objects, shots[0].Actions, shots[0].Mood)
	}
	if len(shots[1].Objects) != 1 || shots[1].Objects[0] != "car" {
		t.Fatalf("shot with own observations was not preserved: %v", shots[1].Objects)
	}
	if shots[0].StartMS != 0 || shots[0].EndMS != 10_000 || shots[0].Description != "empty street at dawn" {
		t.Fatalf("shot timeline fields mangled: %+v", shots[0])
	}
	// The asset-global lists must still exist at asset level, where they
	// describe the whole file rather than masquerading as shot evidence.
	asset := result.ToStructuredAnalysis()
	if !contains(asset.Subjects, "car") || !contains(asset.ExtraTags, "driving") || !contains(asset.MoodTags, "tense") {
		t.Fatalf("asset-global metadata lost: %+v", asset)
	}
}
