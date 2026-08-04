package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

func TestVideoProviderDecodesUnifiedGeminiResponse(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"summary":  "夜晚城市街道",
		"scenes":   []map[string]any{{"description": "霓虹商业街", "tags": []string{"urban_night"}}},
		"objects":  []map[string]any{{"name": "person", "count": 2}},
		"actions":  []map[string]any{{"name": "walking"}},
		"mood":     []string{"busy"},
		"raw_tags": []string{"night city"},
		"analysis": map[string]any{"asset_type": "b_roll", "quality": "usable"},
	})
	response, _ := json.Marshal(map[string]any{
		"candidates": []map[string]any{{"content": map[string]any{"parts": []map[string]string{{"text": string(payload)}}}}},
	})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/generate" {
			t.Errorf("unexpected provider path: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(response))), Header: make(http.Header), Request: r}, nil
	})}

	videoPath := filepath.Join(t.TempDir(), "sample.mp4")
	if err := os.WriteFile(videoPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &VideoProvider{Vision: &Vision{
		Endpoint:  common.Endpoint{BaseURL: "http://provider.invalid", AuthHeader: "x-api-key", AuthScheme: "raw", APIKey: "test", HTTPClient: client},
		ModelName: "fixture-model",
		Path:      "generate",
	}}

	result, raw, err := provider.Analyze(context.Background(), videoanalysis.Input{VideoPath: videoPath})
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || result.Summary != "夜晚城市街道" {
		t.Fatalf("unexpected provider result: result=%+v raw=%q", result, raw)
	}
	legacy := result.ToStructuredAnalysis()
	if legacy.AssetType != "b_roll" || legacy.PeopleCount != 2 || !slices.Contains(legacy.ExtraTags, "walking") {
		t.Fatalf("unified result did not map to legacy analysis: %+v", legacy)
	}
}

func TestDecodeAnalysisPreservesLegacyFlatResponse(t *testing.T) {
	text := `{"asset_type":"architecture","scene_tags":["city"],"summary":"旧格式响应","quality":"usable"}`
	result, _, err := decodeAnalysis(text, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if result.Analysis.AssetType != "architecture" || result.Analysis.Quality != "usable" || result.Summary != "旧格式响应" {
		t.Fatalf("legacy response was not preserved: %+v", result)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
