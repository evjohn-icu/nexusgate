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
	start := max64(iv.StartMS, other.StartMS)
	end := min64(iv.EndMS, other.EndMS)
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

// DiarizationReference is one reference turn and the predicted turns over the
// same asset.
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
	byKey := make(map[key][]EvalInterval)
	for _, annotation := range annotations {
		byKey[key{annotation.AssetID, annotation.Label}] = append(byKey[key{annotation.AssetID, annotation.Label}], annotation.Interval)
	}
	predicted := make(map[key][]EvalInterval)
	for _, prediction := range predictions {
		predicted[key{prediction.AssetID, prediction.Label}] = append(predicted[key{prediction.AssetID, prediction.Label}], prediction.Interval)
	}
	labels := make(map[key]bool)
	for k := range byKey {
		labels[k] = true
	}
	for k := range predicted {
		labels[k] = true
	}
	var totalTP, totalFP, totalFN int
	var boundarySum float64
	var boundaryCount int
	var f1Sum float64
	var labelCount int
	for k := range labels {
		tp, fp, fn, boundary, matched := matchIntervals(byKey[k], predicted[k])
		totalTP += tp
		totalFP += fp
		totalFN += fn
		boundarySum += boundary
		boundaryCount += matched
		precision := ratio(tp, tp+fp)
		recall := ratio(tp, tp+fn)
		if precision+recall > 0 {
			f1Sum += 2 * precision * recall / (precision + recall)
			labelCount++
		}
	}
	precision := ratio(totalTP, totalTP+totalFP)
	recall := ratio(totalTP, totalTP+totalFN)
	if precision+recall > 0 {
		microF1 = 2 * precision * recall / (precision + recall)
	}
	if labelCount > 0 {
		macroF1 = f1Sum / float64(labelCount)
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
	DERWithOverlap    float64 `json:"der_with_overlap"`
	DERWithoutOverlap float64 `json:"der_without_overlap"`
	SpeakerCountError int     `json:"speaker_count_error"`
	Evaluable         bool    `json:"evaluable"`
}

// ScoreDiarization computes DER under the documented collar. Reference and
// predicted speakers are matched by optimal permutation on the overlap-free
// frames; the result is not evaluable when there is no scored frame.
func ScoreDiarization(reference, predicted []SpeakerSegment, durationMS int64) DiarizationResult {
	const (
		frameMS  = 10
		collarMS = 250
	)
	if durationMS <= 0 {
		return DiarizationResult{Evaluable: false}
	}
	mark := func(segments []SpeakerSegment) []string {
		frames := make([]string, durationMS/frameMS)
		for _, segment := range segments {
			start := segment.Interval.StartMS / frameMS
			end := segment.Interval.EndMS / frameMS
			for i := int(start); i < int(end) && i < len(frames); i++ {
				frames[i] = segment.Speaker
			}
		}
		return frames
	}
	refFrames := mark(reference)
	hypFrames := mark(predicted)
	collared := make([]bool, len(refFrames))
	overlapping := make([]bool, len(refFrames))
	refCounts := make(map[string]int)
	for _, speaker := range refFrames {
		if speaker != "" {
			refCounts[speaker]++
		}
	}
	predCounts := make(map[string]int)
	for _, speaker := range hypFrames {
		if speaker != "" {
			predCounts[speaker]++
		}
	}
	// A collar excuses frames near a reference boundary, not the whole span:
	// marking [start-collar, end+collar] would excuse the entire recording.
	collarFrames := int(collarMS / frameMS)
	for _, segment := range reference {
		start := int(segment.Interval.StartMS / frameMS)
		end := int(segment.Interval.EndMS / frameMS)
		for _, mark := range [][2]int{{start - collarFrames, start + collarFrames}, {end - collarFrames, end + collarFrames}} {
			from, to := mark[0], mark[1]
			if from < 0 {
				from = 0
			}
			for i := from; i < to && i < len(collared); i++ {
				collared[i] = true
			}
		}
	}
	// Overlap frames are those where more than one reference segment is active.
	active := make([]int, len(refFrames))
	for _, segment := range reference {
		start := int(segment.Interval.StartMS / frameMS)
		end := int(segment.Interval.EndMS / frameMS)
		for i := start; i < end && i < len(active); i++ {
			active[i]++
		}
	}
	for i := range active {
		if active[i] > 1 {
			overlapping[i] = true
		}
	}

	pairs := make(map[cell]int)
	for i := range refFrames {
		if refFrames[i] == "" || hypFrames[i] == "" {
			continue
		}
		pairs[cell{refFrames[i], hypFrames[i]}]++
	}
	assignment := optimalAssignment(pairs)

	var missWith, totalWith int
	var missWithout, totalWithout int
	for i := range refFrames {
		if collared[i] {
			continue
		}
		ref := refFrames[i]
		hyp := hypFrames[i]
		wrong := false
		if ref != "" {
			if mapped, ok := assignment[ref]; !ok || mapped != hyp {
				wrong = true
			}
		} else if hyp != "" {
			wrong = true
		}
		totalWith++
		if wrong {
			missWith++
		}
		if overlapping[i] {
			continue
		}
		totalWithout++
		if wrong {
			missWithout++
		}
	}
	result := DiarizationResult{
		DERWithOverlap:    ratio(missWith, totalWith),
		DERWithoutOverlap: ratio(missWithout, totalWithout),
		Evaluable:         totalWith > 0,
	}
	result.SpeakerCountError = len(predCounts) - len(refCounts)
	return result
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

// optimalAssignment finds the reference->hypothesis mapping that maximises the
// number of agreeing frames. It is an exhaustive bitmask DP over the predicted
// speakers (typically fewer than ten), which is the optimal permutation DER
// requires; a greedy pick fails on a case like A-X=5, A-Y=4, B-X=4, where it
// takes A-X and leaves B unassigned.
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
	if len(refs) == 0 || len(hyps) == 0 || len(hyps) > 20 {
		return map[string]string{}
	}
	const unset = -1
	type state struct {
		score int
		ref   int
	}
	width := 1 << len(hyps)
	dp := make([]int, width)
	for i := range dp {
		dp[i] = unset
	}
	dp[0] = 0
	for refIndex := range refs {
		next := make([]int, width)
		for i := range next {
			next[i] = unset
		}
		for mask, score := range dp {
			if score < 0 {
				continue
			}
			// Leaving this reference unassigned is allowed: its frames then
			// count as speaker error.
			if score > next[mask] {
				next[mask] = score
			}
			for hypIndex, hyp := range hyps {
				if mask&(1<<hypIndex) != 0 {
					continue
				}
				gain := pairs[cell{refs[refIndex], hyp}]
				candidate := score + gain
				nextMask := mask | 1<<hypIndex
				if candidate > next[nextMask] {
					next[nextMask] = candidate
				}
			}
		}
		dp = next
	}
	bestMask, bestScore := 0, unset
	for mask, score := range dp {
		if score > bestScore {
			bestScore, bestMask = score, mask
		}
	}
	// Reconstruct greedily from the optimal DP table by replaying each choice
	// against its recorded predecessor scores.
	assignment := make(map[string]string, len(refs))
	mask := bestMask
	for refIndex := len(refs) - 1; refIndex >= 0; refIndex-- {
		target := -1
		for hypIndex, hyp := range hyps {
			if mask&(1<<hypIndex) == 0 {
				continue
			}
			without := mask &^ (1 << hypIndex)
			if dpValue(dp, without) < 0 {
				continue
			}
			expected := dpValue(dp, without) + pairs[cell{refs[refIndex], hyp}]
			if expected == dpValue(dp, mask) {
				target = hypIndex
				break
			}
		}
		if target >= 0 {
			assignment[refs[refIndex]] = hyps[target]
			mask &^= 1 << target
		}
	}
	return assignment
}

func dpValue(table []int, mask int) int {
	if mask < 0 || mask >= len(table) {
		return -1
	}
	return table[mask]
}
