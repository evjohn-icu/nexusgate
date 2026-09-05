package normalize

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

const (
	MaxSummaryLength = 2000
	MaxTagLength     = 80
	MaxTagsPerList   = 64
)

// The controlled vocabulary for the fields that are filtered on. These are
// exported so the prompt can state them: a model that is never shown the
// permitted values answers in its own words, every answer then fails the
// check below, and the field silently becomes "unknown" for the whole library.
// Prompt and validator must come from one list, or they drift apart again.
var (
	AssetTypeValues = []string{"b_roll", "talking_to_camera", "conversation", "activity", "performance", "food", "transport", "architecture", "landscape", "animal", "document", "screen_recording", "accidental", "other"}
	MotionValues    = []string{"static", "pan_left", "pan_right", "tilt_up", "tilt_down", "forward", "backward", "tracking", "orbit", "handheld", "mixed", "unknown"}
	ShotSizeValues  = []string{"extreme_wide", "wide", "medium", "close_up", "extreme_close_up", "mixed", "unknown"}
	AudioTypeValues = []string{"silence", "ambient", "speech", "music", "singing", "speech_and_music", "noise", "mixed", "unknown"}
	QualityValues   = []string{"excellent", "usable", "limited", "not_recommended", "unknown"}
	UsableAsValues  = []string{"hook", "opening", "establishing", "transition", "montage", "narration_support", "character_intro", "activity_detail", "emotional_pause", "behind_the_scenes", "ending", "not_recommended"}
)

var allowedAssetType = makeSet(AssetTypeValues...)
var allowedMotion = makeSet(MotionValues...)
var allowedShot = makeSet(ShotSizeValues...)
var allowedAudio = makeSet(AudioTypeValues...)
var allowedQuality = makeSet(QualityValues...)
var allowedUse = makeSet(UsableAsValues...)

// VocabularyPrompt states the permitted values for every constrained field, in
// the wording a provider prompt embeds. Both prompts call it so neither can
// fall behind the validator.
func VocabularyPrompt() string {
	var b strings.Builder
	b.WriteString("For these fields use exactly one of the listed values, spelled exactly as shown; if none applies use the last value. ")
	for _, field := range []struct {
		name   string
		values []string
	}{
		{"asset_type", AssetTypeValues},
		{"camera_motion", MotionValues},
		{"shot_size", ShotSizeValues},
		{"audio_type", AudioTypeValues},
		{"quality", QualityValues},
	} {
		b.WriteString(field.name)
		b.WriteString(": ")
		b.WriteString(strings.Join(field.values, ", "))
		b.WriteString(". ")
	}
	b.WriteString("usable_as is a list drawn from: ")
	b.WriteString(strings.Join(UsableAsValues, ", "))
	b.WriteString(". Do not invent values for these fields and do not add qualifiers to them; put any extra detail in summary or extra_tags instead. ")
	return b.String()
}

// ValidateAndNormalize is a pure function of the model's answer, so anything
// it rejects it would reject identically on every later attempt — and each of
// those attempts is a paid provider call for a reply already known to be
// unusable. The rejection therefore carries domain.ErrPermanentFailure out of
// this package, marked in this wrapper rather than at each return inside, so a
// check added to normalizeAnalysis is permanent as soon as it is written. This
// used to be a phrase ("summary is required") in a list in internal/app; see
// domain.ErrPermanentFailure for why the list could not stay right.
func ValidateAndNormalize(a domain.StructuredAnalysis) (domain.StructuredAnalysis, error) {
	a, err := normalizeAnalysis(a)
	if err != nil {
		return a, domain.Permanent(err)
	}
	return a, nil
}

func normalizeAnalysis(a domain.StructuredAnalysis) (domain.StructuredAnalysis, error) {
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

// NormalizeEnumValue coerces one value into a controlled vocabulary the same
// way normEnum does (case, trimming, separator collapsing, unknown fallback).
// ValidateAndNormalize only reaches asset-level fields; per-shot metadata
// (shot_size, camera_motion, quality on a multiframe shot) bypasses it, so
// the adapter that decodes them calls this with the matching exported list.
func NormalizeEnumValue(value string, allowed []string) string {
	if len(allowed) == 0 {
		return ""
	}
	set := makeSet(allowed...)
	fallback := allowed[len(allowed)-1]
	return normEnum(value, set, fallback)
}

func normEnum(v string, set map[string]bool, fallback string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if set[v] {
		return v
	}
	// "close-up" and "pan left" are the vocabulary's close_up and pan_left
	// written the way prose writes them. Discarding a correct answer over a
	// hyphen loses real information, so separators are collapsed before the
	// second attempt.
	//
	// Only spelling is forgiven, never meaning: an answer that is not one of
	// the permitted values still falls back rather than being guessed at from
	// the words it contains, because "no speech, ambient only" contains
	// "speech" and means the opposite.
	if collapsed := collapseSeparators(v); collapsed != v && set[collapsed] {
		return collapsed
	}
	return fallback
}

// collapseSeparators reduces every run of characters that is neither a letter
// nor a digit to a single underscore, which is how the vocabulary spells its
// multi-word values.
func collapseSeparators(v string) string {
	var b strings.Builder
	b.Grow(len(v))
	separator := false
	for _, ch := range v {
		switch {
		case (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9'):
			b.WriteRune(ch)
			separator = false
		case !separator:
			b.WriteByte('_')
			separator = true
		}
	}
	return strings.Trim(b.String(), "_")
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
