package search

import (
	"strings"
)

// RouteIntent classifies what kind of answer a query wants, without an LLM.
// The rules are deliberately transparent and deterministic so the benchmark
// can measure router agreement against hand-tagged intents:
//
//	speech:    quoted phrase or a speech marker (说过/说了/说) present
//	similar:   a shot id ("shot_...") in the text
//	negative:  MustNot constraints present -> fact (the gate enforces it)
//	semantic:  mood vocabulary without objects, or only scene/weather/time
//	creative:  creative-request phrases (给我找/找点/来几个/创意)
//	fact:      two or more object constraints, or anything else
func RouteIntent(q SearchQuery) SearchIntent {
	lower := strings.ToLower(q.Raw)
	if _, phrase := extractSpeechPhrase(q.Raw); phrase != "" {
		return IntentSpeech
	}
	if shotIDPattern.MatchString(q.Raw) {
		return IntentSimilar
	}
	if len(q.MustNot) > 0 {
		return IntentFact
	}
	if isCreativePhrase(lower) {
		return IntentCreative
	}
	hasObject := false
	hasMood := false
	hasSceneOrTime := false
	for _, c := range q.Must {
		switch c.Type {
		case ConstraintObject:
			hasObject = true
		case ConstraintMood:
			hasMood = true
		case ConstraintScene, ConstraintTime, ConstraintWeather:
			hasSceneOrTime = true
		}
	}
	for _, c := range q.Should {
		switch c.Type {
		case ConstraintObject:
			hasObject = true
		case ConstraintMood:
			hasMood = true
		case ConstraintScene, ConstraintTime, ConstraintWeather:
			hasSceneOrTime = true
		}
	}
	if hasMood && !hasObject {
		return IntentSemantic
	}
	if hasSceneOrTime && !hasObject {
		return IntentSemantic
	}
	if objectCount(q.Must) >= 2 {
		return IntentFact
	}
	return IntentFact
}

var creativePhrases = []string{"给我找", "帮我找", "找点", "找几个", "来几个", "来点", "创意", "creative"}

func isCreativePhrase(lower string) bool {
	// This is intent classification only. Contains is useful for recognizing a
	// request embedded in natural language, but it must not be reused as speech
	// evidence; speech claims are validated exactly by the evidence gate.
	for _, phrase := range creativePhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func objectCount(constraints []Constraint) int {
	count := 0
	for _, c := range constraints {
		if c.Type == ConstraintObject {
			count++
		}
	}
	return count
}

// normalizeMode maps an API mode string to an intent. The empty string and
// "auto" mean "route it". An unknown mode is an error for the caller.
func normalizeMode(mode string) (SearchIntent, bool) {
	switch mode {
	case "", "auto":
		return IntentAuto, true
	case "fact":
		return IntentFact, true
	case "speech":
		return IntentSpeech, true
	case "semantic":
		return IntentSemantic, true
	case "similar":
		return IntentSimilar, true
	case "creative":
		return IntentCreative, true
	}
	return IntentAuto, false
}

// ValidMode reports whether mode is a recognized API mode value (including
// "" and "auto"). Exported so the API layer can 400 an unknown mode before
// the engine runs.
func ValidMode(mode string) bool {
	_, ok := normalizeMode(mode)
	return ok
}
