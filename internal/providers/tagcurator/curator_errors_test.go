package tagcurator

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

func TestCuratorErrorPaths(t *testing.T) {
	const apiKey = "curator-key-errtest"

	unresolved := []domain.UnresolvedTag{
		{NormalizedTag: "城市夜景", AssetCount: 2},
	}

	tests := []struct {
		name         string
		handler      http.HandlerFunc
		wantStatus   int // expected HTTPStatusCode from *common.StatusError, 0 = not a StatusError
		wantContains string
	}{
		{
			name: "4xx bad request",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("invalid request body"))
			},
			wantStatus:   400,
			wantContains: "invalid request body",
		},
		{
			name: "5xx service unavailable",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("model not ready"))
			},
			wantStatus:   503,
			wantContains: "model not ready",
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
				_, _ = w.Write([]byte("not valid json"))
			},
			wantContains: "decode tag curator response",
		},
		{
			name: "empty content in response",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""}}]}`))
			},
			wantContains: "empty tag curator response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			provider := &Provider{
				Endpoint:  common.Endpoint{BaseURL: server.URL, APIKey: apiKey},
				ModelName: "tiny-model",
			}
			_, err := provider.Curate(context.Background(), unresolved, nil)
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
