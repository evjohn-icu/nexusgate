package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// modelProbeFixture is a minimal OpenAI-compatible /models fixture. It refuses
// any request that does not carry the exact expected Bearer key, so a test can
// prove the key rode in the Authorization header and nowhere in the result.
type modelProbeFixture struct {
	t       *testing.T
	key     string
	status  int
	body    string
	calls   int
	path    string
	missing bool
}

func (f *modelProbeFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls++
	if f.missing {
		http.NotFound(w, r)
		return
	}
	if f.path != "" && r.URL.Path != f.path {
		http.NotFound(w, r)
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+f.key {
		f.t.Errorf("probe Authorization = %q, want Bearer %q", got, f.key)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.status)
	_, _ = w.Write([]byte(f.body))
}

func TestProbeProviderModelListReturnsFullListing(t *testing.T) {
	const secret = "probe-list-secret"
	fx := &modelProbeFixture{t: t, key: secret, status: http.StatusOK,
		body: `{"data":[{"id":"qwen3.7-max"},{"id":"qwen3.7-plus"},{"id":"qwen3.7-max"},{"id":"  glm-5.2 "}]}`}
	server := httptest.NewServer(fx)
	defer server.Close()

	result, err := (&Service{}).ProbeProviderModelList(context.Background(), "openai_chat", server.URL, secret)
	if err != nil {
		t.Fatalf("ProbeProviderModelList: %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("status=%q message=%q, want ok", result.Status, result.Message)
	}
	want := []string{"qwen3.7-max", "qwen3.7-plus", "glm-5.2"}
	if len(result.Models) != len(want) {
		t.Fatalf("models=%v, want %v (deduped and trimmed)", result.Models, want)
	}
	for i, m := range want {
		if result.Models[i] != m {
			t.Fatalf("models[%d]=%q, want %q", i, result.Models[i], m)
		}
	}
	if fx.calls != 1 {
		t.Fatalf("fixture calls=%d, want 1", fx.calls)
	}
}

func TestProbeProviderModelListRedactsKeyFromModelIDs(t *testing.T) {
	// A relay that echoes the key back as a model id must not smuggle it to
	// the page: every id is redacted against the key before it is returned.
	const secret = "probe-redact-secret"
	fx := &modelProbeFixture{t: t, key: secret, status: http.StatusOK,
		body: fmt.Sprintf(`{"data":[{"id":"legit"},{"id":"%s"},{"id":"prefix-%s-suffix"}]}`, secret, secret)}
	server := httptest.NewServer(fx)
	defer server.Close()

	result, err := (&Service{}).ProbeProviderModelList(context.Background(), "openai_chat", server.URL, secret)
	if err != nil {
		t.Fatalf("ProbeProviderModelList: %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("status=%q, want ok", result.Status)
	}
	for _, m := range result.Models {
		if strings.Contains(m, secret) {
			t.Fatalf("model id %q leaked the key", m)
		}
	}
	foundRedacted := false
	for _, m := range result.Models {
		if strings.Contains(m, "[REDACTED]") {
			foundRedacted = true
		}
	}
	if !foundRedacted {
		t.Fatalf("models=%v, want at least one id with the key redacted", result.Models)
	}
}

func TestProbeProviderModelListUnprobeableProvidersMakeNoCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unprobeable provider must not reach the network, got %s", r.URL.Path)
	}))
	defer server.Close()
	service := &Service{}
	for _, name := range []string{"gemini", "gemini_embed_content", "volcengine_asr"} {
		result, err := service.ProbeProviderModelList(context.Background(), name, server.URL, "any-key")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.Status != "unprobeable" {
			t.Fatalf("%s: status=%q, want unprobeable", name, result.Status)
		}
		if len(result.Models) != 0 {
			t.Fatalf("%s: models=%v, want none", name, result.Models)
		}
	}
}

func TestProbeProviderModelListGuardsInputs(t *testing.T) {
	service := &Service{}
	ctx := context.Background()
	cases := []struct {
		name, provider, endpoint, key string
		wantStatus                    string
	}{
		{"empty key", "openai_chat", "https://example.invalid", "  ", "no_key"},
		{"missing endpoint", "openai_chat", "", "key", "invalid_endpoint"},
		{"non-http scheme", "openai_chat", "ftp://example.invalid/v1", "key", "invalid_endpoint"},
		{"no host", "openai_chat", "https://", "key", "invalid_endpoint"},
		{"unknown provider still probes", "openai_chat", "not a url", "key", "invalid_endpoint"},
	}
	for _, tc := range cases {
		result, err := service.ProbeProviderModelList(ctx, tc.provider, tc.endpoint, tc.key)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if result.Status != tc.wantStatus {
			t.Fatalf("%s: status=%q, want %q", tc.name, result.Status, tc.wantStatus)
		}
	}
}

func TestProbeProviderModelListKeyInvalidAndEmptyAndSchema(t *testing.T) {
	const secret = "probe-http-secret"
	service := &Service{}
	ctx := context.Background()

	// 401 means the endpoint answered and the key was rejected.
	fx401 := &modelProbeFixture{t: t, key: secret, status: http.StatusUnauthorized, body: `{"error":"bad key"}`}
	s401 := httptest.NewServer(fx401)
	result, err := service.ProbeProviderModelList(ctx, "openai_chat", s401.URL, secret)
	s401.Close()
	if err != nil {
		t.Fatalf("401 case: %v", err)
	}
	if result.Status != "key_invalid" || result.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("401 case: status=%q http=%d, want key_invalid/401", result.Status, result.HTTPStatus)
	}

	// 200 with an empty listing is a valid endpoint that has no models.
	fxEmpty := &modelProbeFixture{t: t, key: secret, status: http.StatusOK, body: `{"data":[]}`}
	sEmpty := httptest.NewServer(fxEmpty)
	result, err = service.ProbeProviderModelList(ctx, "openai_chat", sEmpty.URL, secret)
	sEmpty.Close()
	if err != nil {
		t.Fatalf("empty case: %v", err)
	}
	if result.Status != "no_models" {
		t.Fatalf("empty case: status=%q, want no_models", result.Status)
	}

	// 404 means the endpoint is reachable but exposes no model list.
	fx404 := &modelProbeFixture{t: t, key: secret, status: http.StatusNotFound, body: `not found`}
	s404 := httptest.NewServer(fx404)
	result, err = service.ProbeProviderModelList(ctx, "openai_chat", s404.URL, secret)
	s404.Close()
	if err != nil {
		t.Fatalf("404 case: %v", err)
	}
	if result.Status != "schema_unknown" {
		t.Fatalf("404 case: status=%q, want schema_unknown", result.Status)
	}
}

func TestProbeProviderModelListCapsListingSize(t *testing.T) {
	const secret = "probe-cap-secret"
	const total = maxProbeModelCount + 50
	ids := make([]any, 0, total)
	for i := range total {
		ids = append(ids, map[string]any{"id": fmt.Sprintf("model-%03d", i)})
	}
	payload, _ := json.Marshal(map[string]any{"data": ids})
	fx := &modelProbeFixture{t: t, key: secret, status: http.StatusOK, body: string(payload)}
	server := httptest.NewServer(fx)
	defer server.Close()

	result, err := (&Service{}).ProbeProviderModelList(context.Background(), "openai_chat", server.URL, secret)
	if err != nil {
		t.Fatalf("ProbeProviderModelList: %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("status=%q, want ok", result.Status)
	}
	if len(result.Models) != maxProbeModelCount {
		t.Fatalf("models=%d, want capped at %d", len(result.Models), maxProbeModelCount)
	}
}
