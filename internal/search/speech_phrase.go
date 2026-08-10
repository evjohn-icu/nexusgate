package search

import (
	"strings"
	"unicode"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

const maxSpeechPhraseGapMS int64 = 1500

// matchAlignedSpeechPhrase requires the complete phrase, rather than a token
// hit, so retrieval cannot become speech evidence for a different utterance.
func matchAlignedSpeechPhrase(phrase string, spans []domain.AlignmentWord) bool {
	components := speechComponents(strings.TrimSpace(strings.ToLower(phrase)))
	if len(components) == 0 {
		return false
	}
	for start := 0; start < len(spans); start++ {
		if matchSpeechComponents(components, spans, start) {
			return true
		}
	}
	return false
}

type speechComponent struct {
	text string
	cjk  bool
}

func speechComponents(text string) []speechComponent {
	r := []rune(text)
	var out []speechComponent
	for i := 0; i < len(r); {
		if isSpeechCJK(r[i]) {
			j := i + 1
			for j < len(r) && isSpeechCJK(r[j]) {
				j++
			}
			out = append(out, speechComponent{string(r[i:j]), true})
			i = j
			continue
		}
		if unicode.IsLetter(r[i]) || unicode.IsDigit(r[i]) || r[i] == '_' {
			j := i + 1
			for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_') && !isSpeechCJK(r[j]) {
				j++
			}
			out = append(out, speechComponent{string(r[i:j]), false})
			i = j
			continue
		}
		i++
	}
	return out
}

func matchSpeechComponents(cs []speechComponent, spans []domain.AlignmentWord, pos int) bool {
	var previous *domain.AlignmentWord
	for _, component := range cs {
		if component.cjk {
			joined := ""
			for i := pos; i < len(spans); i++ {
				if previous != nil && spans[i].StartMS-previous.EndMS > maxSpeechPhraseGapMS {
					return false
				}
				joined += strings.ToLower(spans[i].Text)
				pos = i + 1
				if joined == component.text {
					previous = &spans[i]
					break
				}
				if !strings.HasPrefix(component.text, joined) {
					return false
				}
			}
			if previous == nil || joined != component.text {
				return false
			}
			continue
		}
		found := false
		for i := pos; i < len(spans); i++ {
			if previous != nil && spans[i].StartMS-previous.EndMS > maxSpeechPhraseGapMS {
				return false
			}
			if strings.ToLower(spans[i].Text) == component.text {
				previous = &spans[i]
				pos = i + 1
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func isSpeechCJK(r rune) bool {
	return r >= '\u3040' && r <= '\u30ff' || r >= '\u3400' && r <= '\u9fff' || r >= '\uac00' && r <= '\ud7af' || r >= '\uf900' && r <= '\ufaff'
}

// MatchAlignedSpeechPhrase is the repository boundary for exact speech validation.
func MatchAlignedSpeechPhrase(phrase string, spans []domain.AlignmentWord) bool {
	return matchAlignedSpeechPhrase(phrase, spans)
}
