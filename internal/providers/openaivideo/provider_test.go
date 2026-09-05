package openaivideo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	videoanalysis "github.com/evjohn-icu/nexusgate/internal/domain/video_analysis"
	"github.com/evjohn-icu/nexusgate/internal/providers/common"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderSendsVideoURLAndDecodesUnifiedResult(t *testing.T) {
	videoPath := filepath.Join(t.TempDir(), "fixture.mp4")
	if err := os.WriteFile(videoPath, []byte("video-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		messages := body["messages"].([]any)
		content := messages[0].(map[string]any)["content"].([]any)
		video := content[1].(map[string]any)["video_url"].(map[string]any)["url"].(string)
		if !strings.HasPrefix(video, "data:video/mp4;base64,") {
			t.Fatalf("video URL=%q", video)
		}
		response := `{"choices":[{"message":{"content":"{\"summary\":\"夜晚城市街道\",\"raw_tags\":[\"urban_night\"],\"shots\":[{\"start_ms\":12000,\"end_ms\":18000,\"description\":\"霓虹街道\",\"tags\":[\"urban_night\"]}] }"}}]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header), Request: r}, nil
	})}
	provider := &Provider{ProviderName: "qwen_video", Endpoint: common.Endpoint{BaseURL: "http://provider.test/v1", HTTPClient: client}, ModelName: "qwen-vl-fixture"}

	result, raw, err := provider.Analyze(context.Background(), videoanalysis.Input{VideoPath: videoPath, MIMEType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || result.Summary != "夜晚城市街道" || len(result.Shots) != 1 || result.Shots[0].StartMS != 12000 {
		t.Fatalf("result=%+v raw=%q", result, raw)
	}
}

func TestProviderUsesRemoteVideoURIWhenAvailable(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		content := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
		video := content[1].(map[string]any)["video_url"].(map[string]any)["url"].(string)
		if video != "https://media.example/clip.mp4" {
			t.Fatalf("video URL=%q", video)
		}
		response := `{"choices":[{"message":{"content":"{\"summary\":\"remote clip\"}"}}]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header), Request: r}, nil
	})}
	provider := &Provider{ProviderName: "volcengine_video", Endpoint: common.Endpoint{BaseURL: "http://provider.test/v1", HTTPClient: client}}
	result, _, err := provider.Analyze(context.Background(), videoanalysis.Input{RemoteURI: "https://media.example/clip.mp4"})
	if err != nil || result.Summary != "remote clip" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
