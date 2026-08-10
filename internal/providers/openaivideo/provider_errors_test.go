package openaivideo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

func TestProviderErrorPaths(t *testing.T) {
	const apiKey = "openai-video-key-errtest"

	videoPath := filepath.Join(t.TempDir(), "fixture.mp4")
	if err := os.WriteFile(videoPath, []byte("video-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		handler      http.HandlerFunc
		wantStatus   int
		wantContains string
	}{
		{
			name: "4xx too many requests",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte("rate limit exceeded"))
			},
			wantStatus:   429,
			wantContains: "rate limit exceeded",
		},
		{
			name: "5xx bad gateway",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte("upstream error"))
			},
			wantStatus:   502,
			wantContains: "upstream error",
		},
		{
			name: "bounded echoed secret",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(strings.Repeat("diagnostic ", 300) + apiKey))
			},
			wantStatus:   502,
			wantContains: "diagnostic",
		},
		{
			name: "malformed JSON with 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("not valid json {{{"))
			},
			wantContains: "missing choices",
		},
		{
			name: "empty semantic fields",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"\"}"}}]}`))
			},
			wantContains: "no semantic fields",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			provider := &Provider{
				ProviderName: "test_openai_video",
				Endpoint:     common.Endpoint{BaseURL: server.URL, APIKey: apiKey},
				ModelName:    "test-video-model",
			}
			_, _, err := provider.Analyze(context.Background(), videoanalysis.Input{
				VideoPath: videoPath,
				MIMEType:  "video/mp4",
			})
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			var se *common.StatusError
			if tt.wantStatus != 0 {
				if !errors.As(err, &se) {
					t.Fatalf("expected *common.StatusError, got %T: %v", err, err)
				}
				if se.HTTPStatusCode() != tt.wantStatus {
					t.Fatalf("HTTPStatusCode() = %d, want %d", se.HTTPStatusCode(), tt.wantStatus)
				}
				if strings.Contains(se.Body, apiKey) || len(se.Body) > 2048+len("…(truncated)") {
					t.Fatalf("status body leaked or was not bounded: len=%d body=%q", len(se.Body), se.Body)
				}
			} else {
				if errors.As(err, &se) {
					t.Fatalf("unexpected *common.StatusError wrapping: %v", err)
				}
			}

			if tt.wantContains != "" && !strings.Contains(err.Error(), tt.wantContains) {
				t.Fatalf("error text = %q, want it to contain %q", err.Error(), tt.wantContains)
			}

			if strings.Contains(err.Error(), apiKey) {
				t.Fatalf("error text leaks API key: %q", err.Error())
			}
		})
	}
}
