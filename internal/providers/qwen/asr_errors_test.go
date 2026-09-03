package qwen

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/providers/common"
)

func TestASRErrorPaths(t *testing.T) {
	const apiKey = "qwen-key-errtest"

	tests := []struct {
		name          string
		handler       http.HandlerFunc
		wantStatus    int    // expected HTTPStatusCode from *common.StatusError, 0 = not a StatusError
		wantContains  string // expected substring in err.Error()
		skipAudioFile bool   // if true, don't create a wav fixture
	}{
		{
			name: "4xx bad request",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("invalid audio format"))
			},
			wantStatus:   400,
			wantContains: "invalid audio format",
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
			wantContains: "decode",
		},
		{
			name: "empty choices",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"choices":[]}`))
			},
			wantContains: "no choices",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			audioPath := filepath.Join(t.TempDir(), "sample.wav")
			if err := os.WriteFile(audioPath, []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}

			provider := &ASR{
				Endpoint:  common.Endpoint{BaseURL: server.URL, APIKey: apiKey},
				ModelName: "qwen3-asr-flash",
			}
			_, err := provider.Transcribe(context.Background(), common.TranscribeRequest{AudioPath: audioPath, Language: "zh"})
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			// Verify *common.StatusError for 4xx/5xx.
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

			// Verify error text contains expected substring.
			if tt.wantContains != "" {
				errStr := err.Error()
				if !strings.Contains(errStr, tt.wantContains) {
					t.Fatalf("error text = %q, want it to contain %q", errStr, tt.wantContains)
				}
			}

			// Verify error text does NOT leak API key.
			if strings.Contains(err.Error(), apiKey) {
				t.Fatalf("error text leaks API key: %q", err.Error())
			}
		})
	}
}
