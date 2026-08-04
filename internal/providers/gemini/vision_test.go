package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

func TestAnalyzeNativeFixtureUsesRawGeminiKey(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1beta/models/gemini-test:generateContent" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("x-goog-api-key"); got != "gemini-key" {
			t.Fatalf("x-goog-api-key=%q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		analysis := `{"asset_type":"b_roll","scene_tags":["urban_night"],"subjects":["street"],"people_count":0,"shot_size":"wide","camera_motion":"static","lighting":"night","audio_type":"ambient","has_speech":false,"summary":"城市夜景旧素材","usable_as":["montage"],"mood_tags":["calm"],"quality":"usable","quality_flags":[],"extra_tags":[],"editorial_reason":"可作过场"}`
		response, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": analysis}}}}}})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(response)), Header: make(http.Header), Request: r}, nil
	})}

	provider := &Vision{
		Endpoint:  common.Endpoint{BaseURL: "http://provider.invalid/v1beta", APIKey: "gemini-key", AuthHeader: "x-goog-api-key", AuthScheme: "raw", HTTPClient: client},
		ModelName: "gemini-test",
	}
	analysis, _, err := provider.Analyze(context.Background(), common.AnalyzeRequest{RemoteURI: "files/test", MIMEType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Summary != "城市夜景旧素材" || len(analysis.SceneTags) != 1 {
		t.Fatalf("analysis=%+v", analysis)
	}
}

func TestPrepareVideoResumableFixture(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("x-goog-api-key"); got != "gemini-key" {
			t.Fatalf("x-goog-api-key=%q", got)
		}
		switch r.URL.Path {
		case "/upload/v1beta/files":
			if r.Header.Get("X-Goog-Upload-Command") != "start" {
				t.Fatalf("upload command=%q", r.Header.Get("X-Goog-Upload-Command"))
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Goog-Upload-Url": []string{"http://provider.invalid/upload-session"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		case "/upload-session":
			data, _ := io.ReadAll(r.Body)
			if string(data) != "video-fixture" {
				t.Fatalf("uploaded=%q", string(data))
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"file":{"name":"files/abc","uri":"https://files.example/abc","mimeType":"video/mp4","state":"ACTIVE"}}`)), Request: r}, nil
		default:
			t.Fatalf("unexpected path=%q", r.URL.Path)
		}
		return nil, nil
	})}

	videoPath := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(videoPath, []byte("video-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &Vision{
		Endpoint: common.Endpoint{
			BaseURL: "http://provider.invalid/v1beta", APIKey: "gemini-key",
			AuthHeader: "x-goog-api-key", AuthScheme: "raw", HTTPClient: client,
		},
		ModelName: "gemini-test",
	}
	prepared, err := provider.PrepareVideo(context.Background(), common.PrepareVideoRequest{
		VideoPath: videoPath, DisplayName: "clip.mp4", MIMEType: "video/mp4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.State != "ACTIVE" || prepared.RemoteName != "files/abc" {
		t.Fatalf("prepared=%+v", prepared)
	}
}
