package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// TestProviderChannelEmbeddingEndpointRunsOneInference is the discriminating
// check for POST /api/v1/admin/provider-channels/{id}/test-embedding: the
// route resolves the persisted channel, makes exactly one POST
// {endpoint}/embeddings with the stored key and returns the diagnostic shape
// (status / dimensions / message). The fixture fails anything that is not that
// one call, so a second request — or a request to another path — is a test
// failure. No vector and no key may appear in the response body.
func TestProviderChannelEmbeddingEndpointRunsOneInference(t *testing.T) {
	const probeKey = "{{SECRET_ebdapi}}"
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/embeddings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+probeKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid api key ` + probeKey + `"}`))
			return
		}
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Model != "api-embed-model" || len(body.Input) != 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"unexpected request"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.5,0.25,-0.125]}]}`))
	}))
	defer server.Close()

	service := providerChannelTestService(t, "provider-channel-embedding-endpoint.db")
	handler := NewServer("", service).Handler()
	token := "Bearer " + service.AdminToken()

	createBody := `{"capability":"embedding","label":"Embedding probe","provider_name":"openai_embeddings","endpoint":"` + server.URL + `","model":"api-embed-model","enabled":true,"members":[{"label":"probe","api_key":"` + probeKey + `","enabled":true,"weight":1,"max_inflight":1}]}`
	create := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels", strings.NewReader(createBody))
	req.Header.Set("Authorization", token)
	handler.ServeHTTP(create, req)
	if create.Code != http.StatusCreated {
		t.Fatalf("create channel status=%d body=%s", create.Code, create.Body.String())
	}
	var channel domain.ProviderChannel
	if err := json.Unmarshal(create.Body.Bytes(), &channel); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels/"+channel.ID+"/test-embedding", nil)
	req.Header.Set("Authorization", token)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("test-embedding status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result struct {
		ChannelID    string `json:"channel_id"`
		ProviderName string `json:"provider_name"`
		Model        string `json:"model"`
		Status       string `json:"status"`
		Dimensions   int    `json:"dimensions"`
		LatencyMS    int64  `json:"latency_ms"`
		Message      string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode embedding result: %v\nbody=%s", err, rec.Body.String())
	}
	if result.ChannelID != channel.ID || result.Model != "api-embed-model" {
		t.Fatalf("result=%+v; want the addressed channel and its model", result)
	}
	if result.Status != "ok" || result.Dimensions != 3 {
		t.Fatalf("status=%q dimensions=%d; want ok/3", result.Status, result.Dimensions)
	}
	if result.Message == "" || result.LatencyMS < 0 {
		t.Fatalf("result=%+v; want a message and a non-negative latency", result)
	}
	if calls != 1 {
		t.Fatalf("provider calls=%d; want exactly 1", calls)
	}
	body := rec.Body.String()
	for _, secret := range []string{probeKey, "Bearer " + probeKey, "0.5", "embedding\":["} {
		if strings.Contains(body, secret) {
			t.Fatalf("test-embedding response contains %q: %s", secret, body)
		}
	}
}

// TestProviderChannelEmbeddingEndpointReportsUnsupported pins that a channel
// of another capability answers with a diagnostic 200 instead of a rejection:
// the route works, the channel just cannot be probed for vectors.
func TestProviderChannelEmbeddingEndpointReportsUnsupported(t *testing.T) {
	service := providerChannelTestService(t, "provider-channel-embedding-endpoint-unsupported.db")
	handler := NewServer("", service).Handler()
	token := "Bearer " + service.AdminToken()

	createBody := `{"capability":"tag_curator","label":"Chat channel","provider_name":"openai_chat","endpoint":"https://example.invalid","model":"chat-model","enabled":true,"members":[{"label":"probe","api_key":"chat-key","enabled":true,"weight":1,"max_inflight":1}]}`
	create := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels", strings.NewReader(createBody))
	req.Header.Set("Authorization", token)
	handler.ServeHTTP(create, req)
	if create.Code != http.StatusCreated {
		t.Fatalf("create channel status=%d body=%s", create.Code, create.Body.String())
	}
	var channel domain.ProviderChannel
	if err := json.Unmarshal(create.Body.Bytes(), &channel); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels/"+channel.ID+"/test-embedding", nil)
	req.Header.Set("Authorization", token)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s; want 200 with a diagnostic status", rec.Code, rec.Body.String())
	}
	var result struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "unsupported" || result.Message == "" {
		t.Fatalf("result=%+v; want unsupported with a message", result)
	}
}

// TestProviderChannelEmbeddingEndpointRequiresHubAdmin pins that the billed
// route sits behind the same admin gate as every other provider-channel route,
// so an unauthenticated caller cannot spend an operator's provider credits.
func TestProviderChannelEmbeddingEndpointRequiresHubAdmin(t *testing.T) {
	service := providerChannelTestService(t, "provider-channel-embedding-endpoint-auth.db")
	handler := NewServer("", service).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels/some-id/test-embedding", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated test-embedding status=%d body=%s; want 401/403", rec.Code, rec.Body.String())
	}
}
