package search

import (
	"context"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

func TestEvidenceMixedPolarityKeepsBothAndGateExcludesObserved(t *testing.T) {
	q := SearchQuery{
		Intent:  IntentFact,
		Must:    []Constraint{{Type: ConstraintObject, Value: "person"}},
		MustNot: []Constraint{{Type: ConstraintObject, Value: "person"}},
	}
	observed := Candidate{ShotID: "observed", Objects: []string{"person"}, Signals: map[string]float64{}}

	evidence := evaluateConstraints(q, observed, nil)
	if len(evidence) != 2 {
		t.Fatalf("mixed-polarity query must retain both evidence entries, got %+v", evidence)
	}
	if evidence[0].Negated || !evidence[1].Negated {
		t.Fatalf("evidence order must remain positive then negated, got %+v", evidence)
	}
	if evidence[0].Constraint != ConstraintObject || evidence[1].Constraint != ConstraintObject {
		t.Fatalf("evidence constraints = %+v, want object for both polarities", evidence)
	}

	survivors, _, err := NewEvidenceGate(DefaultOptions()).Gate(
		context.Background(), &fakeStore{}, q, []Candidate{observed}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(survivors) != 0 {
		t.Fatalf("gate must exclude the forbidden-object shot, got %+v", survivors)
	}

	verdict := evidenceForGate(q, evidence)
	if !verdict.observedMustNot {
		t.Fatalf("negated evidence must mark the observed forbidden object: %+v", verdict)
	}
}

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

func TestCreativeIntentDoesNotCreateSpeechEvidence(t *testing.T) {
	q := Compile("给我找点雨")
	if q.Intent != IntentCreative {
		t.Fatalf("intent = %s, want creative", q.Intent)
	}
	if q.SpeechPhrase != "" {
		t.Fatalf("creative query unexpectedly compiled speech phrase %q", q.SpeechPhrase)
	}

	// The transcript contains the creative query term as part of a longer
	// utterance. It may be a retrieval hit, but without a speech constraint it
	// cannot be reported as speech evidence.
	c := Candidate{ShotID: "s1", Description: "creative sample", Signals: map[string]float64{}}
	spans := []domain.AlignmentWord{{StartMS: 100, EndMS: 200, Text: "雨天出发"}}
	evidence := evaluateConstraints(q, c, spans)
	rain := evidenceForValue(evidence, "rain")
	if rain == nil || rain.State != EvidenceUnknown {
		t.Fatalf("partial transcript phrase must not support creative query evidence: %+v", rain)
	}
	for _, evidence := range evidence {
		for _, source := range evidence.Sources {
			if evidence.Constraint == ConstraintSpeech || source == SourceTranscript {
				t.Fatalf("creative partial phrase produced speech evidence: %+v", evidence)
			}
		}
	}
}

func TestEvidenceConflictPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		candidate Candidate
		negated   bool
		value     string
		state     EvidenceState
		sources   []EvidenceSource
	}{
		{"structured and description negation", "person", Candidate{Objects: []string{"person"}, Description: "no people"}, false, "person", EvidenceContradicted, []EvidenceSource{SourceObjects, SourceDescription}},
		{"structured and positive description", "person", Candidate{Objects: []string{"person"}, Description: "a person"}, false, "person", EvidenceConfirmed, []EvidenceSource{SourceObjects, SourceDescription}},
		{"description negation only", "person", Candidate{Description: "no people"}, false, "person", EvidenceContradicted, []EvidenceSource{SourceDescription}},
		{"negated structured plus absence", "没有人的海边", Candidate{Objects: []string{"person"}, Description: "no people"}, true, "person", EvidenceConfirmed, []EvidenceSource{SourceObjects, SourceDescription}},
		{"negated description mention", "没有人的海边", Candidate{Description: "a person"}, true, "person", EvidencePossible, []EvidenceSource{SourceDescription}},
		{"negated absence only", "没有人的海边", Candidate{Description: "no people"}, true, "person", EvidenceUnknown, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := Compile(tt.query)
			got := evaluateConstraints(q, tt.candidate, nil)
			e := evidenceForValue(got, tt.value)
			if e == nil || e.State != tt.state || e.Negated != tt.negated {
				t.Fatalf("evidence=%+v, want state=%s negated=%v", e, tt.state, tt.negated)
			}
			if len(e.Sources) != len(tt.sources) {
				t.Fatalf("sources=%v, want %v", e.Sources, tt.sources)
			}
			for i := range tt.sources {
				if e.Sources[i] != tt.sources[i] {
					t.Fatalf("sources=%v, want %v", e.Sources, tt.sources)
				}
			}
		})
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
