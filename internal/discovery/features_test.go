package discovery

import (
	"math"
	"strings"
	"testing"
)

func TestVectorForText_Dimensions(t *testing.T) {
	// Empty text produces a zero vector of correct length.
	v := VectorForText("")
	if len(v) != VectorSize {
		t.Fatalf("expected length %d, got %d", VectorSize, len(v))
	}
	for i, val := range v {
		if val != 0 {
			t.Errorf("expected zero at index %d for empty text, got %f", i, val)
		}
	}

	// Non-empty text produces a non-zero vector of correct length.
	v = VectorForText("city night rain")
	if len(v) != VectorSize {
		t.Fatalf("expected length %d, got %d", VectorSize, len(v))
	}
	nonZero := false
	for _, val := range v {
		if val != 0 {
			nonZero = true
			break
		}
	}
	if !nonZero {
		t.Error("expected non-zero vector for non-empty text")
	}
}

func TestVectorForText_Deterministic(t *testing.T) {
	text := "city night rain with cinematic mood"
	v1 := VectorForText(text)
	v2 := VectorForText(text)
	if len(v1) != len(v2) {
		t.Fatal("length mismatch")
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatalf("vectors differ at index %d: %f vs %f", i, v1[i], v2[i])
		}
	}
}

func TestVectorForText_OrthogonalApprox(t *testing.T) {
	// Two texts that share no semantic tokens should have cosine near 0.
	v1 := VectorForText("city night rain")
	v2 := VectorForText("beach day sunny")
	c := Cosine(v1, v2)
	// With 64 dimensions and few tokens, overlap is unlikely but not
	// impossible; accept < 0.60 as "not similar".
	if math.Abs(c) >= 0.60 {
		t.Errorf("expected cosine near 0 for unrelated texts, got %f", c)
	}
}

func TestCosine_SameVector(t *testing.T) {
	v := VectorForText("city night rain")
	c := Cosine(v, v)
	if math.Abs(c-1.0) > 1e-9 {
		t.Errorf("expected cosine 1.0 for identical vectors, got %f", c)
	}
}

func TestCosine_ZeroVector(t *testing.T) {
	zero := make([]float64, VectorSize)
	nonZero := VectorForText("city night")
	c := Cosine(zero, nonZero)
	if c != 0 {
		t.Errorf("expected cosine 0 for zero left vector, got %f", c)
	}
	c = Cosine(nonZero, zero)
	if c != 0 {
		t.Errorf("expected cosine 0 for zero right vector, got %f", c)
	}
}

func TestCosine_MismatchedLength(t *testing.T) {
	short := []float64{1, 0}
	long := []float64{1, 0, 0}
	if c := Cosine(short, long); c != 0 {
		t.Errorf("expected cosine 0 for mismatched length, got %f", c)
	}
}

func TestCosine_EmptySlice(t *testing.T) {
	if c := Cosine(nil, nil); c != 0 {
		t.Errorf("expected cosine 0 for nil slices, got %f", c)
	}
	if c := Cosine([]float64{}, []float64{1}); c != 0 {
		t.Errorf("expected cosine 0 for empty left, got %f", c)
	}
}

func TestSemanticTokens_EnglishAliases(t *testing.T) {
	tokens := semanticTokens("it was raining in the city at night")
	// Expect canonical tokens, not raw words.
	for _, canonical := range []string{"rain", "city", "night"} {
		found := false
		for _, tok := range tokens {
			if tok == canonical {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected token %q, got %v", canonical, tokens)
		}
	}
}

func TestSemanticTokens_ChineseAliases(t *testing.T) {
	tokens := semanticTokens("雨中的城市夜晚")
	// Chinese aliases map to the same canonical tokens.
	for _, canonical := range []string{"rain", "city", "night"} {
		found := false
		for _, tok := range tokens {
			if tok == canonical {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected token %q from Chinese text, got %v", canonical, tokens)
		}
	}
}

func TestSemanticTokens_CJKSkipped(t *testing.T) {
	// CJK characters that are not in aliases should be dropped from token
	// output (bigrams are an FTS retrieval representation, not semantic).
	tokens := semanticTokens("日本語の天気")
	// "天気" (weather) is CJK and not in aliases, so should not appear.
	// But "日本語" is also CJK. The result may contain no tokens.
	for _, tok := range tokens {
		if strings.ContainsAny(tok, "日本語天気") {
			t.Errorf("CJK text %q leaked into token %q", "日本語の天気", tok)
		}
	}
}

func TestSemanticTokens_Empty(t *testing.T) {
	tokens := semanticTokens("")
	if tokens != nil {
		t.Errorf("expected nil for empty text, got %v", tokens)
	}
	tokens = semanticTokens("   ")
	if tokens != nil {
		t.Errorf("expected nil for whitespace-only text, got %v", tokens)
	}
}
