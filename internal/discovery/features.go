// Package discovery provides deterministic, local semantic features for shots.
// These vectors represent the already-extracted visual analysis; they never
// inspect or upload original media.
package discovery

import (
	"hash/fnv"
	"math"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/textindex"
)

const VectorSize = 64

// HeuristicVectorModel identifies the hashing scheme in VectorForText. Stored
// vectors carry it, so vectors produced by a different scheme are never scored
// against vectors from this one: a scheme change must re-embed rather than
// silently compare across dimension spaces.
//
// The stored value keeps the historical name "semantic-hash-v1" so existing
// rows keep matching; do not read that string as a claim. This vector is a
// deterministic heuristic feature built from FNV hashing and a hand-written
// multilingual alias table — explicitly NOT a learned embedding — and every
// consumer (hybrid search, similar shots, rare-shot discovery) treats it as
// such. Capability docs must not upgrade its wording.
const HeuristicVectorModel = "semantic-hash-v1"

func VectorForShot(shot domain.AssetShot) []float64 {
	parts := append([]string{shot.Description}, shot.Tags...)
	parts = append(parts, shot.Objects...)
	parts = append(parts, shot.Actions...)
	parts = append(parts, shot.Mood...)
	return VectorForText(strings.Join(parts, " "))
}

func TokensForShot(shot domain.AssetShot) []string {
	parts := append([]string{shot.Description}, shot.Tags...)
	parts = append(parts, shot.Objects...)
	parts = append(parts, shot.Actions...)
	parts = append(parts, shot.Mood...)
	return semanticTokens(strings.Join(parts, " "))
}

// TokensForText is the exported form of semanticTokens: the same tokens that
// VectorForText hashes, so a caller can ask "does this query share any token
// with this shot" without re-deriving the tokenizer rules.
func TokensForText(text string) []string {
	return semanticTokens(text)
}

func VectorForText(text string) []float64 {
	vector := make([]float64, VectorSize)
	for _, token := range semanticTokens(text) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(token))
		sum := h.Sum32()
		index := int(sum % VectorSize)
		if sum&1 == 0 {
			vector[index] += 1
		} else {
			vector[index] -= 1
		}
	}
	normalize(vector)
	return vector
}

func Cosine(left, right []float64) float64 {
	if len(left) == 0 || len(left) != len(right) {
		// This branch is silent by necessity — 0 is also a legitimate cosine
		// value, so nothing downstream can tell "orthogonal" from "wrong
		// dimensions" — and that is exactly why it must not be the place a
		// scheme change is caught. Callers reject mismatched vectors before
		// reaching here: the model filter on shot_semantic_vectors and the
		// length check in sqlite's semanticVector. Kept as a defensive floor
		// so a future caller that forgets both degrades instead of panicking.
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for i := range left {
		dot += left[i] * right[i]
		leftNorm += left[i] * left[i]
		rightNorm += right[i] * right[i]
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / math.Sqrt(leftNorm*rightNorm)
}

func semanticTokens(text string) []string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return nil
	}
	aliases := map[string][]string{
		"rain":      {"rain", "rainy", "raining", "雨"},
		"city":      {"city", "urban", "downtown", "城市", "城"},
		"night":     {"night", "nighttime", "evening", "夜", "晚"},
		"street":    {"street", "road", "alley", "街", "路"},
		"person":    {"person", "people", "human", "pedestrian", "人", "行人"},
		"traffic":   {"traffic", "car", "vehicle", "车", "车流"},
		"cinematic": {"cinematic", "film", "moody", "电影", "氛围"},
		"beach":     {"beach", "ocean", "sea", "海", "沙滩"},
		"day":       {"day", "daytime", "sunny", "白天", "晴"},
	}
	seen := map[string]bool{}
	output := make([]string, 0, len(aliases)+8)
	appendToken := func(token string) {
		if token != "" && !seen[token] {
			seen[token] = true
			output = append(output, token)
		}
	}
	// Alias words are matched by whole word, not substring: "car" must not
	// match "carefree", "careful" or "carpet" (a live retrieval finding — the
	// golden corpus's first pass missed it because no fixture carried such a
	// word). CJK alias words stay substring-based: Chinese text has no word
	// boundaries, and "车" inside "红色轿车" is the semantic bridge.
	asciiTokens := map[string]bool{}
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		asciiTokens[field] = true
	}
	for canonical, words := range aliases {
		for _, word := range words {
			matched := false
			if hasCJK(word) {
				matched = strings.Contains(text, word)
			} else {
				matched = asciiTokens[word]
			}
			if matched {
				appendToken(canonical)
				break
			}
		}
	}
	// Reuse the shared tokenizer's non-CJK word boundaries. CJK bigrams are an
	// FTS retrieval representation, not semantic concepts: hashing each one
	// into the legacy 64-dimensional heuristic would distort similarity scores.
	// The explicit multilingual aliases above remain the CJK semantic bridge.
	for _, chunk := range textindex.Chunks(text) {
		if chunk.CJK {
			continue
		}
		for _, field := range chunk.Tokens {
			if len([]rune(field)) >= 3 && !containsAlias(aliases, field) {
				appendToken(field)
			}
		}
	}
	return output
}

func containsAlias(aliases map[string][]string, value string) bool {
	for _, words := range aliases {
		for _, word := range words {
			if value == word {
				return true
			}
		}
	}
	return false
}

// hasCJK reports whether the string carries CJK runes. Alias words with CJK
// runes are matched as substrings (Chinese text has no spaces); everything
// else is matched as a whole token.
func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x2E80 {
			return true
		}
	}
	return false
}

func normalize(vector []float64) {
	var sum float64
	for _, value := range vector {
		sum += value * value
	}
	if sum == 0 {
		return
	}
	scale := 1 / math.Sqrt(sum)
	for i := range vector {
		vector[i] *= scale
	}
}
