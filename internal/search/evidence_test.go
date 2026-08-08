package search

import (
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

func TestEvidenceConfirmedFromStructuredFields(t *testing.T) {
	q := Compile("汽车经过街道")
	c := Candidate{
		ShotID:  "s1",
		Objects: []string{"car"},
		Actions: []string{"walking"},
		Tags:    []string{"street"},
		Mood:    []string{},
		Signals: map[string]float64{},
	}
	evidence := evaluateConstraints(q, c, nil)
	car := evidenceForValue(evidence, "car")
	if car == nil || car.State != EvidenceConfirmed {
		t.Fatalf("car must be confirmed from objects, got %+v", car)
	}
	foundObjects := false
	for _, source := range car.Sources {
		if source == SourceObjects {
			foundObjects = true
		}
	}
	if !foundObjects {
		t.Fatalf("car evidence must cite objects, got %+v", car.Sources)
	}
}

func TestEvidenceDescriptionOnlyIsPossible(t *testing.T) {
	q := Compile("rain")
	c := Candidate{ShotID: "s1", Description: "a rainy street scene", Signals: map[string]float64{}}
	evidence := evaluateConstraints(q, c, nil)
	rain := evidenceForValue(evidence, "rain")
	if rain == nil || rain.State != EvidencePossible {
		t.Fatalf("rain must be possible from description only, got %+v", rain)
	}
}

func TestEvidenceTranscriptIsPossibleNotConfirmed(t *testing.T) {
	q := Compile("rain")
	c := Candidate{ShotID: "s1", Description: "quiet street", Signals: map[string]float64{}}
	evidence := evaluateConstraints(q, c, nil)
	rain := evidenceForValue(evidence, "rain")
	if rain == nil || rain.State != EvidenceUnknown {
		t.Fatalf("no transcript given, rain must be unknown, got %+v", rain)
	}
}

func TestEvidenceMissingIsUnknownNotAbsent(t *testing.T) {
	// The core honesty rule: a shot that never mentions person has UNKNOWN
	// person evidence — not "no person".
	q := Compile("person")
	c := Candidate{ShotID: "s1", Description: "empty street at dawn", Tags: []string{"empty"}, Signals: map[string]float64{}}
	evidence := evaluateConstraints(q, c, nil)
	p := evidenceForValue(evidence, "person")
	if p == nil || p.State != EvidenceUnknown {
		t.Fatalf("missing person evidence must be unknown, got %+v", p)
	}
}

func TestEvidenceExplicitNegationContradicts(t *testing.T) {
	q := Compile("person")
	c := Candidate{ShotID: "s1", Description: "empty beach no people", Signals: map[string]float64{}}
	evidence := evaluateConstraints(q, c, nil)
	p := evidenceForValue(evidence, "person")
	if p == nil || p.State != EvidenceContradicted {
		t.Fatalf("explicit no-people must contradict person, got %+v", p)
	}
}

func TestEvidenceMustNotAbsenceStatedStaysUnknown(t *testing.T) {
	// For a no-person query, a shot that says "no people" supports the
	// negative: its evidence must be unknown (absence stated), not "observed".
	q := Compile("没有人的海边空镜")
	c := Candidate{ShotID: "s1", Description: "empty beach no people", Tags: []string{"empty", "beach"}, Signals: map[string]float64{}}
	evidence := evaluateConstraints(q, c, nil)
	p := evidenceForValue(evidence, "person")
	if p == nil || !p.Negated {
		t.Fatalf("person evidence must exist as negated, got %+v", p)
	}
	if p.State != EvidenceUnknown {
		t.Fatalf("no-people shot must keep person unknown, got %s", p.State)
	}
}

func TestEvidenceMustNotObservedExcludes(t *testing.T) {
	q := Compile("没有人的海边空镜")
	c := Candidate{ShotID: "s1", Objects: []string{"person"}, Description: "person walking on beach", Signals: map[string]float64{}}
	evidence := evaluateConstraints(q, c, nil)
	p := evidenceForValue(evidence, "person")
	if p == nil || !p.Negated || p.State != EvidenceConfirmed {
		t.Fatalf("observed person in mustNot must be confirmed(observed), got %+v", p)
	}
}

func TestEvidenceSpeechPhraseFromTranscript(t *testing.T) {
	q := Compile("谁说过我们明天出发")
	c := Candidate{ShotID: "s1", Description: "documentary", Signals: map[string]float64{}}
	spans := []domain.AlignmentWord{
		{StartMS: 100_000, EndMS: 101_000, Text: "我们"},
		{StartMS: 101_000, EndMS: 102_000, Text: "明天"},
		{StartMS: 102_000, EndMS: 103_000, Text: "出发"},
	}
	evidence := evaluateConstraints(q, c, spans)
	speech := evidenceForValue(evidence, "我们明天出发")
	if speech == nil || speech.State != EvidencePossible {
		t.Fatalf("speech phrase must be possible from transcript, got %+v", speech)
	}
	if len(speech.Sources) == 0 || speech.Sources[0] != SourceTranscript {
		t.Fatalf("speech evidence must cite transcript, got %+v", speech.Sources)
	}
}

func evidenceForValue(evidence []Evidence, value string) *Evidence {
	for i := range evidence {
		if evidence[i].Value == value {
			return &evidence[i]
		}
	}
	return nil
}
