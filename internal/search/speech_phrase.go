package search

import (
	"strings"
	"unicode"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

const maxSpeechPhraseGapMS int64 = 1500

// matchAlignedSpeechPhrase requires the complete phrase, rather than a token
// hit, so retrieval cannot become speech evidence for a different utterance.
func matchAlignedSpeechPhrase(phrase string, spans []domain.AlignmentWord) bool {
	components := SpeechComponents(strings.TrimSpace(strings.ToLower(phrase)))
	if len(components) == 0 {
		return false
	}
	for start := range len(spans) {
		if _, _, ok := matchSpeechComponents(components, spans, start); ok {
			return true
		}
	}
	return false
}

// SpeechComponent is one ordered unit of a speech phrase: a contiguous CJK run
// or an ASCII word run, as the validation pass consumes them.
type SpeechComponent struct {
	text string
	cjk  bool
}

// Text returns the component surface text.
func (c SpeechComponent) Text() string { return c.text }

// IsCJK reports whether the component is a contiguous CJK run (matched by
// boundary runes) or an ASCII word (matched whole-word).
func (c SpeechComponent) IsCJK() bool { return c.cjk }

// SpeechComponents splits a lowercased speech phrase into ordered components:
// contiguous CJK runs and ASCII word runs. Exported so the repository layer
// can build phrase-first SQL prefilter conditions from the same component
// boundary the validation pass uses.
func SpeechComponents(text string) []SpeechComponent {
	r := []rune(text)
	var out []SpeechComponent
	for i := 0; i < len(r); {
		if isSpeechCJK(r[i]) {
			j := i + 1
			for j < len(r) && isSpeechCJK(r[j]) {
				j++
			}
			out = append(out, SpeechComponent{string(r[i:j]), true})
			i = j
			continue
		}
		if unicode.IsLetter(r[i]) || unicode.IsDigit(r[i]) || r[i] == '_' {
			j := i + 1
			for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_') && !isSpeechCJK(r[j]) {
				j++
			}
			out = append(out, SpeechComponent{string(r[i:j]), false})
			i = j
			continue
		}
		i++
	}
	return out
}

// matchSpeechComponents consumes the components in order over the spans
// starting at pos, using the established matching semantics: CJK components
// match as substrings joined across adjacent spans, ASCII components match
// whole aligned words, and adjacent consumed spans may not be more than
// maxSpeechPhraseGapMS apart. On success it returns the first and one-past
// the last consumed span index, so callers can report the located match's
// interval; earlier spans that matched nothing are skipped, not consumed.
func matchSpeechComponents(cs []SpeechComponent, spans []domain.AlignmentWord, pos int) (int, int, bool) {
	var previous *domain.AlignmentWord
	first := -1
	for _, component := range cs {
		if component.cjk {
			joined := ""
			for i := pos; i < len(spans); i++ {
				if previous != nil && spans[i].StartMS-previous.EndMS > maxSpeechPhraseGapMS {
					return first, pos, false
				}
				joined += strings.ToLower(spans[i].Text)
				previous = &spans[i]
				if first < 0 {
					first = i
				}
				pos = i + 1
				if joined == component.text {
					break
				}
				if !strings.HasPrefix(component.text, joined) {
					return first, pos, false
				}
			}
			if previous == nil || joined != component.text {
				return first, pos, false
			}
			continue
		}
		found := false
		for i := pos; i < len(spans); i++ {
			if previous != nil && spans[i].StartMS-previous.EndMS > maxSpeechPhraseGapMS {
				return first, pos, false
			}
			if strings.ToLower(spans[i].Text) == component.text {
				previous = &spans[i]
				if first < 0 {
					first = i
				}
				pos = i + 1
				found = true
				break
			}
		}
		if !found {
			return first, pos, false
		}
	}
	return first, pos, true
}

// speechMatchIntervals returns the intervals of every located occurrence of
// phrase in spans, using the same whole-word/ASR-fallback component semantics
// the transcript channel validates with. Occurrences are reported in temporal
// order and never overlap: the scan resumes after each consumed range.
func speechMatchIntervals(phrase string, spans []domain.AlignmentWord) []Interval {
	components := SpeechComponents(strings.ToLower(strings.TrimSpace(phrase)))
	if len(components) == 0 || len(spans) == 0 {
		return nil
	}
	var out []Interval
	for start := 0; start < len(spans); {
		first, end, ok := matchSpeechComponents(components, spans, start)
		if !ok || first < 0 || end <= first {
			start++
			continue
		}
		out = append(out, Interval{StartMS: spans[first].StartMS, EndMS: spans[end-1].EndMS})
		start = end
	}
	return out
}

func isSpeechCJK(r rune) bool {
	return r >= '\u3040' && r <= '\u30ff' || r >= '\u3400' && r <= '\u9fff' || r >= '\uac00' && r <= '\ud7af' || r >= '\uf900' && r <= '\ufaff'
}

// MatchAlignedSpeechPhrase is the repository boundary for exact speech validation.
func MatchAlignedSpeechPhrase(phrase string, spans []domain.AlignmentWord) bool {
	return matchAlignedSpeechPhrase(phrase, spans)
}
