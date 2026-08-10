package search

import (
	"testing"
)

// The whole-word discipline is the vocabulary's hard contract: ASCII alias
// words must never match as substrings. Both cases below are live retrieval
// findings (carefree→car was a real regression; train→rain is the same trap
// in the other direction).
func TestVocabularyASCIIWholeWordOnly(t *testing.T) {
	for _, text := range []string{"carefree", "careful", "carpet", "cargo"} {
		for _, m := range Canonicalize(text) {
			if m.Canonical == "car" {
				t.Fatalf("%q must not canonicalize to car, got %+v", text, m)
			}
		}
	}
	for _, m := range Canonicalize("raining") {
		if m.Canonical == "train" {
			t.Fatalf("raining must not canonicalize to train, got %+v", m)
		}
	}
}

func TestVocabularyCarriesMultilingualFamilies(t *testing.T) {
	cases := []struct {
		text      string
		canonical string
	}{
		{"汽车", "car"}, {"轿车", "car"}, {"car", "car"}, {"vehicle", "car"}, {"cars", "car"},
		{"伞", "umbrella"}, {"雨伞", "umbrella"}, {"umbrella", "umbrella"},
		{"人", "person"}, {"行人", "person"}, {"people", "person"},
		{"走", "walking"}, {"步行", "walking"}, {"walk", "walking"},
		{"街道", "street"}, {"street", "street"},
		{"下雨", "rain"}, {"rainy", "rain"},
		{"夜晚", "night"}, {"night", "night"},
		{"孤独", "lonely"}, {"压抑", "depressing"},
	}
	for _, c := range cases {
		values := CanonicalValues(c.text)
		found := false
		for _, v := range values {
			if v == c.canonical {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%q must canonicalize to %q, got %v", c.text, c.canonical, values)
		}
	}
}

func TestCanonicalizeOrderedByPosition(t *testing.T) {
	matches := Canonicalize("有人撑伞走过街道")
	want := []string{"person", "umbrella", "walking", "street"}
	got := make([]string, 0, len(matches))
	for _, m := range matches {
		got = append(got, m.Canonical)
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order mismatch: got %v want %v", got, want)
		}
	}
}

func TestCJKPassThroughOnlyShortUnknownTerms(t *testing.T) {
	// Long CJK runs are sentences, not terms — nothing passes through.
	values := CanonicalValues("夜晚下雨有人撑伞走过街道")
	for _, v := range values {
		if v == "夜晚下雨有人撑伞走过街道" {
			t.Fatalf("whole sentence leaked as a canonical: %v", values)
		}
	}
	// A short unknown term passes through as its own canonical.
	found := false
	for _, v := range CanonicalValues("红绿灯") {
		if v == "红绿灯" {
			found = true
		}
	}
	if !found {
		t.Fatalf("short unknown CJK term must pass through")
	}
	// Generic stopwords never become constraints.
	for _, v := range CanonicalValues("没有人的地方") {
		if v == "地方" {
			t.Fatalf("stopword leaked as a canonical")
		}
	}
}

func TestNegationSpans(t *testing.T) {
	spans := NegationSpans("没有人的海边空镜")
	if len(spans) == 0 {
		t.Fatal("expected negation spans")
	}
	// person (人) at byte offset 6 must be negated by 没有 [0,6) + window.
	negated := false
	for _, span := range spans {
		if Negates(span, ConstraintObject, 6) {
			negated = true
		}
	}
	if !negated {
		t.Fatal("person must be negated in 没有人的海边空镜")
	}
	// 海边 at offset 12 must NOT be negated.
	for _, span := range spans {
		if Negates(span, ConstraintScene, 12) {
			t.Fatal("海边 must not be negated by 没有人的")
		}
	}
}

func TestNegationAbsentDescriptorOnlyNegatesObjects(t *testing.T) {
	spans := NegationSpans("empty beach")
	found := false
	for _, span := range spans {
		if Negates(span, ConstraintScene, 6) {
			found = true
		}
	}
	if found {
		t.Fatal("empty must not negate beach (scene)")
	}
	// An empty car contains a car: absence descriptors describe content, not
	// existence, so they never negate the object they modify.
	for _, span := range spans {
		if Negates(span, ConstraintObject, 6) {
			t.Fatal("empty must not negate an object it modifies")
		}
	}
}

func TestNegationPhraseWindowCJK(t *testing.T) {
	// 空无一人的街道: 无 [3,6) — the window runs to the clause boundary 的 at
	// byte 12, so 人 (9) is negated while 街道 (15) is not. A fixed byte radius
	// left 人 outside and a no-person query kept the very shot that says no
	// person is present.
	spans := NegationSpans("空无一人的街道")
	negated := false
	for _, span := range spans {
		if Negates(span, ConstraintObject, 9) {
			negated = true
		}
	}
	if !negated {
		t.Fatal("人 must be negated in 空无一人的街道")
	}
	for _, span := range spans {
		if Negates(span, ConstraintScene, 15) {
			t.Fatal("街道 must not be negated by 空无一人的")
		}
	}
	// 无一人 (no clause boundary): the window reaches the end of the text.
	spans = NegationSpans("无一人")
	if !Negates(spans[0], ConstraintObject, 6) {
		t.Fatal("人 must be negated in 无一人")
	}
}

func TestNegationNoPersonASCII(t *testing.T) {
	spans := NegationSpans("no people on the beach")
	for _, span := range spans {
		if !Negates(span, ConstraintObject, 3) {
			t.Fatal("no must negate people at offset 3")
		}
	}
}
