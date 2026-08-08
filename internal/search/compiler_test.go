package search

import (
	"reflect"
	"testing"
)

func constraintsToValues(constraints []Constraint) []string {
	out := make([]string, 0, len(constraints))
	for _, c := range constraints {
		out = append(out, string(c.Type)+":"+c.Value)
	}
	return out
}

// The spec's golden example: the compiler must extract the two objects as
// musts and the rest as shoulds, preserving word order.
func TestCompileUmbrellaNightExample(t *testing.T) {
	q := Compile("夜晚下雨，有人撑伞走过街道")
	if q.Intent != IntentFact {
		t.Fatalf("intent = %s, want fact", q.Intent)
	}
	wantMust := []string{"object:person", "object:umbrella"}
	wantShould := []string{"action:walking", "scene:street", "weather:rain", "time:night"}
	gotMust := constraintsToValues(q.Must)
	gotShould := constraintsToValues(q.Should)
	if !reflect.DeepEqual(gotMust, wantMust) {
		t.Fatalf("must = %v, want %v", gotMust, wantMust)
	}
	if !reflect.DeepEqual(gotShould, wantShould) {
		t.Fatalf("should = %v, want %v", gotShould, wantShould)
	}
}

func TestCompileShortQueryPromotesToMust(t *testing.T) {
	q := Compile("rain")
	if len(q.Must) != 1 || q.Must[0].Value != "rain" || q.Must[0].Type != ConstraintWeather {
		t.Fatalf("bare rain must be a must constraint, got %+v", q.Must)
	}
	if len(q.Should) != 0 {
		t.Fatalf("should must be empty after promotion, got %+v", q.Should)
	}
}

func TestCompileNegativeBeachQuery(t *testing.T) {
	q := Compile("没有人的海边空镜")
	if len(q.MustNot) != 1 || q.MustNot[0].Value != "person" {
		t.Fatalf("mustNot = %+v, want [person]", q.MustNot)
	}
	foundBeach := false
	for _, c := range append(append([]Constraint{}, q.Must...), q.Should...) {
		if c.Value == "beach" {
			foundBeach = true
		}
	}
	if !foundBeach {
		t.Fatalf("beach must stay positive, got must=%+v should=%+v", q.Must, q.Should)
	}
}

func TestCompileNegativeCarQuery(t *testing.T) {
	q := Compile("没有车的街道")
	if len(q.MustNot) != 1 || q.MustNot[0].Value != "car" {
		t.Fatalf("mustNot = %+v, want [car]", q.MustNot)
	}
}

func TestCompileSpeechMarker(t *testing.T) {
	q := Compile("谁说过我们明天出发")
	if q.Intent != IntentSpeech {
		t.Fatalf("intent = %s, want speech", q.Intent)
	}
	found := false
	for _, c := range q.Must {
		if c.Type == ConstraintSpeech && c.Value == "我们明天出发" {
			found = true
		}
	}
	if !found {
		t.Fatalf("speech phrase missing from must: %+v", q.Must)
	}
}

func TestCompileQuotedSpeech(t *testing.T) {
	q := Compile(`他说“明天见”`)
	if q.Intent != IntentSpeech {
		t.Fatalf("intent = %s, want speech", q.Intent)
	}
	found := false
	for _, c := range q.Must {
		if c.Type == ConstraintSpeech && c.Value == "明天见" {
			found = true
		}
	}
	if !found {
		t.Fatalf("quoted phrase missing from must: %+v", q.Must)
	}
}

func TestCompileShotID(t *testing.T) {
	q := Compile("类似 shot_abc123xyz 的镜头")
	if q.Intent != IntentSimilar {
		t.Fatalf("intent = %s, want similar", q.Intent)
	}
	found := false
	for _, c := range q.Must {
		if c.Type == ConstraintMetadata && c.Value == "shot_abc123xyz" {
			found = true
		}
	}
	if !found {
		t.Fatalf("shot id missing from constraints: %+v", q.Must)
	}
}

func TestCompileEmpty(t *testing.T) {
	q := Compile("   ")
	if q.Intent != IntentAuto || len(q.Must) != 0 {
		t.Fatalf("blank query must compile to zero query, got %+v", q)
	}
}

func TestCompileDetectsNothingForNegativeOnly(t *testing.T) {
	// A pure negative with only stopwords must not fabricate positive musts.
	q := Compile("没有人的地方")
	if len(q.Must) != 0 {
		t.Fatalf("generic stopword must not become a must, got %+v", q.Must)
	}
}
