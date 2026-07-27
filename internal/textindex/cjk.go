// Package textindex keeps the write and query token boundaries for Timingdex
// full-text search in one place. SQLite unicode61 does not split adjacent Han
// characters, so continuous CJK text is represented as overlapping bigrams.
package textindex

import (
	"strings"
	"unicode"
)

type Chunk struct {
	Tokens []string
	CJK    bool
}

func Segment(text string) string { return strings.Join(Tokens(text), " ") }

func Tokens(text string) []string {
	chunks := Chunks(text)
	out := make([]string, 0, len(chunks)*2)
	for _, chunk := range chunks {
		out = append(out, chunk.Tokens...)
	}
	return out
}

func Chunks(text string) []Chunk {
	runes := []rune(strings.ToLower(strings.TrimSpace(text)))
	chunks := make([]Chunk, 0)
	for i := 0; i < len(runes); {
		if isCJK(runes[i]) {
			start := i
			for i < len(runes) && isCJK(runes[i]) {
				i++
			}
			sequence := runes[start:i]
			tokens := make([]string, 0, len(sequence))
			if len(sequence) == 1 {
				tokens = append(tokens, string(sequence))
			} else {
				for j := 0; j+1 < len(sequence); j++ {
					tokens = append(tokens, string(sequence[j:j+2]))
				}
			}
			chunks = append(chunks, Chunk{Tokens: tokens, CJK: true})
			continue
		}
		if isWord(runes[i]) {
			start := i
			for i < len(runes) && isWord(runes[i]) {
				i++
			}
			chunks = append(chunks, Chunk{Tokens: []string{string(runes[start:i])}})
			continue
		}
		i++
	}
	return chunks
}

// FTSQuery creates a safe FTS5 MATCH expression. A continuous CJK phrase is
// kept as a phrase of overlapping bigrams, so “雨夜街道” cannot be satisfied by
// unrelated bigrams in arbitrary positions or fields.
func FTSQuery(text string) string {
	chunks := Chunks(text)
	terms := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		if len(chunk.Tokens) == 0 {
			continue
		}
		term := strings.ReplaceAll(strings.Join(chunk.Tokens, " "), `"`, `""`)
		terms = append(terms, `"`+term+`"`)
	}
	return strings.Join(terms, " AND ")
}

func IsCJKToken(value string) bool {
	runes := []rune(value)
	if len(runes) == 0 {
		return false
	}
	for _, r := range runes {
		if !isCJK(r) {
			return false
		}
	}
	return true
}

func isCJK(r rune) bool  { return r >= '\u3400' && r <= '\u9fff' }
func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
