package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// TestProviderChannelTestEndpointReturnsExtendedShape is the discriminating
// check for POST /api/v1/admin/provider-channels/{id}/test: after the
// reachability GET, the Hub resolves the stored key and makes one non-billed
// GET {endpoint}/models against the fixture, and the route returns the
// extended shape (model_responded / model_name / schema_ok) alongside the
// original fields. The fixture proves the key was actually used by refusing
// anything but the exact Bearer value stored in the secret store. The test
// drives the real Handler() as an authenticated Hub admin, exactly like the
// provider-channel runtime status tests.
func TestProviderChannelTestEndpointReturnsExtendedShape(t *testing.T) {
	const probeKey = "probe-secret-key-42"
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
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"api-probe-model"},{"id":"api-probe-model-2"}]}`))
	}))
	defer server.Close()

	service := providerChannelTestService(t, "provider-channel-test-endpoint.db")
	handler := NewServer("", service).Handler()
	token := "Bearer " + service.AdminToken()

	createBody := `{"capability":"tag_curator","label":"Probe channel","provider_name":"openai_chat","endpoint":"` + server.URL + `","model":"probe-model","enabled":true,"members":[{"label":"probe","api_key":"` + probeKey + `","enabled":true,"weight":1,"max_inflight":1}]}`
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
	if channel.ID == "" {
		t.Fatal("create did not return a channel id")
	}

	rec := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels/"+channel.ID+"/test", nil)
	req.Header.Set("Authorization", token)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("test channel status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result struct {
		ChannelID      string `json:"channel_id"`
		Status         string `json:"status"`
		SecretReady    bool   `json:"secret_ready"`
		Reachable      bool   `json:"reachable"`
		HTTPStatus     int    `json:"http_status"`
		LatencyMS      int64  `json:"latency_ms"`
		Message        string `json:"message"`
		ModelResponded bool   `json:"model_responded"`
		ModelName      string `json:"model_name"`
		SchemaOK       bool   `json:"schema_ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode test result: %v\nbody=%s", err, rec.Body.String())
	}
	if result.ChannelID != channel.ID {
		t.Fatalf("channel_id=%q; want %q", result.ChannelID, channel.ID)
	}
	if !result.SecretReady || !result.Reachable || result.Status != "reachable" {
		t.Fatalf("secret_ready=%v reachable=%v status=%q; want reachable channel: %+v", result.SecretReady, result.Reachable, result.Status, result)
	}
	if !result.ModelResponded || !result.SchemaOK {
		t.Fatalf("model_responded=%v schema_ok=%v; want both true: %+v", result.ModelResponded, result.SchemaOK, result)
	}
	if result.ModelName != "api-probe-model" {
		t.Fatalf("model_name=%q; want api-probe-model", result.ModelName)
	}
	if result.HTTPStatus != http.StatusOK {
		t.Fatalf("http_status=%d; want 200", result.HTTPStatus)
	}
	if result.LatencyMS < 0 {
		t.Fatalf("latency_ms=%d; want non-negative", result.LatencyMS)
	}
	body := rec.Body.String()
	for _, secret := range []string{probeKey, "Bearer " + probeKey} {
		if strings.Contains(body, secret) {
			t.Fatalf("test route response leaked a provider key: contains %q\nbody=%s", secret, body)
		}
	}
}

// TestProviderChannelTestEndpointRequiresHubAdmin pins that the /test route
// sits behind the same admin gate as every other provider-channel route
// rather than accidentally landing on a public or worker-scoped path.
func TestProviderChannelTestEndpointRequiresHubAdmin(t *testing.T) {
	service := providerChannelTestService(t, "provider-channel-test-endpoint-auth.db")
	handler := NewServer("", service).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels/some-id/test", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated test request status=%d body=%s; want 401/403", rec.Code, rec.Body.String())
	}
}
