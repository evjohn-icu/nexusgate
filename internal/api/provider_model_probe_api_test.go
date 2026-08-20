package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProbeProviderModelListEndpointReturnsListing drives the one-click
// model-list read over the real Handler(): an authenticated Hub admin POSTs
// the still-open dialog's endpoint and key, the Hub makes one non-billed GET
// {endpoint}/models, and the route returns the model ids with status ok. The
// fixture refuses anything but the exact Bearer key and echoes the key back as
// a model id, so the response must prove both that the key was used and that
// it never appears in the result.
func TestProbeProviderModelListEndpointReturnsListing(t *testing.T) {
	const probeKey = "probe-api-secret-77"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+probeKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// The second id echoes the key: it must be redacted out of the result.
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3.7-max"},{"id":"` + probeKey + `"},{"id":"glm-5.2"}]}`))
	}))
	defer server.Close()

	service := providerChannelTestService(t, "probe-models-api.db")
	handler := NewServer("", service).Handler()
	token := "Bearer " + service.AdminToken()

	body := `{"provider_name":"openai_chat","endpoint":"` + server.URL + `","api_key":"` + probeKey + `"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels/probe-models", strings.NewReader(body))
	req.Header.Set("Authorization", token)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe status=%d body=%s", rec.Code, rec.Body.String())
	}
	response := rec.Body.String()
	if strings.Contains(response, probeKey) {
		t.Fatalf("probe response leaked the api key: %s", response)
	}
	var result struct {
		Status string   `json:"status"`
		Models []string `json:"models"`
	}
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		t.Fatalf("unmarshal probe response: %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("status=%q, want ok", result.Status)
	}
	if len(result.Models) != 3 {
		t.Fatalf("models=%v, want 3 (key echo stays as [REDACTED], never the key itself)", result.Models)
	}
	if result.Models[0] != "qwen3.7-max" {
		t.Fatalf("models[0]=%q, want qwen3.7-max", result.Models[0])
	}
	if result.Models[1] != "[REDACTED]" {
		t.Fatalf("models[1]=%q, want the key-echo redacted to [REDACTED]", result.Models[1])
	}
}

// TestProbeProviderModelListEndpointRequiresHubAdmin pins that the new route
// carries the same admin gate as every other /api/v1/admin/provider-channels
// route. The key rides in this request body, so an unauthenticated probe must
// be refused before the handler can do anything with it.
func TestProbeProviderModelListEndpointRequiresHubAdmin(t *testing.T) {
	service := providerChannelTestService(t, "probe-models-auth.db")
	handler := NewServer("", service).Handler()

	body := `{"provider_name":"openai_chat","endpoint":"https://example.invalid","api_key":"secret"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels/probe-models", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated probe status=%d body=%s; want 401/403", rec.Code, rec.Body.String())
	}
}
