package eval

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/media"
)

// TestRunAndScoreEndToEnd drives the whole harness: a real clip through a
// fake multiframe endpoint, then scores the resulting database against a
// ground truth. Skipped without ffmpeg.
func TestRunAndScoreEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required to build the eval fixture")
	}
	corpusDir := t.TempDir()
	clipsDir := filepath.Join(corpusDir, "clips")
	if err := os.MkdirAll(clipsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	clip := filepath.Join(clipsDir, "night-street.mp4")
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=25",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", clip,
	}
	if raw, err := exec.CommandContext(context.Background(), "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("test fixture cannot be encoded by local ffmpeg: %v: %s", err, raw)
	}
	groundTruth := []Query{
		{Query: "wet street at night", Language: "en", Expected: []ExpectedShot{{Asset: "night-street.mp4", StartMS: 0, EndMS: 2000}}},
		{Query: "a car", Language: "en", Expected: []ExpectedShot{{Asset: "night-street.mp4", StartMS: 0, EndMS: 2000}}},
	}
	rawGT, _ := json.Marshal(groundTruth)
	if err := os.WriteFile(filepath.Join(corpusDir, "ground_truth.json"), rawGT, 0o600); err != nil {
		t.Fatal(err)
	}

	// The fake local endpoint answers the summary call and every shot call
	// with metadata the ground-truth queries can actually find.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		prompt := ""
		for _, part := range req.Messages[0].Content {
			if part.Type == "text" {
				prompt = part.Text
			}
		}
		reply := `{"summary":"city night","analysis":{"asset_type":"b_roll","shot_size":"wide","summary":"city night"}}`
		if strings.Contains(prompt, "single shot") {
			reply = `{"description":"wet city street at night with a parked car","objects":["car","street lamp"],"actions":["reflecting"],"mood":["calm"],"tags":["rain","night","reflection"],"shot_size":"wide","camera_motion":"static","quality":"usable","usable_as":["establishing"],"confidence":0.9}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + quote(reply) + `}}]}`))
	}))
	t.Cleanup(server.Close)

	dataDir := t.TempDir()
	cfg := config.Config{
		DataDir: dataDir, CacheDir: filepath.Join(dataDir, "cache"),
		DatabasePath:  filepath.Join(dataDir, "nexusgate.db"),
		Hardware:      media.HardwareConfig{Mode: "software", AllowFallback: true},
		SourceStaging: config.SourceStagingConfig{Mode: "none"},
		Pipeline:      config.PipelineConfig{ProviderRouteDeferralMinutes: 5},
	}
	cfg.Providers.LocalVLM = config.ProviderConfig{
		Enabled: true, Protocol: "openai_multiframe",
		BaseURL: server.URL + "/v1", Path: "chat/completions", Model: "qwen3-vl-4b",
	}
	cfg.Providers.VisionPrimary = "local_vlm"
	cfg.Providers.ShotDetection = config.ShotDetectionConfig{Enabled: true, Mode: config.ShotDetectionModeFFmpegScene}

	corpus, err := LoadCorpus(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), cfg, corpus, dataDir, "fake-qwen3vl")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(report.Assets))
	}
	if !report.Assets[0].Analyzed || report.Assets[0].Shots < 1 {
		t.Fatalf("clip not analyzed: %+v", report.Assets[0])
	}
	if report.Assets[0].FramesProcessed == 0 {
		t.Fatalf("no frames counted: %+v", report.Assets[0])
	}
	if report.Model != "qwen3-vl-4b" || report.Provider != "local_vlm" {
		t.Fatalf("report identity: %s / %s", report.Provider, report.Model)
	}

	score, err := Score(context.Background(), dataDir, "fake-qwen3vl", corpus)
	if err != nil {
		t.Fatal(err)
	}
	if score.Recall10 != 1.0 {
		t.Fatalf("R@10 = %v, want 1.0: %+v", score.Recall10, score.QueryDetail)
	}
	if score.FalsePositives != 0 {
		t.Fatalf("false positives = %d, want 0", score.FalsePositives)
	}
	if score.RelevantInTop10 != 2 {
		t.Fatalf("relevant hits = %d, want 2", score.RelevantInTop10)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
