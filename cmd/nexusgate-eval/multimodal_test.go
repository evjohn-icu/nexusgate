package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
