package video_analysis

import (
	"sort"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// WindowResult is one model response together with the slice of the asset it
// describes. The model only ever sees the window, so every time it reports is
// relative to StartMS and means nothing until it is shifted onto the asset
// timeline.
type WindowResult struct {
	StartMS int64
	EndMS   int64
	Result  Result
}

// shotOverlapNumerator/Denominator express the fraction of overlap at which two
// shots from adjacent windows are taken to be the same observation seen twice.
// Windows overlap deliberately so an event on a cut survives in one of them;
// without this the overlap would instead duplicate every event inside it.
const (
	shotOverlapNumerator   = 1
	shotOverlapDenominator = 2
)

// MergeWindowResults folds per-window analyses into the single asset-level
// result the rest of the pipeline expects.
//
// Shots are shifted onto the asset timeline and de-duplicated across the
// overlap. Asset-level list fields are unioned, because a tag seen in any
// window is true of the asset. Single-valued fields take the first window that
// answered: they describe "the shot" of an asset that, once it is long enough
// to need splitting, no longer has just one — so any choice is a simplification
// and the earliest is at least deterministic and cheap to explain.
//
// Summary follows that same first-window rule, not a join. Each window is
// shown only its own slice of the asset but is asked to summarise "the
// video", so every window answers as if it were the whole clip — joining
// those answers reads as the same claim restated once per window, not one
// description of the asset, and a long asset can restate it enough times to
// blow past the persisted length bound before reaching the end. For a split
// asset the merged Summary describes the opening window; the deduplicated
// Shots below carry what the later windows saw.
func MergeWindowResults(windows []WindowResult) Result {
	if len(windows) == 0 {
		return Result{}
	}
	if len(windows) == 1 {
		merged := windows[0].Result
		merged.Shots = shiftShots(merged.Shots, windows[0].StartMS, windows[0].EndMS)
		merged.Scenes = shiftScenes(merged.Scenes, windows[0].StartMS, windows[0].EndMS)
		return merged
	}

	merged := Result{}
	var confidenceTotal float64
	var confidenceCount int

	for _, window := range windows {
		r := window.Result
		// A model response may carry its summary in the top-level field or
		// the nested legacy Analysis field depending on which shape it
		// answered in (see multiframe.Provider.Analyze, which accepts
		// either) — check both before moving to the next window.
		windowSummary := firstNonEmpty(strings.TrimSpace(r.Summary), strings.TrimSpace(r.Analysis.Summary))
		merged.Summary = firstNonEmpty(merged.Summary, windowSummary)
		merged.Mood = appendUnique(merged.Mood, r.Mood...)
		merged.RawTags = appendUnique(merged.RawTags, r.RawTags...)
		merged.Objects = mergeObjects(merged.Objects, r.Objects)
		merged.Actions = mergeActions(merged.Actions, r.Actions)
		merged.Shots = append(merged.Shots, shiftShots(r.Shots, window.StartMS, window.EndMS)...)
		merged.Scenes = append(merged.Scenes, shiftScenes(r.Scenes, window.StartMS, window.EndMS)...)
		merged.Analysis = mergeStructured(merged.Analysis, r.Analysis)
		if r.Confidence > 0 {
			confidenceTotal += r.Confidence
			confidenceCount++
		}
	}

	if confidenceCount > 0 {
		merged.Confidence = confidenceTotal / float64(confidenceCount)
	}
	merged.Shots = dedupeShots(merged.Shots)
	merged.Scenes = dedupeScenes(merged.Scenes)
	if merged.Analysis.Summary == "" {
		merged.Analysis.Summary = merged.Summary
	}
	return merged
}

// shiftShots moves window-relative times onto the asset timeline and clamps to
// the window. A model asked about a 5-minute window sometimes answers with
// times past its end; left alone those would either overrun the asset duration
// and fail validation, or land on footage they do not describe.
func shiftShots(shots []Shot, startMS, endMS int64) []Shot {
	if len(shots) == 0 {
		return nil
	}
	shifted := make([]Shot, 0, len(shots))
	for _, shot := range shots {
		shot.StartMS, shot.EndMS = clampToWindow(shot.StartMS, shot.EndMS, startMS, endMS)
		if shot.EndMS <= shot.StartMS {
			continue
		}
		shifted = append(shifted, shot)
	}
	return shifted
}

func shiftScenes(scenes []Scene, startMS, endMS int64) []Scene {
	if len(scenes) == 0 {
		return nil
	}
	shifted := make([]Scene, 0, len(scenes))
	for _, scene := range scenes {
		scene.StartMS, scene.EndMS = clampToWindow(scene.StartMS, scene.EndMS, startMS, endMS)
		if scene.EndMS <= scene.StartMS {
			continue
		}
		shifted = append(shifted, scene)
	}
	return shifted
}

func clampToWindow(start, end, windowStart, windowEnd int64) (int64, int64) {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	start += windowStart
	end += windowStart
	if start > windowEnd {
		start = windowEnd
	}
	if end > windowEnd {
		end = windowEnd
	}
	return start, end
}

// dedupeShots collapses the same observation reported by two adjacent windows.
// Two shots match when they overlap by at least half of the shorter one and
// carry the same description; an overlap alone is not enough, because a long
// take legitimately contains several distinct shots at the same instant.
func dedupeShots(shots []Shot) []Shot {
	if len(shots) < 2 {
		return shots
	}
	sort.SliceStable(shots, func(i, j int) bool {
		if shots[i].StartMS != shots[j].StartMS {
			return shots[i].StartMS < shots[j].StartMS
		}
		return shots[i].EndMS < shots[j].EndMS
	})
	kept := make([]Shot, 0, len(shots))
	for _, shot := range shots {
		duplicate := false
		for i := range kept {
			if sameObservation(kept[i].StartMS, kept[i].EndMS, shot.StartMS, shot.EndMS) &&
				normalizedText(kept[i].Description) == normalizedText(shot.Description) {
				// Keep the wider of the two: the window that saw more of the
				// event described it from more evidence.
				if shot.EndMS-shot.StartMS > kept[i].EndMS-kept[i].StartMS {
					kept[i].StartMS, kept[i].EndMS = shot.StartMS, shot.EndMS
				}
				kept[i].Tags = appendUnique(kept[i].Tags, shot.Tags...)
				kept[i].Objects = appendUnique(kept[i].Objects, shot.Objects...)
				kept[i].Actions = appendUnique(kept[i].Actions, shot.Actions...)
				kept[i].Mood = appendUnique(kept[i].Mood, shot.Mood...)
				if shot.Confidence > kept[i].Confidence {
					kept[i].Confidence = shot.Confidence
				}
				duplicate = true
				break
			}
		}
		if !duplicate {
			kept = append(kept, shot)
		}
	}
	return kept
}

func dedupeScenes(scenes []Scene) []Scene {
	if len(scenes) < 2 {
		return scenes
	}
	sort.SliceStable(scenes, func(i, j int) bool { return scenes[i].StartMS < scenes[j].StartMS })
	kept := make([]Scene, 0, len(scenes))
	for _, scene := range scenes {
		duplicate := false
		for i := range kept {
			if sameObservation(kept[i].StartMS, kept[i].EndMS, scene.StartMS, scene.EndMS) &&
				normalizedText(kept[i].Description) == normalizedText(scene.Description) {
				kept[i].Tags = appendUnique(kept[i].Tags, scene.Tags...)
				duplicate = true
				break
			}
		}
		if !duplicate {
			kept = append(kept, scene)
		}
	}
	return kept
}

func sameObservation(aStart, aEnd, bStart, bEnd int64) bool {
	overlap := min64(aEnd, bEnd) - max64(aStart, bStart)
	if overlap <= 0 {
		return false
	}
	shorter := min64(aEnd-aStart, bEnd-bStart)
	if shorter <= 0 {
		return false
	}
	return overlap*shotOverlapDenominator >= shorter*shotOverlapNumerator
}

func normalizedText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

// mergeStructured unions the list fields and takes the first answer for the
// single-valued ones. PeopleCount takes the maximum rather than the first,
// because "how many people are in this asset" is answered by the busiest
// moment, not by whichever window happened to come back first.
func mergeStructured(into, from domain.StructuredAnalysis) domain.StructuredAnalysis {
	into.SceneTags = appendUnique(into.SceneTags, from.SceneTags...)
	into.Subjects = appendUnique(into.Subjects, from.Subjects...)
	into.UsableAs = appendUnique(into.UsableAs, from.UsableAs...)
	into.MoodTags = appendUnique(into.MoodTags, from.MoodTags...)
	into.QualityFlags = appendUnique(into.QualityFlags, from.QualityFlags...)
	into.ExtraTags = appendUnique(into.ExtraTags, from.ExtraTags...)
	into.HasSpeech = into.HasSpeech || from.HasSpeech
	if from.PeopleCount > into.PeopleCount {
		into.PeopleCount = from.PeopleCount
	}
	into.AssetType = firstNonEmpty(into.AssetType, from.AssetType)
	into.ShotSize = firstNonEmpty(into.ShotSize, from.ShotSize)
	into.CameraMotion = firstNonEmpty(into.CameraMotion, from.CameraMotion)
	into.Lighting = firstNonEmpty(into.Lighting, from.Lighting)
	into.AudioType = firstNonEmpty(into.AudioType, from.AudioType)
	into.Quality = firstNonEmpty(into.Quality, from.Quality)
	into.EditorialReason = firstNonEmpty(into.EditorialReason, from.EditorialReason)
	return into
}

func mergeObjects(into, from []Object) []Object {
	for _, object := range from {
		found := false
		for i := range into {
			if normalizedText(into[i].Name) == normalizedText(object.Name) {
				if object.Count > into[i].Count {
					into[i].Count = object.Count
				}
				if object.Confidence > into[i].Confidence {
					into[i].Confidence = object.Confidence
				}
				found = true
				break
			}
		}
		if !found && strings.TrimSpace(object.Name) != "" {
			into = append(into, object)
		}
	}
	return into
}

func mergeActions(into, from []Action) []Action {
	for _, action := range from {
		found := false
		for i := range into {
			if normalizedText(into[i].Name) == normalizedText(action.Name) {
				if action.Confidence > into[i].Confidence {
					into[i].Confidence = action.Confidence
				}
				found = true
				break
			}
		}
		if !found && strings.TrimSpace(action.Name) != "" {
			into = append(into, action)
		}
	}
	return into
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
