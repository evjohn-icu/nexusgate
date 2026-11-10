package eval

import (
	"math"
	"testing"
)

func intv(start, end int64) EvalInterval { return EvalInterval{StartMS: start, EndMS: end} }

func TestScoreRetrievalUsesOneToOneIoUAndFixedDenominator(t *testing.T) {
	query := EvalQuery{
		ID: "q1",
		Positives: []EvalPositive{
			{AssetID: "a", Interval: intv(0, 1000), Grade: 3},
			{AssetID: "a", Interval: intv(2000, 3000), Grade: 2},
		},
	}
	// The first item matches the first positive; the second item overlaps the
	// same positive again and must not count twice.
	ranked := []RankedItem{
		{AssetID: "a", StartMS: 0, EndMS: 1000},
		{AssetID: "a", StartMS: 100, EndMS: 900},
		{AssetID: "a", StartMS: 2000, EndMS: 3000},
	}
	precision, recall, ndcg := ScoreRetrieval(query, ranked)
	if math.Abs(precision-0.2) > 1e-9 {
		t.Fatalf("precision=%v, want 2/10", precision)
	}
	if math.Abs(recall-1.0) > 1e-9 {
		t.Fatalf("recall=%v, want 1.0", recall)
	}
	if ndcg <= 0 || ndcg > 1 {
		t.Fatalf("ndcg=%v out of range", ndcg)
	}
	emptyPrecision, _, _ := ScoreRetrieval(query, nil)
	if math.Abs(emptyPrecision) > 1e-9 {
		t.Fatal("an empty result list must still divide by ten")
	}
}

func TestScoreRetrievalRequiresHalfIoU(t *testing.T) {
	query := EvalQuery{ID: "q", Positives: []EvalPositive{{AssetID: "a", Interval: intv(0, 1000), Grade: 1}}}
	below := []RankedItem{{AssetID: "a", StartMS: 0, EndMS: 400}}
	if _, recall, _ := ScoreRetrieval(query, below); recall != 0 {
		t.Fatalf("IoU 0.4 must not count: recall=%v", recall)
	}
	at := []RankedItem{{AssetID: "a", StartMS: 0, EndMS: 500}}
	if _, recall, _ := ScoreRetrieval(query, at); recall != 1 {
		t.Fatalf("IoU 0.5 must count: recall=%v", recall)
	}
	wrongAsset := []RankedItem{{AssetID: "b", StartMS: 0, EndMS: 1000}}
	if _, recall, _ := ScoreRetrieval(query, wrongAsset); recall != 0 {
		t.Fatal("an interval on another asset must not count")
	}
}

func TestEventDetectionScoresOneToOnePerClass(t *testing.T) {
	annotations := []EventAnnotation{
		{AssetID: "a", Label: "Rain", Interval: intv(1000, 5000)},
		{AssetID: "a", Label: "Horn", Interval: intv(9000, 10000)},
	}
	predictions := []EventAnnotation{
		{AssetID: "a", Label: "Rain", Interval: intv(1200, 4800)},
		{AssetID: "a", Label: "Rain", Interval: intv(1500, 4500)},
		{AssetID: "a", Label: "Horn", Interval: intv(3000, 4000)},
	}
	micro, macro, boundary := ScoreEventDetection(annotations, predictions)
	if micro <= 0 || micro >= 1 {
		t.Fatalf("micro F1=%v; duplicated rain and a misplaced horn must not score 1", micro)
	}
	if macro <= 0 {
		t.Fatalf("macro F1=%v", macro)
	}
	if boundary <= 0 {
		t.Fatalf("matched rain boundary error should be positive, got %v", boundary)
	}
	perfectMicro, _, _ := ScoreEventDetection(annotations, annotations)
	if math.Abs(perfectMicro-1) > 1e-9 {
		t.Fatalf("identical sets must score 1, got %v", perfectMicro)
	}
}

func TestDiarizationDERCountsCollarAndOverlap(t *testing.T) {
	reference := []SpeakerSegment{
		{Speaker: "A", Interval: intv(0, 5000)},
		{Speaker: "B", Interval: intv(5000, 10000)},
	}
	predicted := []SpeakerSegment{
		{Speaker: "SPEAKER_00", Interval: intv(0, 5000)},
		{Speaker: "SPEAKER_01", Interval: intv(5000, 10000)},
	}
	perfect := ScoreDiarization(reference, predicted, 10000)
	if !perfect.Evaluable {
		t.Fatal("a fully labelled case must be evaluable")
	}
	if perfect.DERWithOverlap != 0 || perfect.DERWithoutOverlap != 0 {
		t.Fatalf("a renamed but identical partition must score DER 0: %+v", perfect)
	}
	// One speaker is missing for the second half: two predicted speakers become
	// one, so half the reference is miss and the count error is -1.
	missing := ScoreDiarization(reference, []SpeakerSegment{{Speaker: "SPEAKER_00", Interval: intv(0, 5000)}}, 10000)
	if missing.DERWithOverlap < 0.4 {
		t.Fatalf("DER too low for a missing speaker: %+v", missing)
	}
	if missing.SpeakerCountError != -1 {
		t.Fatalf("speaker count error=%d, want -1", missing.SpeakerCountError)
	}
}

func TestSpeakerThresholdSelectionPrefersPrecisionThenRecall(t *testing.T) {
	trials := []SpeakerTrial{
		{ID: "s1", Same: true, Similarity: 0.9, Partition: "calibration"},
		{ID: "s2", Same: true, Similarity: 0.8, Partition: "calibration"},
		{ID: "s3", Same: true, Similarity: 0.4, Partition: "calibration"},
		{ID: "d1", Same: false, Similarity: 0.85, Partition: "calibration"},
		{ID: "d2", Same: false, Similarity: 0.3, Partition: "calibration"},
		{ID: "d3", Same: false, Similarity: 0.1, Partition: "calibration"},
	}
	metrics, ok := SelectThreshold(trials, 0.95)
	if !ok {
		t.Fatal("a threshold with perfect precision exists at 0.9")
	}
	if metrics.Precision < 0.95 {
		t.Fatalf("selected precision %v below the floor", metrics.Precision)
	}
	if metrics.Threshold != 0.9 {
		t.Fatalf("threshold=%v, want 0.9 (highest recall with precision >= 0.95)", metrics.Threshold)
	}
	if metrics.Positives != 3 || metrics.Negatives != 3 {
		t.Fatalf("denominators=%d/%d", metrics.Positives, metrics.Negatives)
	}
	if _, ok := SelectThreshold(nil, 0.95); ok {
		t.Fatal("no trials means no threshold")
	}
	if _, ok := SelectThreshold([]SpeakerTrial{{ID: "only", Same: true, Similarity: 0.5}}, 0.95); ok {
		t.Fatal("a single-class trial set cannot be calibrated")
	}
}

func TestManifestValidationRejectsLeakageAndBadIntervals(t *testing.T) {
	valid := MultimodalManifest{
		SchemaVersion: MultimodalManifestVersion,
		Assets: []EvalAsset{{
			ID: "a", SHA256: "sha256:" + hash64("a"), License: "CC0", Source: "generated",
			DurationMS: 40000, Partition: "test",
			AnnotationCoverage: []EvalInterval{intv(0, 40000)},
		}},
		Queries: []EvalQuery{{ID: "q", Positives: []EvalPositive{{AssetID: "a", Interval: intv(0, 1000), Grade: 1}}}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	cases := map[string]func(*MultimodalManifest){
		"unknown asset":     func(m *MultimodalManifest) { m.Queries[0].Positives[0].AssetID = "missing" },
		"reversed interval": func(m *MultimodalManifest) { m.Queries[0].Positives[0].Interval = intv(1000, 1000) },
		"outside duration":  func(m *MultimodalManifest) { m.Queries[0].Positives[0].Interval = intv(0, 50000) },
		"bad hash":          func(m *MultimodalManifest) { m.Assets[0].SHA256 = "sha256:zz" },
		"no licence":        func(m *MultimodalManifest) { m.Assets[0].License = "" },
		"bad partition":     func(m *MultimodalManifest) { m.Assets[0].Partition = "everything" },
		"no positives":      func(m *MultimodalManifest) { m.Queries[0].Positives = nil },
		"bad trial partition": func(m *MultimodalManifest) {
			m.SpeakerTrials = []SpeakerTrial{{ID: "t", Same: true, Partition: "tune"}}
		},
	}
	for name, mutate := range cases {
		manifest := valid
		manifest.Assets = append([]EvalAsset(nil), valid.Assets...)
		manifest.Queries = append([]EvalQuery(nil), valid.Queries...)
		mutate(&manifest)
		if err := manifest.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

// hash64 builds a deterministic 64-hex-character digest for fixtures without
// pulling crypto into every test literal.
func hash64(seed string) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = "0123456789abcdef"[int(seed[i%len(seed)]+byte(i))%16]
	}
	return string(out)
}

func TestOptimalAssignmentBeatsGreedy(t *testing.T) {
	pairs := map[cell]int{
		{"A", "X"}: 5,
		{"A", "Y"}: 4,
		{"B", "X"}: 4,
	}
	assignment := optimalAssignment(pairs)
	if assignment["A"] != "Y" || assignment["B"] != "X" {
		// Greedy takes A-X (5) and then leaves B with nothing (total 5); the
		// optimal permutation is A-Y + B-X (total 8).
		t.Fatalf("assignment=%v, want A->Y and B->X", assignment)
	}
	if got := optimalAssignment(nil); len(got) != 0 {
		t.Fatalf("empty pairs produced %v", got)
	}
}
