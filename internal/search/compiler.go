package search

import (
	"regexp"
	"strings"
)

// Compile turns a raw search string into a structured SearchQuery entirely
// offline: normalization, the controlled vocabulary, positional negation and
// phrase rules. There is no LLM and no I/O; the same compiler runs in tests,
// in the Hub and in the benchmark. It is not expected to parse every natural
// sentence correctly — the structure it produces (intent, must/should/mustNot
// constraints) is the contract the router, channels, gate and evidence all
// consume.
//
// The rule set:
//   - object constraints land in Must; action/scene/weather/time/mood land in
//     Should; "夜晚下雨，有人撑伞走过街道" therefore compiles to
//     must:[person, umbrella], should:[walking, street, rain, night].
//   - a query with only Should constraints promotes them to Must, so a bare
//     "rain" is a requirement, not a preference.
//   - negation words (没有/无人/no/without/empty/...) negate the constraint
//     whose surface word sits in their window; negated constraints move to
//     MustNot. Positional, so "没有人的海边空镜" negates person only.
//   - quoted speech and speech markers (说过/说了/说) become a single
//     ConstraintSpeech holding the phrase.
//   - a shot id ("shot_...") becomes a metadata constraint and the similar
//     intent.

var shotIDPattern = regexp.MustCompile(`(?i)shot_[a-z0-9]{6,}`)

// speechMarkers are the words that introduce a speech claim. The marker's
// trailing text is the claimed phrase.
var speechMarkers = []string{"说过", "说了", "说道", "提到", "说"}

// Compile parses raw into a SearchQuery. A blank raw string yields the zero
// query (empty Must/Should/MustNot, IntentAuto).
func Compile(raw string) SearchQuery {
	raw = strings.TrimSpace(raw)
	q := SearchQuery{Raw: raw, Intent: IntentAuto}
	if raw == "" {
		return q
	}

	var speechPhrase string
	raw, speechPhrase = extractSpeechPhrase(raw)
	q.SpeechPhrase = speechPhrase

	matches := Canonicalize(raw)
	spans := NegationSpans(raw)
	var must, should, mustNot []Constraint
	for _, m := range matches {
		constraint := Constraint{Type: m.Type, Value: m.Canonical}
		negated := false
		for _, span := range spans {
			// Only DIRECT negation markers (没有/无人/no/without) negate in a
			// query. Absence descriptors (empty/abandoned/空) describe a
			// scene — "empty counter" asks for an empty counter, it does not
			// forbid counters.
			if span.Class == negationDirect && Negates(span, m.Type, m.Position) {
				negated = true
				break
			}
		}
		switch {
		case negated:
			mustNot = append(mustNot, constraint)
		case constraint.Type == ConstraintObject:
			must = append(must, constraint)
		default:
			should = append(should, constraint)
		}
	}

	// A bare speech claim is the query's core: it must gate the result, so it
	// goes to Must.
	if speechPhrase != "" {
		must = append(must, Constraint{Type: ConstraintSpeech, Value: speechPhrase})
	}

	if id := shotIDPattern.FindString(raw); id != "" {
		must = append(must, Constraint{Type: ConstraintMetadata, Value: strings.ToLower(id)})
	}

	// Should-only queries are really single-term requirements: promote, or
	// the fact gate would have nothing confirmed to hold them to.
	if len(must) == 0 && len(should) > 0 {
		must = should
		should = nil
	}

	q.Must = sortConstraints(must)
	q.Should = sortConstraints(should)
	q.MustNot = sortConstraints(mustNot)
	q.Intent = RouteIntent(q)
	return q
}

// constraintTypeRank orders constraints for output: objects first (they are
// the fact core), then action/scene/weather/time/mood by the spec's example
// ordering, then the rarer types. Equal ranks keep position order.
func constraintTypeRank(typ ConstraintType) int {
	switch typ {
	case ConstraintObject:
		return 0
	case ConstraintAction:
		return 1
	case ConstraintScene:
		return 2
	case ConstraintWeather:
		return 3
	case ConstraintTime:
		return 4
	case ConstraintMood:
		return 5
	case ConstraintSpeech:
		return 6
	case ConstraintCamera:
		return 7
	case ConstraintShotSize:
		return 8
	case ConstraintTag:
		return 9
	case ConstraintMetadata:
		return 10
	}
	return 11
}

func sortConstraints(constraints []Constraint) []Constraint {
	out := make([]Constraint, len(constraints))
	copy(out, constraints)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && constraintTypeRank(out[j].Type) < constraintTypeRank(out[j-1].Type); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// extractSpeechPhrase pulls a quoted or marker-introduced phrase out of the
// raw text. Quoted forms win (「」/“”/""/”); otherwise the text after the
// last speech marker (说过/说了/说/...) is the phrase. Returns the cleaned
// text and the phrase.
func extractSpeechPhrase(raw string) (text, phrase string) {
	for _, pair := range [][2]string{{"「", "」"}, {"“", "”"}, {`"`, `"`}, {"'", "'"}} {
		start := strings.Index(raw, pair[0])
		if start < 0 {
			continue
		}
		innerStart := start + len(pair[0])
		if innerStart > len(raw) {
			continue
		}
		end := strings.Index(raw[innerStart:], pair[1])
		if end < 0 {
			continue
		}
		inner := raw[innerStart : innerStart+end]
		text = raw[:start] + raw[innerStart+end+len(pair[1]):]
		return text, strings.TrimSpace(inner)
	}
	for _, marker := range speechMarkers {
		if idx := strings.LastIndex(raw, marker); idx >= 0 {
			phrase = strings.TrimSpace(raw[idx+len(marker):])
			if phrase == "" {
				continue
			}
			return raw[:idx] + raw[idx+len(marker):], phrase
		}
	}
	return raw, ""
}
