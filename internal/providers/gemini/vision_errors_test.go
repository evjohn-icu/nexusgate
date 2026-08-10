package gemini

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

func TestVisionErrorPaths(t *testing.T) {
	const apiKey = "gemini-key-errtest"

	tests := []struct {
		name         string
		handler      http.HandlerFunc
		wantStatus   int // expected HTTPStatusCode from *common.StatusError, 0 = not a StatusError
		wantContains string
	}{
		{
			name: "4xx unauthorized",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("invalid key"))
			},
			wantStatus:   401,
			wantContains: "invalid key",
		},
		{
			name: "5xx internal server error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("backend failure"))
			},
			wantStatus:   500,
			wantContains: "backend failure",
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
			wantContains: "invalid Gemini response",
		},
		{
			name: "empty candidates",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"candidates":[]}`))
			},
			wantContains: "invalid Gemini response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			provider := &Vision{
				Endpoint:  common.Endpoint{BaseURL: server.URL, APIKey: apiKey, AuthHeader: "x-goog-api-key", AuthScheme: "raw"},
				ModelName: "gemini-test",
			}
			// Use a remote URI so we skip the local file read.
			_, _, err := provider.Analyze(context.Background(), common.AnalyzeRequest{
				RemoteURI: "files/test",
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
