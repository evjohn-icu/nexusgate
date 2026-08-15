package stepfun

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/providers/common"
	"github.com/evjohn-icu/timingdex/internal/testhelper"
)

func TestASRErrorPaths(t *testing.T) {
	const apiKey = "step-key-errtest"

	// Fake ffmpeg that returns minimal PCM data so the adapter proceeds past the
	// conversion step and reaches the HTTP call.
	binDir := t.TempDir()
	testhelper.InstallCommand(t, binDir, "ffmpeg")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	audioPath := filepath.Join(t.TempDir(), "sample.m4a")
	if err := os.WriteFile(audioPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		handler      http.HandlerFunc
		wantStatus   int    // expected HTTPStatusCode from *common.StatusError, 0 = not a StatusError
		wantContains string // expected substring in err.Error()
	}{
		{
			name: "4xx forbidden",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("forbidden"))
			},
			wantStatus:   403,
			wantContains: "forbidden",
		},
		{
			name: "5xx service unavailable",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("upstream failure"))
			},
			wantStatus:   503,
			wantContains: "upstream failure",
		},
		{
			name: "SSE error event",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("data: {\"type\":\"error\",\"message\":\"model overloaded\"}\n\n"))
			},
			wantContains: "stepfun SSE error: model overloaded",
		},
		{
			name: "SSE error event bounds echoed secret",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`data: {"type":"error","message":"` + strings.Repeat("diagnostic ", 300) + apiKey + `"}` + "\n\n"))
			},
			wantContains: "stepfun SSE error: diagnostic",
		},
		{
			name: "SSE no transcript text",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				// Send a delta with empty text, then [DONE] - no text should be extracted.
				_, _ = w.Write([]byte("data: {\"type\":\"transcript.text.delta\",\"delta\":\"\"}\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			},
			wantContains: "contained no transcript text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			provider := &ASR{
				Endpoint:  common.Endpoint{BaseURL: server.URL, APIKey: apiKey},
				ModelName: "stepaudio-2.5-asr",
			}
			_, err := provider.Transcribe(context.Background(), common.TranscribeRequest{AudioPath: audioPath, Language: "zh"})
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
