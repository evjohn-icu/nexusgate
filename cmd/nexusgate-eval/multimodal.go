package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/evjohn-icu/nexusgate/internal/eval"
)

// multimodalResults is the recorded output of one real search run. It is
// deliberately a separate file from the manifest: the manifest is labelled
// ground truth, the results are what a model actually returned, and mixing the
// two would let a harness score itself.
type multimodalResults struct {
	SchemaVersion string `json:"schema_version"`
	// Ranking maps a query id to the ordered results the product returned.
	Ranking map[string][]eval.RankedItem `json:"ranking"`
	// Events are produced detections for the annotated assets.
	Events []eval.EventAnnotation `json:"events,omitempty"`
	// Diarization holds predicted turns per asset.
	Diarization []eval.DiarizationCase `json:"diarization,omitempty"`
	// SpeakerTrials are test-partition similarities (calibration trials come
	// from the manifest to keep the split explicit).
	SpeakerTrials []eval.SpeakerTrial `json:"speaker_trials,omitempty"`
	// Runtime carries provenance an operator must see next to the numbers.
	Runtime map[string]string `json:"runtime,omitempty"`
}

// multimodalReport is the scored summary.
type multimodalReport struct {
	SchemaVersion string                           `json:"schema_version"`
	Manifest      string                           `json:"manifest"`
	Retrieval     eval.RetrievalMetrics            `json:"retrieval"`
	PerQuery      map[string]eval.RetrievalMetrics `json:"per_query"`
	Events        struct {
		MicroF1             float64 `json:"micro_f1"`
		MacroF1             float64 `json:"macro_f1"`
		MeanBoundaryErrorMS float64 `json:"mean_boundary_error_ms"`
	} `json:"events"`
	Diarization  []eval.DiarizationResult `json:"diarization,omitempty"`
	Speaker      *eval.SpeakerMetrics     `json:"speaker,omitempty"`
	NotEvaluable []string                 `json:"not_evaluable,omitempty"`
	Runtime      map[string]string        `json:"runtime,omitempty"`
}

func multimodalCmd(args []string) error {
	if len(args) == 0 || args[0] != "score" {
		return fmt.Errorf("usage: nexusgate-eval multimodal score --manifest <file> --results <file> [--out <file>]")
	}
	fs := flag.NewFlagSet("nexusgate-eval multimodal score", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "multimodal-eval.v1 manifest")
	resultsPath := fs.String("results", "", "recorded search results (multimodal-results.v1)")
	outPath := fs.String("out", "", "optional report path")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *manifestPath == "" || *resultsPath == "" {
		return fmt.Errorf("--manifest and --results are required")
	}
	manifest, err := eval.LoadMultimodalManifest(*manifestPath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*resultsPath)
	if err != nil {
		return err
	}
	var results multimodalResults
	if err := json.Unmarshal(raw, &results); err != nil {
		return fmt.Errorf("parse results: %w", err)
	}
	if results.SchemaVersion != "multimodal-results.v1" {
		return fmt.Errorf("unsupported results schema %q", results.SchemaVersion)
	}
	report := multimodalReport{
		SchemaVersion: "multimodal-report.v1",
		Manifest:      *manifestPath,
		PerQuery:      make(map[string]eval.RetrievalMetrics, len(manifest.Queries)),
		Runtime:       results.Runtime,
	}
	if results.Ranking == nil {
		report.NotEvaluable = append(report.NotEvaluable, "retrieval: no ranking recorded")
	}
	var perQuery []eval.RetrievalMetrics
	for _, query := range manifest.Queries {
		ranked := results.Ranking[query.ID]
		precision, recall, ndcg := eval.ScoreRetrieval(query, ranked)
		metrics := eval.RetrievalMetrics{Queries: 1, PrecisionAt10: precision, RecallAt10: recall, NDCGAt10: ndcg}
		report.PerQuery[query.ID] = metrics
		perQuery = append(perQuery, metrics)
	}
	report.Retrieval = eval.SummarizeRetrieval(perQuery)

	if len(manifest.Events) > 0 {
		micro, macro, boundary := eval.ScoreEventDetection(manifest.Events, results.Events)
		report.Events.MicroF1 = micro
		report.Events.MacroF1 = macro
		report.Events.MeanBoundaryErrorMS = boundary
	} else {
		report.NotEvaluable = append(report.NotEvaluable, "sound events: no annotations")
	}

	for _, testCase := range manifest.Diarization {
		var duration int64
		for _, asset := range manifest.Assets {
			if asset.ID == testCase.AssetID {
				duration = asset.DurationMS
				break
			}
		}
		report.Diarization = append(report.Diarization, eval.ScoreDiarization(testCase.Reference, testCase.Predicted, duration))
	}
	if len(manifest.Diarization) == 0 {
		report.NotEvaluable = append(report.NotEvaluable, "diarization: no cases")
	}

	if len(manifest.SpeakerTrials) > 0 {
		var calibration []eval.SpeakerTrial
		for _, trial := range manifest.SpeakerTrials {
			if trial.Partition == "calibration" {
				calibration = append(calibration, trial)
			}
		}
		chosen, ok := eval.SelectThreshold(calibration, 0.95)
		if !ok {
			report.NotEvaluable = append(report.NotEvaluable, "speaker: no calibration threshold reached 95% precision")
		} else {
			tested := eval.ScoreSpeakerTrials(results.SpeakerTrials, chosen.Threshold)
			report.Speaker = &tested
			if !tested.Evaluable {
				report.NotEvaluable = append(report.NotEvaluable, "speaker: test trials lack both classes")
			}
		}
	} else {
		report.NotEvaluable = append(report.NotEvaluable, "speaker: no calibration trials")
	}
	sort.Strings(report.NotEvaluable)

	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if *outPath != "" {
		if err := os.WriteFile(*outPath, encoded, 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("retrieval: queries=%d P@10=%.4f R@10=%.4f nDCG@10=%.4f\n",
		report.Retrieval.Queries, report.Retrieval.PrecisionAt10, report.Retrieval.RecallAt10, report.Retrieval.NDCGAt10)
	fmt.Printf("events: micro-F1=%.4f macro-F1=%.4f boundary-error=%.1fms\n",
		report.Events.MicroF1, report.Events.MacroF1, report.Events.MeanBoundaryErrorMS)
	for i, result := range report.Diarization {
		fmt.Printf("diarization[%d]: DER(overlap)=%.4f DER(no-overlap)=%.4f speaker-count-error=%d\n",
			i, result.DERWithOverlap, result.DERWithoutOverlap, result.SpeakerCountError)
	}
	if report.Speaker != nil {
		fmt.Printf("speaker: threshold=%.4f precision=%.4f recall=%.4f FAR=%.4f FRR=%.4f EER=%.4f\n",
			report.Speaker.Threshold, report.Speaker.Precision, report.Speaker.Recall,
			report.Speaker.FAR, report.Speaker.FRR, report.Speaker.EER)
	}
	for _, item := range report.NotEvaluable {
		fmt.Printf("not evaluable: %s\n", item)
	}
	fmt.Printf("%s\n", encoded)
	return nil
}
