package embedding

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/providers/common"
)

func TestEmbedErrorPaths(t *testing.T) {
	const apiKey = "embed-key-errtest"

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
				_, _ = w.Write([]byte("unauthorized"))
			},
			wantStatus:   401,
			wantContains: "unauthorized",
		},
		{
			name: "5xx internal server error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("server error"))
			},
			wantStatus:   500,
			wantContains: "server error",
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
				_, _ = w.Write([]byte("not json"))
			},
			wantContains: "decode embeddings",
		},
		{
			name: "response count mismatch",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				// Request sends 2 inputs, but response has only 1 embedding.
				_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
			},
			wantContains: "does not match request count",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			provider := &Provider{
				Endpoint:  common.Endpoint{BaseURL: server.URL, APIKey: apiKey},
				ModelName: "embed-test",
			}
			_, err := provider.Embed(context.Background(), []string{"night city", "urban night"})
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
