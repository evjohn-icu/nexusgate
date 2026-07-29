package normalize

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ev/timingdex/internal/domain"
)

const (
	MaxSummaryLength = 2000
	MaxTagLength     = 80
	MaxTagsPerList   = 64
)

var allowedAssetType = makeSet("b_roll", "talking_to_camera", "conversation", "activity", "performance", "food", "transport", "architecture", "landscape", "animal", "document", "screen_recording", "accidental", "other")
var allowedMotion = makeSet("static", "pan_left", "pan_right", "tilt_up", "tilt_down", "forward", "backward", "tracking", "orbit", "handheld", "mixed", "unknown")
var allowedShot = makeSet("extreme_wide", "wide", "medium", "close_up", "extreme_close_up", "mixed", "unknown")
var allowedAudio = makeSet("silence", "ambient", "speech", "music", "singing", "speech_and_music", "noise", "mixed", "unknown")
var allowedQuality = makeSet("excellent", "usable", "limited", "not_recommended", "unknown")
var allowedUse = makeSet("hook", "opening", "establishing", "transition", "montage", "narration_support", "character_intro", "activity_detail", "emotional_pause", "behind_the_scenes", "ending", "not_recommended")

func ValidateAndNormalize(a domain.StructuredAnalysis) (domain.StructuredAnalysis, error) {
	a.AssetType = normEnum(a.AssetType, allowedAssetType, "other")
	a.CameraMotion = normEnum(a.CameraMotion, allowedMotion, "unknown")
	a.ShotSize = normEnum(a.ShotSize, allowedShot, "unknown")
	a.AudioType = normEnum(a.AudioType, allowedAudio, "unknown")
	a.Quality = normEnum(a.Quality, allowedQuality, "unknown")
	a.UsableAs = normList(a.UsableAs, allowedUse)
	a.SceneTags = clean(a.SceneTags)
	a.Subjects = clean(a.Subjects)
	a.MoodTags = clean(a.MoodTags)
	a.QualityFlags = clean(a.QualityFlags)
	a.ExtraTags = clean(a.ExtraTags)
	a.Summary = truncateByRunes(strings.TrimSpace(a.Summary), MaxSummaryLength)
	a.EditorialReason = truncateByRunes(strings.TrimSpace(a.EditorialReason), MaxSummaryLength)
	if a.Summary == "" {
		return a, fmt.Errorf("summary is required")
	}
	if a.PeopleCount < 0 {
		a.PeopleCount = 0
	}
	return a, nil
}

func makeSet(v ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range v {
		m[x] = true
	}
	return m
}
func normEnum(v string, set map[string]bool, fallback string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if set[v] {
		return v
	}
	return fallback
}
func normList(v []string, set map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range v {
		if len(out) >= MaxTagsPerList {
			break
		}
		x = strings.ToLower(strings.TrimSpace(x))
		if set[x] && !seen[x] {
			out = append(out, x)
			seen[x] = true
		}
	}
	return out
}
func clean(v []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range v {
		if len(out) >= MaxTagsPerList {
			break
		}
		x = strings.TrimSpace(strings.ToLower(x))
		if utf8.RuneCountInString(x) > MaxTagLength {
			x = string([]rune(x)[:MaxTagLength])
		}
		if x != "" && !seen[x] {
			out = append(out, x)
			seen[x] = true
		}
	}
	return out
}
func truncateByRunes(s string, max int) string {
	if max < 0 {
		return ""
	}
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return s
}
