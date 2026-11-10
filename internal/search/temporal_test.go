package search

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestTemporalPredicates(t *testing.T) {
	tests := []struct {
		name      string
		relation  string
		left      TimeInterval
		right     TimeInterval
		maxGapMS  int64
		want      bool
		wantError bool
	}{
		{name: "touching intervals are not simultaneous despite gap tolerance", relation: RelationSimultaneous, left: TimeInterval{StartMS: 0, EndMS: 1000}, right: TimeInterval{StartMS: 1000, EndMS: 2000}, maxGapMS: 500, want: false},
		{name: "overlapping intervals are simultaneous", relation: RelationSimultaneous, left: TimeInterval{StartMS: 0, EndMS: 1001}, right: TimeInterval{StartMS: 1000, EndMS: 2000}, want: true},
		{name: "before includes exact 500ms gap", relation: RelationBefore, left: TimeInterval{StartMS: 0, EndMS: 1000}, right: TimeInterval{StartMS: 1500, EndMS: 2000}, maxGapMS: 500, want: true},
		{name: "before excludes gap over 499ms bound", relation: RelationBefore, left: TimeInterval{StartMS: 0, EndMS: 1000}, right: TimeInterval{StartMS: 1500, EndMS: 2000}, maxGapMS: 499, want: false},
		{name: "later horn touching action does not precede it", relation: RelationBefore, left: TimeInterval{StartMS: 12000, EndMS: 13000}, right: TimeInterval{StartMS: 10500, EndMS: 12000}, maxGapMS: 500, want: false},
		{name: "before permits touching boundary", relation: RelationBefore, left: TimeInterval{StartMS: 0, EndMS: 1000}, right: TimeInterval{StartMS: 1000, EndMS: 2000}, maxGapMS: 1, want: true},
		{name: "during permits equal start and end", relation: RelationDuring, left: TimeInterval{StartMS: 10, EndMS: 20}, right: TimeInterval{StartMS: 10, EndMS: 20}, want: true},
		{name: "during rejects an overrun", relation: RelationDuring, left: TimeInterval{StartMS: 9, EndMS: 20}, right: TimeInterval{StartMS: 10, EndMS: 20}, want: false},
		{name: "starts within includes equal start", relation: RelationStartsWithin, left: TimeInterval{StartMS: 10, EndMS: 30}, right: TimeInterval{StartMS: 10, EndMS: 20}, want: true},
		{name: "starts within excludes right end", relation: RelationStartsWithin, left: TimeInterval{StartMS: 20, EndMS: 30}, right: TimeInterval{StartMS: 10, EndMS: 20}, want: false},
		{name: "ends within includes equal end", relation: RelationEndsWithin, left: TimeInterval{StartMS: 10, EndMS: 20}, right: TimeInterval{StartMS: 5, EndMS: 20}, want: true},
		{name: "ends within excludes right start", relation: RelationEndsWithin, left: TimeInterval{StartMS: 10, EndMS: 15}, right: TimeInterval{StartMS: 15, EndMS: 20}, want: false},
		{name: "after is symmetric with swapped before", relation: RelationAfter, left: TimeInterval{StartMS: 1500, EndMS: 2000}, right: TimeInterval{StartMS: 0, EndMS: 1000}, maxGapMS: 500, want: true},
		{name: "unknown relation", relation: "near", left: TimeInterval{StartMS: 0, EndMS: 10}, right: TimeInterval{StartMS: 0, EndMS: 10}, wantError: true},
		{name: "negative max gap", relation: RelationDuring, left: TimeInterval{StartMS: 0, EndMS: 10}, right: TimeInterval{StartMS: 0, EndMS: 10}, maxGapMS: -1, wantError: true},
		{name: "before requires positive max gap", relation: RelationBefore, left: TimeInterval{StartMS: 0, EndMS: 10}, right: TimeInterval{StartMS: 10, EndMS: 20}, wantError: true},
		{name: "invalid empty interval", relation: RelationSimultaneous, left: TimeInterval{StartMS: 0, EndMS: 0}, right: TimeInterval{StartMS: 0, EndMS: 10}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := EvaluatePredicate(test.relation, test.left, test.right, test.maxGapMS)
			if (err != nil) != test.wantError {
				t.Fatalf("EvaluatePredicate() error = %v, wantError %v", err, test.wantError)
			}
			if err == nil && got != test.want {
				t.Fatalf("EvaluatePredicate() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestTemporalAfterSwapsBeforeOperands(t *testing.T) {
	left := TimeInterval{StartMS: 1400, EndMS: 2000}
	right := TimeInterval{StartMS: 1000, EndMS: 1200}
	after, err := EvaluatePredicate(RelationAfter, left, right, 200)
	if err != nil {
		t.Fatal(err)
	}
	before, err := EvaluatePredicate(RelationBefore, right, left, 200)
	if err != nil {
		t.Fatal(err)
	}
	if after != before || !after {
		t.Fatalf("after(%v,%v)=%v, swapped before=%v; both should match", left, right, after, before)
	}
}

func TestTemporalSupportIntervals(t *testing.T) {
	left := TimeInterval{StartMS: 100, EndMS: 400}
	right := TimeInterval{StartMS: 250, EndMS: 500}
	got := SupportIntervals(TemporalPredicate{Relation: RelationSimultaneous}, left, right)
	if want := []Interval{{StartMS: 250, EndMS: 400}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("simultaneous support = %v, want %v", got, want)
	}
	got = SupportIntervals(TemporalPredicate{Relation: RelationDuring}, left, TimeInterval{StartMS: 100, EndMS: 500})
	if want := []Interval{{StartMS: 100, EndMS: 400}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("during support = %v, want %v", got, want)
	}
	got = SupportIntervals(TemporalPredicate{Relation: RelationBefore, MaxGapMS: 100}, TimeInterval{StartMS: 0, EndMS: 100}, TimeInterval{StartMS: 200, EndMS: 300})
	if want := []Interval{{StartMS: 0, EndMS: 100}, {StartMS: 200, EndMS: 300}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("before support = %v, want %v", got, want)
	}
	if got := SupportIntervals(TemporalPredicate{Relation: RelationSimultaneous}, left, TimeInterval{StartMS: 400, EndMS: 600}); got != nil {
		t.Fatalf("touching intervals support = %v, want nil", got)
	}
	if intersection, ok := Intersect(left, right); !ok || intersection != (Interval{StartMS: 250, EndMS: 400}) {
		t.Fatalf("Intersect() = (%v,%v), want ([250,400),true)", intersection, ok)
	}
}

func TestTemporalEvidenceClassification(t *testing.T) {
	left := ObservedAt{Modality: ModalitySoundEvent, Interval: TimeInterval{StartMS: 100, EndMS: 200}, CanonicalTimedObservation: true, EvidenceStatus: EvidenceStatusConfirmed}
	right := ObservedAt{Modality: ModalityVisual, Interval: TimeInterval{StartMS: 150, EndMS: 250}, CanonicalTimedObservation: true, EvidenceStatus: EvidenceStatusConfirmed}
	result, err := EvaluateObservedPredicate(TemporalPredicate{ID: "overlap", Relation: RelationSimultaneous}, left, right)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Matched || result.EvidenceStatus != EvidenceStatusConfirmed || result.EvidenceSource != SourceSoundEvent {
		t.Fatalf("canonical predicate result = %+v, want confirmed sound_event primary evidence", result)
	}
	if want := []EvidenceSource{SourceSoundEvent, SourceVisual}; !reflect.DeepEqual(result.EvidenceSources, want) {
		t.Fatalf("evidence sources = %v, want %v", result.EvidenceSources, want)
	}
	right.CanonicalTimedObservation = false
	right.EvidenceStatus = EvidenceStatusPossible
	result, err = EvaluateObservedPredicate(TemporalPredicate{Relation: RelationSimultaneous}, left, right)
	if err != nil {
		t.Fatal(err)
	}
	if result.EvidenceStatus != EvidenceStatusPossible {
		t.Fatalf("uncalibrated/shot-wide operand status = %q, want possible", result.EvidenceStatus)
	}
}

func TestTemporalCompiler(t *testing.T) {
	tests := []struct {
		query    string
		left     string
		right    string
		relation string
	}{
		{query: "rain 和 horn 同时出现", left: "rain", right: "horn", relation: RelationSimultaneous},
		{query: "A and B happen at the same time", left: "A", right: "B", relation: RelationSimultaneous},
		{query: "A while B", left: "A", right: "B", relation: RelationSimultaneous},
		{query: "rain 在 action 之前出现", left: "rain", right: "action", relation: RelationBefore},
		{query: "A before B", left: "A", right: "B", relation: RelationBefore},
		{query: "A 之后出现 B", left: "A", right: "B", relation: RelationAfter},
		{query: "A after B", left: "A", right: "B", relation: RelationAfter},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			query, ok := CompileTemporalQuery(test.query)
			if !ok || query == nil {
				t.Fatal("CompileTemporalQuery() did not recognize the explicit template")
			}
			if query.Conditions[0].Query != test.left || query.Conditions[1].Query != test.right {
				t.Fatalf("compiled conditions = %+v, want %q then %q", query.Conditions, test.left, test.right)
			}
			if query.Predicates[0].Relation != test.relation {
				t.Fatalf("relation = %q, want %q", query.Predicates[0].Relation, test.relation)
			}
			if query.Interpretation != test.query {
				t.Fatalf("interpretation = %q, want verbatim template %q", query.Interpretation, test.query)
			}
			if err := ValidateMultimodal(MultimodalRequest{Temporal: query}); err != nil {
				t.Fatalf("compiled query fails request validation: %v", err)
			}
		})
	}
}

func TestTemporalCompilerFallback(t *testing.T) {
	for _, input := range []string{"rain near horn", "A before", "A and B maybe together", ""} {
		if got, ok := CompileTemporalQuery(input); ok || got != nil {
			t.Fatalf("CompileTemporalQuery(%q) = (%v,%v), want (nil,false)", input, got, ok)
		}
	}
}

func TestMultimodalValidationRejections(t *testing.T) {
	baseConditions := []TemporalCondition{
		{ID: "left", Modality: ModalityLexical, Query: "rain"},
		{ID: "right", Modality: ModalitySoundEvent, Query: "horn"},
	}
	validPredicate := TemporalPredicate{ID: "p", Relation: RelationSimultaneous, LeftID: "left", RightID: "right"}
	makeTemporal := func(conditions []TemporalCondition, predicates []TemporalPredicate) *TemporalQuery {
		return &TemporalQuery{Conditions: conditions, Predicates: predicates}
	}
	manyConditions := make([]TemporalCondition, 9)
	for i := range manyConditions {
		manyConditions[i] = TemporalCondition{ID: strconv.Itoa(i), Modality: ModalityLexical}
	}
	manyPredicates := make([]TemporalPredicate, 17)
	for i := range manyPredicates {
		manyPredicates[i] = TemporalPredicate{ID: "p" + strconv.Itoa(i), Relation: RelationSimultaneous, LeftID: "left", RightID: "right"}
	}
	unknownConditionModality := append([]TemporalCondition(nil), baseConditions...)
	unknownConditionModality[0].Modality = "pixels"
	duplicateConditions := append([]TemporalCondition(nil), baseConditions...)
	duplicateConditions[1].ID = duplicateConditions[0].ID
	emptyConditionID := append([]TemporalCondition(nil), baseConditions...)
	emptyConditionID[0].ID = " "
	cases := []struct {
		name string
		req  MultimodalRequest
	}{
		{name: "unknown request modality", req: MultimodalRequest{Modalities: []string{"imaginary"}}},
		{name: "unknown condition modality", req: MultimodalRequest{Temporal: makeTemporal(unknownConditionModality, []TemporalPredicate{validPredicate})}},
		{name: "empty condition ID", req: MultimodalRequest{Temporal: makeTemporal(emptyConditionID, []TemporalPredicate{validPredicate})}},
		{name: "duplicate condition ID", req: MultimodalRequest{Temporal: makeTemporal(duplicateConditions, []TemporalPredicate{validPredicate})}},
		{name: "too many conditions", req: MultimodalRequest{Temporal: makeTemporal(manyConditions, nil)}},
		{name: "too many predicates", req: MultimodalRequest{Temporal: makeTemporal(baseConditions, manyPredicates)}},
		{name: "missing left endpoint", req: MultimodalRequest{Temporal: makeTemporal(baseConditions, []TemporalPredicate{{ID: "p", Relation: RelationSimultaneous, LeftID: "missing", RightID: "right"}})}},
		{name: "missing right endpoint", req: MultimodalRequest{Temporal: makeTemporal(baseConditions, []TemporalPredicate{{ID: "p", Relation: RelationSimultaneous, LeftID: "left", RightID: "missing"}})}},
		{name: "empty predicate ID", req: MultimodalRequest{Temporal: makeTemporal(baseConditions, []TemporalPredicate{{Relation: RelationSimultaneous, LeftID: "left", RightID: "right"}})}},
		{name: "unknown relation", req: MultimodalRequest{Temporal: makeTemporal(baseConditions, []TemporalPredicate{{ID: "p", Relation: "near", LeftID: "left", RightID: "right"}})}},
		{name: "negative gap", req: MultimodalRequest{Temporal: makeTemporal(baseConditions, []TemporalPredicate{{ID: "p", Relation: RelationDuring, LeftID: "left", RightID: "right", MaxGapMS: -1}})}},
		{name: "before requires gap", req: MultimodalRequest{Temporal: makeTemporal(baseConditions, []TemporalPredicate{{ID: "p", Relation: RelationBefore, LeftID: "left", RightID: "right"}})}},
		{name: "after requires gap", req: MultimodalRequest{Temporal: makeTemporal(baseConditions, []TemporalPredicate{{ID: "p", Relation: RelationAfter, LeftID: "left", RightID: "right"}})}},
		{name: "empty speaker filter", req: MultimodalRequest{SpeakerFilter: &SpeakerFilter{}}},
		{name: "partial asset-local speaker filter", req: MultimodalRequest{SpeakerFilter: &SpeakerFilter{AssetID: "asset", LocalSpeakerID: "speaker"}}},
		{name: "mixed profile and asset filter", req: MultimodalRequest{SpeakerFilter: &SpeakerFilter{AssetID: "asset", DiarizationGenerationID: "gen", LocalSpeakerID: "speaker", ProfileID: "profile"}}},
		{name: "both visual references conflict", req: MultimodalRequest{ReferenceID: "reference", VisualShotID: "shot"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateMultimodal(test.req); err == nil {
				t.Fatal("ValidateMultimodal() accepted invalid request")
			}
		})
	}
	var conflicting *ConflictingReferencesError
	err := ValidateMultimodal(MultimodalRequest{ReferenceID: "reference", VisualShotID: "shot"})
	if !errors.As(err, &conflicting) || !errors.Is(err, ErrConflictingReferences) {
		t.Fatalf("conflicting reference error = %T %v, want typed conflict", err, err)
	}
}

func TestMultimodalValidationAcceptsSpeakerFilterForms(t *testing.T) {
	for name, filter := range map[string]SpeakerFilter{
		"asset-local": {AssetID: "asset", DiarizationGenerationID: "generation", LocalSpeakerID: "speaker"},
		"profile":     {ProfileID: "profile"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateMultimodal(MultimodalRequest{SpeakerFilter: &filter}); err != nil {
				t.Fatalf("valid speaker filter rejected: %v", err)
			}
		})
	}
}

func TestMultimodalValidationAllowsNoReference(t *testing.T) {
	if err := ValidateMultimodal(MultimodalRequest{}); err != nil {
		t.Fatalf("zero-value extension rejected: %v", err)
	}
}

func TestTemporalValidationRejectsDuplicatePredicateIDs(t *testing.T) {
	query := &TemporalQuery{
		Conditions: []TemporalCondition{{ID: "a", Modality: ModalityLexical}, {ID: "b", Modality: ModalityLexical}},
		Predicates: []TemporalPredicate{
			{ID: "same", Relation: RelationSimultaneous, LeftID: "a", RightID: "b"},
			{ID: "same", Relation: RelationSimultaneous, LeftID: "a", RightID: "b"},
		},
	}
	if err := ValidateMultimodal(MultimodalRequest{Temporal: query}); err == nil || !strings.Contains(err.Error(), "duplicate temporal predicate") {
		t.Fatalf("duplicate predicate validation error = %v, want duplicate-ID rejection", err)
	}
}
