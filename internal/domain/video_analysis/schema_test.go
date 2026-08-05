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
