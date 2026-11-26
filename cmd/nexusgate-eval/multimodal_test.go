package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/eval"
)

func TestMultimodalScoreWritesReport(t *testing.T) {
	dir := t.TempDir()
	manifest := map[string]any{
		"schema_version": "multimodal-eval.v1",
		"assets": []any{map[string]any{
			"id": "a", "sha256": "sha256:" + strings.Repeat("a", 64), "license": "CC-BY-4.0",
			"source": "fixture", "duration_ms": 4000, "partition": "calibration",
			"annotation_coverage": []any{map[string]any{"start_ms": 0, "end_ms": 4000}},
		}},
		"queries": []any{},
		"speaker_trials": []any{
			map[string]any{"id": "cal-same", "same": true, "similarity": 0.9, "partition": "calibration"},
			map[string]any{"id": "cal-diff", "same": false, "similarity": 0.1, "partition": "calibration"},
		},
	}
	results := map[string]any{
		"schema_version": "multimodal-results.v1", "ranking": map[string]any{},
		"speaker_trials": []any{
			map[string]any{"id": "test-same", "same": true, "similarity": 0.8, "partition": "test"},
			map[string]any{"id": "test-diff", "same": false, "similarity": 0.2, "partition": "test"},
		},
		"runtime": map[string]string{"model": "fixture"},
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	resultsPath := filepath.Join(dir, "results.json")
	reportPath := filepath.Join(dir, "report.json")
	for path, value := range map[string]any{manifestPath: manifest, resultsPath: results} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := multimodalCmd([]string{"score", "--manifest", manifestPath, "--results", resultsPath, "--out", reportPath}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		SchemaVersion string `json:"schema_version"`
		Speaker       *struct {
			Evaluable bool `json:"evaluable"`
		} `json:"speaker"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != "multimodal-report.v1" || report.Speaker == nil || !report.Speaker.Evaluable {
		t.Fatalf("unexpected report: %s", raw)
	}
	if err := multimodalCmd([]string{"score"}); err == nil {
		t.Fatal("missing flags accepted")
	}
}

func TestMultimodalScoreUsesOnlyRecordedDiarization(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	resultsPath := filepath.Join(dir, "results.json")
	reportPath := filepath.Join(dir, "report.json")
	interval := eval.EvalInterval{StartMS: 500, EndMS: 2500}
	reference := []eval.SpeakerSegment{{Speaker: "A", Interval: interval}}
	manifest := eval.MultimodalManifest{
		SchemaVersion: eval.MultimodalManifestVersion,
		Assets:        []eval.EvalAsset{{ID: "a", SHA256: "sha256:" + strings.Repeat("a", 64), License: "fixture", Source: "fixture", DurationMS: 4000, Partition: "test"}},
		Queries:       []eval.EvalQuery{{ID: "q", Positives: []eval.EvalPositive{{AssetID: "a", Interval: interval, Grade: 1}}}},
		Diarization:   []eval.DiarizationCase{{AssetID: "a", Reference: reference, Predicted: reference}},
	}
	writeJSON := func(path string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(manifestPath, manifest)
	for _, tt := range []struct {
		name      string
		cases     []eval.DiarizationCase
		ranking   map[string][]eval.RankedItem
		wantScore bool
		wantDER   float64
		wantError bool
	}{
		{name: "missing", ranking: map[string][]eval.RankedItem{}},
		{name: "perfect", cases: []eval.DiarizationCase{{AssetID: "a", Predicted: reference}}, wantScore: true},
		{name: "explicit empty", cases: []eval.DiarizationCase{{AssetID: "a", Predicted: []eval.SpeakerSegment{}}}, ranking: map[string][]eval.RankedItem{"q": {}}, wantScore: true, wantDER: 1},
		{name: "past duration", cases: []eval.DiarizationCase{{AssetID: "a", Predicted: []eval.SpeakerSegment{{Speaker: "B", Interval: eval.EvalInterval{StartMS: 5, EndMS: 4001}}}}}, wantError: true},
		{name: "negative", cases: []eval.DiarizationCase{{AssetID: "a", Predicted: []eval.SpeakerSegment{{Speaker: "B", Interval: eval.EvalInterval{StartMS: -5, EndMS: 50}}}}}, wantError: true},
		{name: "unannotated", cases: []eval.DiarizationCase{{AssetID: "b", Predicted: reference}}, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writeJSON(resultsPath, multimodalResults{SchemaVersion: "multimodal-results.v1", Ranking: tt.ranking, Diarization: tt.cases})
			err := multimodalCmd([]string{"score", "--manifest", manifestPath, "--results", resultsPath, "--out", reportPath})
			if tt.wantError {
				if err == nil {
					t.Fatal("invalid prediction was scored")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			var report multimodalReport
			if err := json.Unmarshal(raw, &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Diarization) != 1 || report.Diarization[0].Evaluable != tt.wantScore || report.Diarization[0].DERWithOverlap != tt.wantDER {
				t.Fatalf("diarization = %+v, want evaluable=%t DER=%g", report.Diarization, tt.wantScore, tt.wantDER)
			}
			if report.Retrieval.Queries != len(tt.ranking) {
				t.Fatalf("scored %d queries, want %d recorded queries", report.Retrieval.Queries, len(tt.ranking))
			}
		})
	}
}
