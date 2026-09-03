package gemini

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	videoanalysis "github.com/evjohn-icu/nexusslate/internal/domain/video_analysis"
	"github.com/evjohn-icu/nexusslate/internal/providers/common"
)

func assertGeminiStatus(t *testing.T, err error, key string, status int) {
	t.Helper()
	var se *common.StatusError
	if !errors.As(err, &se) || se.HTTPStatusCode() != status {
		t.Fatalf("expected status %d, got %T: %v", status, err, err)
	}
	if strings.Contains(se.Body, key) || strings.Contains(se.Error(), key) || len(se.Body) > 2048+len("…(truncated)") {
		t.Fatalf("status error leaked or was not bounded: body=%q error=%q", se.Body, se.Error())
	}
}

func TestPrepareVideoErrorFixtures(t *testing.T) {
	const key = "gemini-upload-key"
	video := filepath.Join(t.TempDir(), "fixture.mp4")
	if err := os.WriteFile(video, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"upload start", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(502)
			_, _ = w.Write([]byte(strings.Repeat("start ", 500) + key))
		}},
		{"upload finalize", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Goog-Upload-Command") == "start" {
				w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/finalize")
				w.WriteHeader(200)
				return
			}
			w.WriteHeader(502)
			_, _ = w.Write([]byte(strings.Repeat("finalize ", 500) + key))
		}},
		{"file state", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Goog-Upload-Command") == "start" {
				w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/finalize")
				w.WriteHeader(200)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/finalize") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"file":{"name":"files/x","uri":"gs://x","state":"PROCESSING"}}`))
				return
			}
			w.WriteHeader(502)
			_, _ = w.Write([]byte(strings.Repeat("state ", 500) + key))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(tc.handler)
			defer s.Close()
			_, err := (&Vision{Endpoint: common.Endpoint{BaseURL: s.URL, APIKey: key, AuthHeader: "x-goog-api-key"}}).PrepareVideo(context.Background(), common.PrepareVideoRequest{VideoPath: video, MIMEType: "video/mp4"})
			if err == nil {
				t.Fatal("expected error")
			}
			assertGeminiStatus(t, err, key, 502)
		})
	}
}

func TestMalformedGeminiSuccessResponsesRedact(t *testing.T) {
	const key = "gemini-malformed-key"
	video := filepath.Join(t.TempDir(), "fixture.mp4")
	if err := os.WriteFile(video, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"gemini_generate_content", "openai_chat"} {
		t.Run(protocol, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("bad ", 600) + key)) }))
			defer s.Close()
			_, raw, err := (&Vision{Endpoint: common.Endpoint{BaseURL: s.URL, APIKey: key}, Protocol: protocol}).AnalyzeVideo(context.Background(), videoanalysis.Input{VideoPath: video})
			if err == nil || strings.Contains(err.Error(), key) || strings.Contains(raw, key) || len(err.Error()) > 2048+128 {
				t.Fatalf("malformed response leaked or was not bounded: err=%v raw-len=%d", err, len(raw))
			}
		})
	}
}
