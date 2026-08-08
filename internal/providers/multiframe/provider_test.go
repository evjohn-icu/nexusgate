package multiframe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
	videoproviders "github.com/evjohn-icu/timingdex/internal/providers/video"
)

func writeFrame(t *testing.T, dir string, name string, ts int64) videoanalysis.Frame {
	t.Helper()
	path := filepath.Join(dir, name)
	// A tiny real JPEG header; the endpoint is faked, so the bytes only need
	// to survive base64 round-tripping.
	if err := os.WriteFile(path, []byte("\xff\xd8\xff\xe0fakejpeg\xff\xd9"), 0o600); err != nil {
		t.Fatal(err)
	}
	return videoanalysis.Frame{Path: path, TimestampMS: ts}
}

// fakeEndpoint serves one OpenAI-compatible chat/completions handler that
// records the request body for assertions.
func fakeEndpoint(t *testing.T, reply string, inspect func(body map[string]any)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", contentType)
		}
		// llama.cpp and LM Studio run without an API key: the adapter must not
		// invent an Authorization header for a keyless endpoint.
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("unexpected Authorization header %q on a keyless endpoint", auth)
		}
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("request body is not JSON: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if inspect != nil {
			inspect(decoded)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestAnalyzeShotRequestShape(t *testing.T) {
	dir := t.TempDir()
	frames := []videoanalysis.Frame{
		writeFrame(t, dir, "f0.jpg", 400),
		writeFrame(t, dir, "f1.jpg", 1400),
		writeFrame(t, dir, "f2.jpg", 2600),
	}
	var seen map[string]any
	server := fakeEndpoint(t, `{"choices":[{"message":{"content":"{\"description\":\"a person walking\",\"objects\":[\"person\"],\"actions\":[\"walking\"],\"mood\":[\"calm\"],\"tags\":[\"street\"],\"shot_size\":\"wide\",\"camera_motion\":\"pan_left\",\"quality\":\"usable\",\"usable_as\":[\"establishing\"],\"confidence\":0.9}"}}]}`, func(body map[string]any) {
		seen = body
	})
	p := &Provider{ProviderName: "local_vlm", Endpoint: common.Endpoint{BaseURL: server.URL + "/v1", HTTPClient: server.Client()}, ModelName: "Qwen3-VL-4B-Instruct"}
	metadata, raw, err := p.AnalyzeShot(context.Background(), videoproviders.ShotAnalysisRequest{
		Frames:      frames,
		ShotStartMS: 0,
		ShotEndMS:   4000,
		Transcript:  &domain.Transcript{Text: "hello there", Segments: []domain.TranscriptSegment{{StartMS: 500, EndMS: 1200, Text: "hello there"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatal("raw response not recorded")
	}
	if metadata.Description != "a person walking" || len(metadata.Objects) != 1 || metadata.ShotSize != "wide" || metadata.CameraMotion != "pan_left" || metadata.Quality != "usable" || len(metadata.UsableAs) != 1 || metadata.Confidence != 0.9 {
		t.Fatalf("unexpected metadata: %+v", metadata)
	}
	if p.Model() != "Qwen3-VL-4B-Instruct" || p.Name() != "local_vlm" {
		t.Fatalf("identity: %s/%s", p.Name(), p.Model())
	}
	if seen == nil {
		t.Fatal("request not inspected")
	}
	messages, ok := seen["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages shape: %v", seen["messages"])
	}
	content, ok := messages[0].(map[string]any)["content"].([]any)
	if !ok || len(content) != 4 {
		t.Fatalf("content shape: got %d parts, want text + 3 images", len(content))
	}
	text, ok := content[0].(map[string]any)["text"].(string)
	if !ok || !strings.Contains(text, "400ms") || !strings.Contains(text, "2600ms") {
		t.Errorf("prompt does not state frame timestamps: %q", text)
	}
	if !strings.Contains(text, "0ms to 4000ms") {
		t.Errorf("prompt does not state the shot window: %q", text)
	}
	if !strings.Contains(text, "hello there") {
		t.Errorf("prompt does not carry the transcript slice: %q", text)
	}
	if !strings.Contains(text, "any time fields") {
		t.Errorf("prompt does not forbid timeline fields: %q", text)
	}
	for i := 1; i < 4; i++ {
		part, ok := content[i].(map[string]any)
		if !ok || part["type"] != "image_url" {
			t.Fatalf("content part %d is not image_url: %v", i, part)
		}
		url, ok := part["image_url"].(map[string]any)["url"].(string)
		if !ok || !strings.HasPrefix(url, "data:image/jpeg;base64,") {
			t.Fatalf("content part %d url is not a jpeg data URL: %v", i, part)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, "data:image/jpeg;base64,"))
		if err != nil || len(decoded) == 0 {
			t.Fatalf("content part %d data URL does not decode: %v", i, err)
		}
	}
	if responseFormat, ok := seen["response_format"].(map[string]any); !ok || responseFormat["type"] != "json_object" {
		t.Errorf("response_format missing: %v", seen["response_format"])
	}
}

func TestAnalyzeShotRejectsUndecodableOutput(t *testing.T) {
	dir := t.TempDir()
	server := fakeEndpoint(t, `{"choices":[{"message":{"content":"{\"description\":\"\",\"objects\":[]}"}}]}`, nil)
	p := &Provider{Endpoint: common.Endpoint{BaseURL: server.URL + "/v1", HTTPClient: server.Client()}}
	_, _, err := p.AnalyzeShot(context.Background(), videoproviders.ShotAnalysisRequest{
		Frames: []videoanalysis.Frame{writeFrame(t, dir, "f.jpg", 100)},
	})
	if err == nil || !strings.Contains(err.Error(), "description is required") {
		t.Fatalf("got %v", err)
	}
	if !errors.Is(err, domain.ErrPermanentFailure) {
		t.Fatalf("undecodable model output must be permanent, got %v", err)
	}
}

func TestAnalyzeShotNormalizesEnums(t *testing.T) {
	dir := t.TempDir()
	server := fakeEndpoint(t, `{"choices":[{"message":{"content":"{\"description\":\"x\",\"shot_size\":\"close-up\",\"camera_motion\":\"pan left\",\"quality\":\"bogus\"}"}}]}`, nil)
	p := &Provider{Endpoint: common.Endpoint{BaseURL: server.URL + "/v1", HTTPClient: server.Client()}}
	metadata, _, err := p.AnalyzeShot(context.Background(), videoproviders.ShotAnalysisRequest{
		Frames: []videoanalysis.Frame{writeFrame(t, dir, "f.jpg", 100)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ShotSize != "close_up" {
		t.Errorf("shot_size = %q, want close_up", metadata.ShotSize)
	}
	if metadata.CameraMotion != "pan_left" {
		t.Errorf("camera_motion = %q, want pan_left", metadata.CameraMotion)
	}
	if metadata.Quality != "unknown" {
		t.Errorf("quality = %q, want unknown", metadata.Quality)
	}
}

// A field the model did not send must stay empty, not become the vocabulary's
// unknown marker: the aggregation fold counts non-empty answers only, and an
// absent vote must not tip the dominant.
func TestAnalyzeShotAbsentEnumsStayEmpty(t *testing.T) {
	dir := t.TempDir()
	server := fakeEndpoint(t, `{"choices":[{"message":{"content":"{\"description\":\"x\"}"}}]}`, nil)
	p := &Provider{Endpoint: common.Endpoint{BaseURL: server.URL + "/v1", HTTPClient: server.Client()}}
	metadata, _, err := p.AnalyzeShot(context.Background(), videoproviders.ShotAnalysisRequest{
		Frames: []videoanalysis.Frame{writeFrame(t, dir, "f.jpg", 100)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ShotSize != "" || metadata.CameraMotion != "" || metadata.Quality != "" {
		t.Fatalf("absent enums must stay empty: %+v", metadata)
	}
}

func TestAnalyzeSummary(t *testing.T) {
	dir := t.TempDir()
	server := fakeEndpoint(t, `{"choices":[{"message":{"content":"{\"summary\":\"city night\",\"analysis\":{\"asset_type\":\"b_roll\",\"shot_size\":\"wide\",\"summary\":\"city night\"}}"}}]}`, nil)
	p := &Provider{Endpoint: common.Endpoint{BaseURL: server.URL + "/v1", HTTPClient: server.Client()}}
	result, raw, err := p.Analyze(context.Background(), videoanalysis.Input{
		Frames: []videoanalysis.Frame{writeFrame(t, dir, "f.jpg", 100)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || result.Summary != "city night" || result.Analysis.AssetType != "b_roll" {
		t.Fatalf("result: %+v raw=%q", result, raw)
	}
}

func TestAnalyzeSummaryRequiresFrames(t *testing.T) {
	p := &Provider{}
	if _, _, err := p.Analyze(context.Background(), videoanalysis.Input{}); err == nil || !strings.Contains(err.Error(), "requires at least one frame") {
		t.Fatalf("got %v", err)
	}
}

func TestAnalyzeShotRequiresFrames(t *testing.T) {
	p := &Provider{}
	if _, _, err := p.AnalyzeShot(context.Background(), videoproviders.ShotAnalysisRequest{}); err == nil || !strings.Contains(err.Error(), "requires at least one frame") {
		t.Fatalf("got %v", err)
	}
}

func TestProviderSurfacesHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("model is loading"))
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	p := &Provider{Endpoint: common.Endpoint{BaseURL: server.URL + "/v1", HTTPClient: server.Client()}}
	_, _, err := p.AnalyzeShot(context.Background(), videoproviders.ShotAnalysisRequest{
		Frames: []videoanalysis.Frame{writeFrame(t, dir, "f.jpg", 100)},
	})
	var status *common.StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected a status-carrying error, got %v", err)
	}
}
