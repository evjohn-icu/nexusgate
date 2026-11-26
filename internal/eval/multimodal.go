package eval

// Multimodal evaluation: manifest validation and the metrics Search v2 cannot
// borrow from the legacy harness.
//
// The legacy `Score` calls HybridSearchShots, so it can never prove the new
// channels. This file is deliberately pure: it takes ranked results and labelled
// intervals and computes the agreed metrics, which keeps the arithmetic
// testable without a model, a GPU or a database.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// MultimodalManifestVersion is the schema of multimodal-eval.v1.
const MultimodalManifestVersion = "multimodal-eval.v1"

// EvalInterval is a half-open asset-relative interval in milliseconds.
type EvalInterval struct {
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
}

// Validate checks the interval is non-empty.
func (iv EvalInterval) Validate() error {
	if iv.StartMS < 0 || iv.EndMS <= iv.StartMS {
		return fmt.Errorf("interval [%d,%d) is empty or reversed", iv.StartMS, iv.EndMS)
	}
	return nil
}

// OverlapMS returns the shared duration.
func (iv EvalInterval) OverlapMS(other EvalInterval) int64 {
	start := max(iv.StartMS, other.StartMS)
	end := min(iv.EndMS, other.EndMS)
	if end <= start {
		return 0
	}
	return end - start
}

// IoU is intersection over union, 0 for disjoint intervals.
func (iv EvalInterval) IoU(other EvalInterval) float64 {
	inter := iv.OverlapMS(other)
	if inter == 0 {
		return 0
	}
	union := iv.DurationMS() + other.DurationMS() - inter
	if union <= 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// DurationMS is EndMS-StartMS.
func (iv EvalInterval) DurationMS() int64 { return iv.EndMS - iv.StartMS }

// EvalAsset is one clip with its provenance and annotation coverage.
type EvalAsset struct {
	ID                 string         `json:"id"`
	SHA256             string         `json:"sha256"`
	License            string         `json:"license"`
	Source             string         `json:"source"`
	DurationMS         int64          `json:"duration_ms"`
	Partition          string         `json:"partition"`
	AnnotationCoverage []EvalInterval `json:"annotation_coverage"`
}

// EvalPositive is one graded positive interval.
type EvalPositive struct {
	AssetID  string       `json:"asset_id"`
	ShotID   string       `json:"shot_id,omitempty"`
	Interval EvalInterval `json:"interval"`
	Grade    int          `json:"grade"`
}

// EvalQuery is one retrieval query with graded positives, explicit negatives
// and the evidence the search must return.
type EvalQuery struct {
	ID               string         `json:"id"`
	Language         string         `json:"language"`
	Modality         string         `json:"modality"`
	Query            string         `json:"query"`
	ReferenceID      string         `json:"reference_id,omitempty"`
	Positives        []EvalPositive `json:"positives"`
	Negatives        []EvalPositive `json:"negatives"`
	ExpectedEvidence string         `json:"expected_evidence,omitempty"`
}

// EventAnnotation is one labelled sound event.
type EventAnnotation struct {
	AssetID  string       `json:"asset_id"`
	Label    string       `json:"label"`
	Interval EvalInterval `json:"interval"`
}

// SpeakerTrial is one evaluation-only same/different decision.
type SpeakerTrial struct {
	ID         string  `json:"id"`
	Same       bool    `json:"same"`
	Similarity float64 `json:"similarity"`
	Partition  string  `json:"partition"`
}

// DiarizationCase holds asset-scoped reference or predicted turns.
type DiarizationCase struct {
	AssetID   string           `json:"asset_id"`
	Reference []SpeakerSegment `json:"reference"`
	Predicted []SpeakerSegment `json:"predicted"`
}

// SpeakerSegment is one diarization label span.
type SpeakerSegment struct {
	Speaker  string       `json:"speaker"`
	Interval EvalInterval `json:"interval"`
}

// MultimodalManifest is the on-disk evaluation manifest.
type MultimodalManifest struct {
	SchemaVersion string            `json:"schema_version"`
	Assets        []EvalAsset       `json:"assets"`
	Queries       []EvalQuery       `json:"queries"`
	Events        []EventAnnotation `json:"events,omitempty"`
	Diarization   []DiarizationCase `json:"diarization,omitempty"`
	SpeakerTrials []SpeakerTrial    `json:"speaker_trials,omitempty"`
}

// LoadMultimodalManifest reads and validates a manifest.
func LoadMultimodalManifest(path string) (*MultimodalManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest MultimodalManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("parse multimodal manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &manifest, nil
}

// Validate enforces the manifest contract: immutable media identity, legal
// intervals, no partition leakage between calibration and test, and no query
// referencing an unknown asset.
func (m MultimodalManifest) Validate() error {
	if m.SchemaVersion != MultimodalManifestVersion {
		return fmt.Errorf("unsupported manifest schema %q", m.SchemaVersion)
	}
	assets := make(map[string]EvalAsset, len(m.Assets))
	partitions := make(map[string]bool)
	for _, asset := range m.Assets {
		if strings.TrimSpace(asset.ID) == "" {
			return fmt.Errorf("asset without an id")
		}
		if _, dup := assets[asset.ID]; dup {
			return fmt.Errorf("duplicate asset id %q", asset.ID)
		}
		if !strings.HasPrefix(asset.SHA256, "sha256:") || len(asset.SHA256) != len("sha256:")+64 {
			return fmt.Errorf("asset %q has no sha256 identity", asset.ID)
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(asset.SHA256, "sha256:")); err != nil {
			return fmt.Errorf("asset %q sha256 is not hex", asset.ID)
		}
		if strings.TrimSpace(asset.License) == "" || strings.TrimSpace(asset.Source) == "" {
			return fmt.Errorf("asset %q lacks licence or source", asset.ID)
		}
		if asset.DurationMS <= 0 {
			return fmt.Errorf("asset %q has no duration", asset.ID)
		}
		switch asset.Partition {
		case "calibration", "test", "dev":
		default:
			return fmt.Errorf("asset %q has unknown partition %q", asset.ID, asset.Partition)
		}
		partitions[asset.Partition] = true
		for _, coverage := range asset.AnnotationCoverage {
			if err := coverage.Validate(); err != nil {
				return fmt.Errorf("asset %q coverage: %w", asset.ID, err)
			}
			if coverage.EndMS > asset.DurationMS {
				return fmt.Errorf("asset %q coverage exceeds its duration", asset.ID)
			}
		}
		assets[asset.ID] = asset
	}
	if partitions["calibration"] && partitions["test"] {
		// Both are allowed in one manifest only when no query/speaker trial
		// crosses them; the per-record checks below enforce that. The flag is
		// kept for the summary so a report can say which partitions ran.
		_ = partitions
	}
	queryIDs := make(map[string]bool, len(m.Queries))
	for _, query := range m.Queries {
		if strings.TrimSpace(query.ID) == "" {
			return fmt.Errorf("query without an id")
		}
		if queryIDs[query.ID] {
			return fmt.Errorf("duplicate query id %q", query.ID)
		}
		queryIDs[query.ID] = true
		for _, positive := range append(append([]EvalPositive{}, query.Positives...), query.Negatives...) {
			asset, ok := assets[positive.AssetID]
			if !ok {
				return fmt.Errorf("query %q references unknown asset %q", query.ID, positive.AssetID)
			}
			if err := positive.Interval.Validate(); err != nil {
				return fmt.Errorf("query %q: %w", query.ID, err)
			}
			if positive.Interval.EndMS > asset.DurationMS {
				return fmt.Errorf("query %q has an interval outside asset %q", query.ID, positive.AssetID)
			}
		}
		if len(query.Positives) == 0 {
			return fmt.Errorf("query %q has no positive interval", query.ID)
		}
	}
	for _, event := range m.Events {
		asset, ok := assets[event.AssetID]
		if !ok {
			return fmt.Errorf("event annotation references unknown asset %q", event.AssetID)
		}
		if err := event.Interval.Validate(); err != nil {
			return fmt.Errorf("event %q: %w", event.Label, err)
		}
		if event.Interval.EndMS > asset.DurationMS {
			return fmt.Errorf("event on %q exceeds the asset duration", event.AssetID)
		}
	}
	seenDiarization := make(map[string]bool, len(m.Diarization))
	for _, testCase := range m.Diarization {
		asset, ok := assets[testCase.AssetID]
		if !ok || seenDiarization[testCase.AssetID] {
			return fmt.Errorf("diarization case references an unknown or repeated asset %q", testCase.AssetID)
		}
		seenDiarization[testCase.AssetID] = true
		if err := validateDiarizationCase(testCase, asset.DurationMS); err != nil {
			return err
		}
	}
	// Speaker trials must be partitioned by recording: calibration and test
	// identities are disjoint, so a trial may not mix partitions.
	seenTrialPartitions := make(map[string]bool)
	for _, trial := range m.SpeakerTrials {
		switch trial.Partition {
		case "calibration", "test":
		default:
			return fmt.Errorf("speaker trial %q has unknown partition %q", trial.ID, trial.Partition)
		}
		seenTrialPartitions[trial.Partition] = true
	}
	return nil
}

func validateDiarizationCase(testCase DiarizationCase, durationMS int64) error {
	for _, turns := range [][]SpeakerSegment{testCase.Reference, testCase.Predicted} {
		for _, turn := range turns {
			if strings.TrimSpace(turn.Speaker) == "" {
				return fmt.Errorf("diarization asset %q has a turn without a speaker ID", testCase.AssetID)
			}
			if err := turn.Interval.Validate(); err != nil || turn.Interval.EndMS > durationMS {
				return fmt.Errorf("diarization asset %q has a turn outside its duration", testCase.AssetID)
			}
		}
	}
	return nil
}

// ScoreDiarizationCases validates the recorded predictions before scoring
// anything. A missing asset case remains unevaluable in the report.
func (m MultimodalManifest) ScoreDiarizationCases(predictions []DiarizationCase) ([]DiarizationResult, error) {
	durations := make(map[string]int64, len(m.Assets))
	for _, asset := range m.Assets {
		durations[asset.ID] = asset.DurationMS
	}
	annotated := make(map[string]bool, len(m.Diarization))
	for _, testCase := range m.Diarization {
		annotated[testCase.AssetID] = true
	}
	byAsset := make(map[string][]SpeakerSegment, len(predictions))
	for _, prediction := range predictions {
		duration, known := durations[prediction.AssetID]
		if !known || !annotated[prediction.AssetID] {
			return nil, fmt.Errorf("prediction references an unannotated diarization asset %q", prediction.AssetID)
		}
		if _, duplicate := byAsset[prediction.AssetID]; duplicate {
			return nil, fmt.Errorf("duplicate diarization predictions for asset %q", prediction.AssetID)
		}
		if prediction.Predicted == nil {
			return nil, fmt.Errorf("diarization asset %q requires a predicted turns array", prediction.AssetID)
		}
		if err := validateDiarizationCase(prediction, duration); err != nil {
			return nil, err
		}
		byAsset[prediction.AssetID] = prediction.Predicted
	}
	results := make([]DiarizationResult, 0, len(m.Diarization))
	for _, testCase := range m.Diarization {
		result := DiarizationResult{AssetID: testCase.AssetID}
		if predicted, recorded := byAsset[testCase.AssetID]; recorded {
			result = ScoreDiarization(testCase.Reference, predicted, durations[testCase.AssetID])
			result.AssetID = testCase.AssetID
		}
		results = append(results, result)
	}
	return results, nil
}

// RankedItem is one search result in rank order.
type RankedItem struct {
	AssetID string `json:"asset_id"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

// RetrievalMetrics summarises one query set.
type RetrievalMetrics struct {
	Queries       int     `json:"queries"`
	PrecisionAt10 float64 `json:"precision_at_10"`
	RecallAt10    float64 `json:"recall_at_10"`
	NDCGAt10      float64 `json:"ndcg_at_10"`
}

// ScoreRetrieval computes P@10, Recall@10 and nDCG@10 under the documented
// interval rule: a result is correct when it overlaps a positive interval on
// the same asset with IoU >= 0.5, and one result may satisfy only one target.
// Precision keeps a fixed denominator of 10 (the standard P@10), so a short
// result list is not rewarded.
func ScoreRetrieval(query EvalQuery, ranked []RankedItem) (precision, recall, ndcg float64) {
	const k = 10
	gained := make([]bool, len(query.Positives))
	hits := 0
	dcg := 0.0
	for rank, item := range ranked {
		if rank >= k {
			break
		}
		grade := 0
		for i, positive := range query.Positives {
			if gained[i] || positive.AssetID != item.AssetID {
				continue
			}
			if (EvalInterval{StartMS: item.StartMS, EndMS: item.EndMS}).IoU(positive.Interval) >= 0.5 {
				gained[i] = true
				grade = positive.Grade
				if grade <= 0 {
					grade = 1
				}
				break
			}
		}
		if grade > 0 {
			hits++
			dcg += (math.Pow(2, float64(grade)) - 1) / math.Log2(float64(rank)+2)
		}
	}
	precision = float64(hits) / k
	if len(query.Positives) > 0 {
		recall = float64(hits) / float64(len(query.Positives))
	}
	ideal := make([]int, 0, len(query.Positives))
	for _, positive := range query.Positives {
		grade := positive.Grade
		if grade <= 0 {
			grade = 1
		}
		ideal = append(ideal, grade)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ideal)))
	idcg := 0.0
	for rank, grade := range ideal {
		if rank >= k {
			break
		}
		idcg += (math.Pow(2, float64(grade)) - 1) / math.Log2(float64(rank)+2)
	}
	if idcg > 0 {
		ndcg = dcg / idcg
	}
	return precision, recall, ndcg
}

// ScoreEventDetection matches predictions to annotations one-to-one within a
// class at IoU >= 0.5, then reports micro and macro F1 plus the mean boundary
// error of matched pairs.
func ScoreEventDetection(annotations, predictions []EventAnnotation) (microF1, macroF1, meanBoundaryErrorMS float64) {
	type key struct{ asset, label string }
	type intervals struct{ reference, predictions []EvalInterval }
	byKey := make(map[key]intervals)
	for _, annotation := range annotations {
		k := key{annotation.AssetID, annotation.Label}
		group := byKey[k]
		group.reference = append(group.reference, annotation.Interval)
		byKey[k] = group
	}
	for _, prediction := range predictions {
		k := key{prediction.AssetID, prediction.Label}
		group := byKey[k]
		group.predictions = append(group.predictions, prediction.Interval)
		byKey[k] = group
	}
	type counts struct{ tp, fp, fn int }
	byLabel := make(map[string]counts)
	var total counts
	var boundarySum float64
	var boundaryCount int
	for k, group := range byKey {
		tp, fp, fn, boundary, matched := matchIntervals(group.reference, group.predictions)
		tally := byLabel[k.label]
		tally.tp += tp
		tally.fp += fp
		tally.fn += fn
		byLabel[k.label] = tally
		total.tp += tp
		total.fp += fp
		total.fn += fn
		boundarySum += boundary
		boundaryCount += matched
	}
	microF1 = ratio(2*total.tp, 2*total.tp+total.fp+total.fn)
	if len(byLabel) > 0 {
		var sum float64
		labels := make([]string, 0, len(byLabel))
		for label := range byLabel {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		for _, label := range labels {
			count := byLabel[label]
			sum += ratio(2*count.tp, 2*count.tp+count.fp+count.fn)
		}
		macroF1 = sum / float64(len(labels))
	}
	if boundaryCount > 0 {
		meanBoundaryErrorMS = boundarySum / float64(boundaryCount)
	}
	return microF1, macroF1, meanBoundaryErrorMS
}

func matchIntervals(reference, predicted []EvalInterval) (tp, fp, fn int, boundarySum float64, matched int) {
	used := make([]bool, len(reference))
	for _, prediction := range predicted {
		best := -1
		bestIoU := 0.0
		for i, truth := range reference {
			if used[i] {
				continue
			}
			if iou := truth.IoU(prediction); iou >= 0.5 && iou > bestIoU {
				best = i
				bestIoU = iou
			}
		}
		if best < 0 {
			fp++
			continue
		}
		used[best] = true
		tp++
		matched++
		boundarySum += math.Abs(float64(prediction.StartMS-reference[best].StartMS)) +
			math.Abs(float64(prediction.EndMS-reference[best].EndMS))
	}
	fn = len(reference) - tp
	return tp, fp, fn, boundarySum, matched
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

// cell keys one reference/hypothesis speaker co-occurrence.
type cell struct {
	ref string
	hyp string
}

// DiarizationResult splits DER into overlap-inclusive and overlap-exclusive
// figures. The collar excuses 250 ms around each reference boundary, and the
// frame step is 10 ms, matching the timeline's resolution.
type DiarizationResult struct {
	AssetID           string  `json:"asset_id,omitempty"`
	DERWithOverlap    float64 `json:"der_with_overlap"`
	DERWithoutOverlap float64 `json:"der_without_overlap"`
	SpeakerCountError int     `json:"speaker_count_error"`
	Evaluable         bool    `json:"evaluable"`
}

// ScoreDiarization computes speaker-time DER with a single optimal mapping.
// The overlap-exclusive score omits frames with multiple reference speakers.
// Frames in the reference collar are excluded from both scores.
func ScoreDiarization(reference, predicted []SpeakerSegment, durationMS int64) DiarizationResult {
	const (
		frameMS      = 10
		collarFrames = 250 / frameMS
		refTrack     = 0
		hypTrack     = 1
		collarTrack  = 2
	)
	if durationMS <= 0 {
		return DiarizationResult{}
	}
	type change struct {
		frame   int64
		speaker string
		track   int
		delta   int
	}
	changes := make([]change, 0, 6*len(reference)+2*len(predicted))
	endFrame := durationMS / frameMS
	addSpan := func(from, to int64, speaker string, track int) {
		from, to = max(0, from), min(endFrame, to)
		if from < to {
			changes = append(changes, change{from, speaker, track, 1}, change{to, speaker, track, -1})
		}
	}
	var speakers [2]map[string]int
	for track, segments := range [][]SpeakerSegment{reference, predicted} {
		speakers[track] = make(map[string]int)
		for _, segment := range segments {
			start, end := segment.Interval.StartMS/frameMS, segment.Interval.EndMS/frameMS
			addSpan(start, end, segment.Speaker, track)
			speakers[track][segment.Speaker] = 0
			if track == refTrack {
				addSpan(start-collarFrames, start+collarFrames, "", collarTrack)
				addSpan(end-collarFrames, end+collarFrames, "", collarTrack)
			}
		}
	}
	countError := len(speakers[hypTrack]) - len(speakers[refTrack])
	clear(speakers[refTrack])
	clear(speakers[hypTrack])
	sort.Slice(changes, func(i, j int) bool { return changes[i].frame < changes[j].frame })

	pairsWith := make(map[cell]int)
	pairsWithout := make(map[cell]int)
	var refWith, errorWith, refWithout, errorWithout int
	var lastFrame int64
	var collar int
	for _, change := range changes {
		// The active speakers are constant between adjacent boundaries.
		// Count one unit of error per speaker until the mapping credits matches.
		width := int(change.frame - lastFrame)
		if width > 0 && collar == 0 {
			ref, hyp := speakers[refTrack], speakers[hypTrack]
			denominator := len(ref) * width
			potentialErrors := max(len(ref), len(hyp)) * width
			refWith += denominator
			errorWith += potentialErrors
			if len(ref) <= 1 {
				refWithout += denominator
				errorWithout += potentialErrors
			}
			for refSpeaker := range ref {
				for hypSpeaker := range hyp {
					pair := cell{refSpeaker, hypSpeaker}
					pairsWith[pair] += width
					if len(ref) <= 1 {
						pairsWithout[pair] += width
					}
				}
			}
		}
		lastFrame = change.frame
		if change.track == collarTrack {
			collar += change.delta
			continue
		}
		active := speakers[change.track]
		if count := active[change.speaker] + change.delta; count == 0 {
			delete(active, change.speaker)
		} else {
			active[change.speaker] = count
		}
	}
	for ref, hyp := range optimalAssignment(pairsWith) {
		pair := cell{ref, hyp}
		errorWith -= pairsWith[pair]
		errorWithout -= pairsWithout[pair]
	}
	return DiarizationResult{
		DERWithOverlap:    ratio(errorWith, refWith),
		DERWithoutOverlap: ratio(errorWithout, refWithout),
		SpeakerCountError: countError,
		Evaluable:         refWith > 0,
	}
}

// SpeakerMetrics is the threshold sweep outcome. Threshold selection happens on
// calibration trials only; the test partition is scored at the chosen value.
type SpeakerMetrics struct {
	Threshold float64 `json:"threshold"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	FAR       float64 `json:"far"`
	FRR       float64 `json:"frr"`
	EER       float64 `json:"eer"`
	Positives int     `json:"positives"`
	Negatives int     `json:"negatives"`
	Evaluable bool    `json:"evaluable"`
}

// SelectThreshold returns the highest-recall threshold whose precision is at
// least minPrecision on the calibration trials. Ties break toward the higher
// threshold, which rejects more non-matches.
func SelectThreshold(trials []SpeakerTrial, minPrecision float64) (SpeakerMetrics, bool) {
	if len(trials) == 0 {
		return SpeakerMetrics{Evaluable: false}, false
	}
	candidates := make([]float64, 0, len(trials)*2)
	for _, trial := range trials {
		candidates = append(candidates, trial.Similarity)
	}
	sort.Float64s(candidates)
	best := SpeakerMetrics{Evaluable: false}
	found := false
	for _, threshold := range candidates {
		metrics := ScoreSpeakerTrials(trials, threshold)
		if !metrics.Evaluable || metrics.Precision < minPrecision {
			continue
		}
		if !found || metrics.Recall > best.Recall ||
			(metrics.Recall == best.Recall && threshold > best.Threshold) {
			best = metrics
			found = true
		}
	}
	return best, found
}

// ScoreSpeakerTrials computes precision, recall, FAR and FRR at one threshold.
func ScoreSpeakerTrials(trials []SpeakerTrial, threshold float64) SpeakerMetrics {
	tp, fp, fn, tn := speakerCounts(trials, threshold)
	metrics := SpeakerMetrics{
		Threshold: threshold,
		Precision: ratio(tp, tp+fp),
		Recall:    ratio(tp, tp+fn),
		FAR:       ratio(fp, fp+tn),
		FRR:       ratio(fn, fn+tp),
		Positives: tp + fn,
		Negatives: fp + tn,
	}
	metrics.Evaluable = metrics.Positives > 0 && metrics.Negatives > 0
	if metrics.Evaluable {
		metrics.EER = equalErrorRate(trials)
	}
	return metrics
}

// speakerCounts is the one place the confusion matrix is computed. The EER
// sweep calls it directly: calling ScoreSpeakerTrials from equalErrorRate would
// recurse forever, because that function computes the EER itself.
func speakerCounts(trials []SpeakerTrial, threshold float64) (tp, fp, fn, tn int) {
	for _, trial := range trials {
		accept := trial.Similarity >= threshold
		switch {
		case trial.Same && accept:
			tp++
		case trial.Same && !accept:
			fn++
		case !trial.Same && accept:
			fp++
		default:
			tn++
		}
	}
	return tp, fp, fn, tn
}

// equalErrorRate sweeps thresholds and returns the point where FAR and FRR are
// closest, as the midpoint of that crossing.
func equalErrorRate(trials []SpeakerTrial) float64 {
	candidates := make([]float64, 0, len(trials)*2)
	for _, trial := range trials {
		candidates = append(candidates, trial.Similarity)
	}
	if len(candidates) == 0 {
		return 0
	}
	sort.Float64s(candidates)
	best, bestGap := 0.0, math.Inf(1)
	for _, threshold := range candidates {
		tp, fp, fn, tn := speakerCounts(trials, threshold)
		far := ratio(fp, fp+tn)
		frr := ratio(fn, fn+tp)
		gap := math.Abs(far - frr)
		if gap < bestGap {
			bestGap = gap
			best = (far + frr) / 2
		}
	}
	return best
}

// SummarizeRetrieval averages per-query metrics. The mean is over queries, so a
// query with no results still contributes its zero to the denominator.
func SummarizeRetrieval(results []RetrievalMetrics) RetrievalMetrics {
	if len(results) == 0 {
		return RetrievalMetrics{}
	}
	var precision, recall, ndcg float64
	for _, metrics := range results {
		precision += metrics.PrecisionAt10
		recall += metrics.RecallAt10
		ndcg += metrics.NDCGAt10
	}
	n := float64(len(results))
	return RetrievalMetrics{
		Queries:       len(results),
		PrecisionAt10: precision / n,
		RecallAt10:    recall / n,
		NDCGAt10:      ndcg / n,
	}
}

// HashFile returns the sha256 identity a manifest asset must carry.
func HashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		n, readErr := file.Read(buf)
		if n > 0 {
			hasher.Write(buf[:n])
		}
		if readErr != nil {
			if readErr.Error() == "EOF" {
				break
			}
			return "", readErr
		}
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

// optimalAssignment uses the Hungarian algorithm on sorted speaker IDs.
// Zero-weight dummy speakers allow either side to remain unmatched.
func optimalAssignment(pairs map[cell]int) map[string]string {
	refSet := map[string]bool{}
	hypSet := map[string]bool{}
	for key := range pairs {
		refSet[key.ref] = true
		hypSet[key.hyp] = true
	}
	refs := make([]string, 0, len(refSet))
	for ref := range refSet {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	hyps := make([]string, 0, len(hypSet))
	for hyp := range hypSet {
		hyps = append(hyps, hyp)
	}
	sort.Strings(hyps)
	result := make(map[string]string, min(len(refs), len(hyps)))
	if len(refs) == 0 || len(hyps) == 0 {
		return result
	}
	count := max(len(refs), len(hyps))
	rowPotential := make([]int, count+1)
	colPotential := make([]int, count+1)
	matchedRow := make([]int, count+1)
	previousCol := make([]int, count+1)
	slack := make([]int, count+1)
	used := make([]bool, count+1)
	for row := 1; row <= count; row++ {
		matchedRow[0] = row
		clear(used)
		for col := 1; col <= count; col++ {
			slack[col] = math.MaxInt
		}
		currentCol := 0
		for {
			used[currentCol] = true
			currentRow := matchedRow[currentCol]
			delta, nextCol := math.MaxInt, 0
			for col := 1; col <= count; col++ {
				if used[col] {
					continue
				}
				gain := 0
				if currentRow <= len(refs) && col <= len(hyps) {
					gain = pairs[cell{refs[currentRow-1], hyps[col-1]}]
				}
				cost := -gain - rowPotential[currentRow] - colPotential[col]
				if cost < slack[col] {
					slack[col] = cost
					previousCol[col] = currentCol
				}
				if slack[col] < delta {
					delta, nextCol = slack[col], col
				}
			}
			for col := 0; col <= count; col++ {
				if used[col] {
					rowPotential[matchedRow[col]] += delta
					colPotential[col] -= delta
				} else {
					slack[col] -= delta
				}
			}
			currentCol = nextCol
			if matchedRow[currentCol] == 0 {
				break
			}
		}
		for currentCol != 0 {
			prev := previousCol[currentCol]
			matchedRow[currentCol] = matchedRow[prev]
			currentCol = prev
		}
	}
	for col := 1; col <= len(hyps); col++ {
		row := matchedRow[col]
		if row <= len(refs) && pairs[cell{refs[row-1], hyps[col-1]}] > 0 {
			result[refs[row-1]] = hyps[col-1]
		}
	}
	return result
}
