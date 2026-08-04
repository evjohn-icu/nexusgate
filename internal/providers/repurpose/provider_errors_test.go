package repurpose

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

func TestPlanErrorPaths(t *testing.T) {
	const apiKey = "repurpose-key-errtest"

	tests := []struct {
		name         string
		handler      http.HandlerFunc
		wantStatus   int // expected HTTPStatusCode from *common.StatusError, 0 = not a StatusError
		wantContains string
	}{
		{
			name: "4xx forbidden",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("access denied"))
			},
			wantStatus:   403,
			wantContains: "access denied",
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
			name: "malformed JSON with 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("not json"))
			},
			wantContains: "missing choices",
		},
		{
			name: "empty content in response",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""}}]}`))
			},
			wantContains: "empty repurpose planner response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			provider := &Provider{
				Endpoint:  common.Endpoint{BaseURL: server.URL, APIKey: apiKey},
				ModelName: "small-model",
			}
			_, err := provider.Plan(context.Background(), domain.RepurposeBrief{Brief: "test brief"})
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
