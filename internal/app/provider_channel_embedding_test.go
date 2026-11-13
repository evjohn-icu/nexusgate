package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/secretstore"
)

// staticChannelRepo serves exactly the channel rows the test hands it and
// embeds the rest of Repository as a nil interface. Any call the embedding
// probe is not supposed to make (an index write, a save, a status update)
// panics instead of quietly passing, which is what makes "this probe persists
// nothing" a real assertion rather than a comment.
type staticChannelRepo struct {
	Repository
	channels []domain.ProviderChannel
}

func (r staticChannelRepo) ListProviderChannels(context.Context, string) ([]domain.ProviderChannel, error) {
	return r.channels, nil
}

// embeddingProbeChannel is one embedding-capability channel row with a single
// enabled member. The member's secret is expected to live at
// provider-channel/<id>/member-1 in the store built by
// newEmbeddingProbeService.
func embeddingProbeChannel(id, endpoint, model string, routeOrder int) domain.ProviderChannel {
	return domain.ProviderChannel{
		ID:           id,
		Capability:   "embedding",
		Label:        "Embedding " + id,
		ProviderName: "openai_embeddings",
		Endpoint:     endpoint,
		Model:        model,
		Enabled:      true,
		RouteOrder:   routeOrder,
		Members: []domain.ProviderChannelMember{{
			ID: "member-1", ChannelID: id, Label: "primary",
			SecretRef: "provider-channel/" + id + "/member-1",
			Enabled:   true, Weight: 1, MaxInflight: 1,
		}},
	}
}

// newEmbeddingProbeService builds a Service whose channel runtime is a real
// providerChannelRuntime over the same static rows, so the probe exercises the
// production config-construction path (buildEmbedderProvider) rather than a
// hand-made provider. Keys are stored for every ref in keys.
func newEmbeddingProbeService(t *testing.T, channels []domain.ProviderChannel, keys map[string]string) *Service {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secrets, err := secretstore.Open(dataDir, "test-admin-token")
	if err != nil {
		t.Fatal(err)
	}
	for ref, key := range keys {
		if err := secrets.Put(ref, key); err != nil {
			t.Fatal(err)
		}
	}
	repo := staticChannelRepo{channels: channels}
	cfg := config.Config{DataDir: dataDir}
	return &Service{
		repo:           repo,
		cfg:            cfg,
		secrets:        secrets,
		channelRuntime: newProviderChannelRuntime(repo, cfg, secrets, nil, nil, nil, nil, nil, nil),
	}
}

// embeddingFixture is the /embeddings endpoint used by most cases: it records
// what it was called with and answers with one valid vector.
type embeddingFixture struct {
	mu       sync.Mutex
	hits     int
	path     string
	auth     string
	model    string
	input    []string
	response string
	status   int
}

func newEmbeddingFixture(status int, response string) (*embeddingFixture, *httptest.Server) {
	fixture := &embeddingFixture{status: status, response: response}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		fixture.hits++
		fixture.path = r.URL.Path
		fixture.auth = r.Header.Get("Authorization")
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fixture.model = body.Model
		fixture.input = body.Input
		status, response := fixture.status, fixture.response
		fixture.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	return fixture, server
}

func (f *embeddingFixture) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// TestProviderChannelEmbeddingSendsConfiguredModelOnce pins the whole billed
// probe contract: one POST to {endpoint}/embeddings carrying the channel's
// configured model and the fixed one-item input, with the selected member's
// key, answering "ok" with the returned dimension and no vector — nothing the
// page could render as data, nothing an index could adopt.
func TestProviderChannelEmbeddingSendsConfiguredModelOnce(t *testing.T) {
	const key = "{{SECRET_ebdedprobe}}"
	fixture, server := newEmbeddingFixture(http.StatusOK, `{"data":[{"index":0,"embedding":[0.25,-0.5,0.75,0.125]}]}`)
	defer server.Close()

	channel := embeddingProbeChannel("channel-embed", server.URL, "embed-test-model", 0)
	service := newEmbeddingProbeService(t, []domain.ProviderChannel{channel}, map[string]string{
		"provider-channel/channel-embed/member-1": key,
	})

	result, err := service.TestProviderChannelEmbedding(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("status=%q message=%q; want ok", result.Status, result.Message)
	}
	if result.Dimensions != 4 {
		t.Fatalf("dimensions=%d; want 4", result.Dimensions)
	}
	if result.Model != "embed-test-model" {
		t.Fatalf("model=%q; want the channel's configured model", result.Model)
	}
	if result.ProviderName != "openai_embeddings" || result.ChannelID != channel.ID {
		t.Fatalf("result identity=%+v; want the addressed channel", result)
	}
	if result.LatencyMS < 0 {
		t.Fatalf("latency_ms=%d; want non-negative", result.LatencyMS)
	}
	if got := fixture.calls(); got != 1 {
		t.Fatalf("provider calls=%d; want exactly 1 (no retry)", got)
	}
	if fixture.path != "/embeddings" {
		t.Fatalf("path=%q; want /embeddings", fixture.path)
	}
	if fixture.auth != "Bearer "+key {
		t.Fatalf("Authorization=%q; want the selected member's key", fixture.auth)
	}
	if fixture.model != "embed-test-model" {
		t.Fatalf("request model=%q; want embed-test-model", fixture.model)
	}
	if len(fixture.input) != 1 || fixture.input[0] != providerChannelEmbeddingProbeInput {
		t.Fatalf("request input=%q; want the fixed single probe text", fixture.input)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for key := range decoded {
		if _, ok := map[string]bool{
			"channel_id": true, "provider_name": true, "model": true, "status": true,
			"dimensions": true, "latency_ms": true, "message": true,
		}[key]; !ok {
			t.Fatalf("probe result carries unexpected field %q: %s", key, raw)
		}
	}
	if _, ok := decoded["vector"]; ok {
		t.Fatalf("probe result carries a vector: %s", raw)
	}
	if strings.Contains(string(raw), key) || strings.Contains(string(raw), "0.25") {
		t.Fatalf("probe result carries the key or vector data: %s", raw)
	}
}

// TestProviderChannelEmbeddingUsesAddressedChannelNotRoutePriority pins that
// the probe tests the card the operator clicked. A second embedding channel
// with a lower route_order (higher priority) is configured: routing through
// the shared embedder wrapper would have sent this call there instead.
func TestProviderChannelEmbeddingUsesAddressedChannelNotRoutePriority(t *testing.T) {
	selectedFixture, selectedServer := newEmbeddingFixture(http.StatusOK, `{"data":[{"index":0,"embedding":[0.1,0.2]}]}`)
	defer selectedServer.Close()
	priorityFixture, priorityServer := newEmbeddingFixture(http.StatusOK, `{"data":[{"index":0,"embedding":[0.3,0.4]}]}`)
	defer priorityServer.Close()

	priority := embeddingProbeChannel("channel-priority", priorityServer.URL, "priority-model", 0)
	selected := embeddingProbeChannel("channel-selected", selectedServer.URL, "selected-model", 10)
	service := newEmbeddingProbeService(t, []domain.ProviderChannel{priority, selected}, map[string]string{
		"provider-channel/channel-priority/member-1": "priority-key",
		"provider-channel/channel-selected/member-1": "selected-key",
	})

	result, err := service.TestProviderChannelEmbedding(context.Background(), selected.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || result.Model != "selected-model" {
		t.Fatalf("result=%+v; want the selected channel's model", result)
	}
	if got := selectedFixture.calls(); got != 1 {
		t.Fatalf("selected channel calls=%d; want 1", got)
	}
	if got := priorityFixture.calls(); got != 0 {
		t.Fatalf("higher-priority channel calls=%d; want 0", got)
	}
}

// TestProviderChannelEmbeddingRejectsNonEmbeddingChannel pins the capability
// preflight: a chat channel is not probed for vectors, and the operator gets a
// fixed explanation instead of a billed call.
func TestProviderChannelEmbeddingRejectsNonEmbeddingChannel(t *testing.T) {
	fixture, server := newEmbeddingFixture(http.StatusOK, `{"data":[{"index":0,"embedding":[0.1]}]}`)
	defer server.Close()

	channel := embeddingProbeChannel("channel-chat", server.URL, "chat-model", 0)
	channel.Capability = "tag_curator"
	channel.ProviderName = "openai_chat"
	service := newEmbeddingProbeService(t, []domain.ProviderChannel{channel}, map[string]string{
		"provider-channel/channel-chat/member-1": "chat-key",
	})

	result, err := service.TestProviderChannelEmbedding(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "unsupported" || result.Message != providerChannelEmbeddingUnsupportedMessage {
		t.Fatalf("result=%+v; want unsupported", result)
	}
	if got := fixture.calls(); got != 0 {
		t.Fatalf("provider calls=%d; want 0 for an unsupported capability", got)
	}
}

// TestProviderChannelEmbeddingRejectsInvalidConfig pins the configuration
// preflight: with no model, or an endpoint that is not an absolute http(s)
// URL, no request can be formed, so none is sent.
func TestProviderChannelEmbeddingRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		endpoint string
	}{
		{name: "empty model", model: "  ", endpoint: "placeholder"},
		{name: "empty endpoint", model: "embed-test-model", endpoint: ""},
		{name: "non-http endpoint", model: "embed-test-model", endpoint: "ftp://example.invalid"},
		{name: "host-less endpoint", model: "embed-test-model", endpoint: "http://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture, server := newEmbeddingFixture(http.StatusOK, `{"data":[{"index":0,"embedding":[0.1]}]}`)
			defer server.Close()
			endpoint := tt.endpoint
			if endpoint == "placeholder" {
				endpoint = server.URL
			}
			channel := embeddingProbeChannel("channel-config", endpoint, tt.model, 0)
			service := newEmbeddingProbeService(t, []domain.ProviderChannel{channel}, map[string]string{
				"provider-channel/channel-config/member-1": "config-key",
			})

			result, err := service.TestProviderChannelEmbedding(context.Background(), channel.ID)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "invalid_config" || result.Message != providerChannelEmbeddingConfigMessage {
				t.Fatalf("result=%+v; want invalid_config", result)
			}
			if got := fixture.calls(); got != 0 {
				t.Fatalf("provider calls=%d; want 0 for invalid config", got)
			}
		})
	}
}

// TestProviderChannelEmbeddingReportsNoReadyMember pins the not-ready
// preflight: a disabled member, or an enabled one whose secret was never
// stored, cannot be tested, and the message says so without touching the
// network. A disabled channel itself is still probed — the operator needs the
// verdict before re-enabling it.
func TestProviderChannelEmbeddingReportsNoReadyMember(t *testing.T) {
	t.Run("disabled member", func(t *testing.T) {
		fixture, server := newEmbeddingFixture(http.StatusOK, `{"data":[{"index":0,"embedding":[0.1]}]}`)
		defer server.Close()
		channel := embeddingProbeChannel("channel-disabled-member", server.URL, "embed-test-model", 0)
		channel.Members[0].Enabled = false
		channel.Enabled = false
		service := newEmbeddingProbeService(t, []domain.ProviderChannel{channel}, map[string]string{
			"provider-channel/channel-disabled-member/member-1": "stored-key",
		})

		result, err := service.TestProviderChannelEmbedding(context.Background(), channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "not_ready" || result.Message != providerChannelEmbeddingNotReadyMessage {
			t.Fatalf("result=%+v; want not_ready", result)
		}
		if got := fixture.calls(); got != 0 {
			t.Fatalf("provider calls=%d; want 0", got)
		}
	})

	t.Run("missing secret", func(t *testing.T) {
		fixture, server := newEmbeddingFixture(http.StatusOK, `{"data":[{"index":0,"embedding":[0.1]}]}`)
		defer server.Close()
		channel := embeddingProbeChannel("channel-missing-secret", server.URL, "embed-test-model", 0)
		service := newEmbeddingProbeService(t, []domain.ProviderChannel{channel}, nil)

		result, err := service.TestProviderChannelEmbedding(context.Background(), channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "not_ready" || result.Message != providerChannelEmbeddingNotReadyMessage {
			t.Fatalf("result=%+v; want not_ready", result)
		}
		if got := fixture.calls(); got != 0 {
			t.Fatalf("provider calls=%d; want 0", got)
		}
	})
}

// TestProviderChannelEmbeddingWithoutRuntime pins the degraded Hub case: a
// Service with no provider-channel runtime cannot build a provider, so it
// reports not_ready instead of panicking or pretending to have tested.
func TestProviderChannelEmbeddingWithoutRuntime(t *testing.T) {
	fixture, server := newEmbeddingFixture(http.StatusOK, `{"data":[{"index":0,"embedding":[0.1]}]}`)
	defer server.Close()
	channel := embeddingProbeChannel("channel-no-runtime", server.URL, "embed-test-model", 0)
	service := newEmbeddingProbeService(t, []domain.ProviderChannel{channel}, map[string]string{
		"provider-channel/channel-no-runtime/member-1": "runtime-key",
	})
	service.channelRuntime = nil

	result, err := service.TestProviderChannelEmbedding(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "not_ready" || result.Message != providerChannelEmbeddingNotReadyMessage {
		t.Fatalf("result=%+v; want not_ready", result)
	}
	if got := fixture.calls(); got != 0 {
		t.Fatalf("provider calls=%d; want 0 without a runtime", got)
	}
}

// TestProviderChannelEmbeddingRedactsProviderFailures pins the failure path: an
// upstream 401/429/5xx whose body echoes the key must produce the fixed failed
// message, never the upstream text and never the key.
func TestProviderChannelEmbeddingRedactsProviderFailures(t *testing.T) {
	const key = "{{SECRET_ebdfail}}"
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fixture, server := newEmbeddingFixture(status, `{"error":"invalid api key `+key+` for relay node 7"}`)
			defer server.Close()
			channel := embeddingProbeChannel("channel-failing", server.URL, "embed-test-model", 0)
			service := newEmbeddingProbeService(t, []domain.ProviderChannel{channel}, map[string]string{
				"provider-channel/channel-failing/member-1": key,
			})

			result, err := service.TestProviderChannelEmbedding(context.Background(), channel.ID)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "failed" || result.Message != providerChannelEmbeddingFailedMessage {
				t.Fatalf("result=%+v; want the fixed failed message", result)
			}
			if result.Dimensions != 0 {
				t.Fatalf("dimensions=%d; want none on failure", result.Dimensions)
			}
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{key, "relay node", "invalid api key"} {
				if strings.Contains(string(raw), forbidden) {
					t.Fatalf("failure result leaked %q: %s", forbidden, raw)
				}
			}
			if got := fixture.calls(); got != 1 {
				t.Fatalf("provider calls=%d; want exactly one attempt", got)
			}
		})
	}
}

// TestProviderChannelEmbeddingRejectsInvalidVectors pins the shape rule at the
// service boundary: an empty vector, an all-zero vector, a count that does not
// match the single input, or a duplicated index is a failed probe with no
// dimension, not a success with a useless vector.
func TestProviderChannelEmbeddingRejectsInvalidVectors(t *testing.T) {
	tests := []struct {
		name     string
		response string
	}{
		{name: "empty vector", response: `{"data":[{"index":0,"embedding":[]}]}`},
		{name: "zero vector", response: `{"data":[{"index":0,"embedding":[0,0,0]}]}`},
		{name: "two vectors", response: `{"data":[{"index":0,"embedding":[0.1]},{"index":1,"embedding":[0.2]}]}`},
		{name: "duplicate index", response: `{"data":[{"index":0,"embedding":[0.1]},{"index":0,"embedding":[0.2]}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, server := newEmbeddingFixture(http.StatusOK, tt.response)
			defer server.Close()
			channel := embeddingProbeChannel("channel-vector", server.URL, "embed-test-model", 0)
			service := newEmbeddingProbeService(t, []domain.ProviderChannel{channel}, map[string]string{
				"provider-channel/channel-vector/member-1": "vector-key",
			})

			result, err := service.TestProviderChannelEmbedding(context.Background(), channel.ID)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "failed" || result.Message != providerChannelEmbeddingFailedMessage {
				t.Fatalf("result=%+v; want failed", result)
			}
			if result.Dimensions != 0 {
				t.Fatalf("dimensions=%d; want 0 for an invalid vector", result.Dimensions)
			}
		})
	}
}

// TestProviderChannelEmbeddingReportsTimeout pins deadline classification
// without sleeping: an already-cancelled caller context reaches the adapter as
// a cancellation, which the probe reports as "timeout" with the fixed message
// rather than as a generic failure.
func TestProviderChannelEmbeddingReportsTimeout(t *testing.T) {
	_, server := newEmbeddingFixture(http.StatusOK, `{"data":[{"index":0,"embedding":[0.1]}]}`)
	defer server.Close()
	channel := embeddingProbeChannel("channel-timeout", server.URL, "embed-test-model", 0)
	service := newEmbeddingProbeService(t, []domain.ProviderChannel{channel}, map[string]string{
		"provider-channel/channel-timeout/member-1": "timeout-key",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := service.TestProviderChannelEmbedding(ctx, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "timeout" || result.Message != providerChannelEmbeddingTimeoutMessage {
		t.Fatalf("result=%+v; want timeout", result)
	}
}

// TestProviderChannelEmbeddingUnknownChannel pins the lookup error: a missing
// channel is a caller error, so it is the one case that returns an error and
// the API renders its safe 400 envelope.
func TestProviderChannelEmbeddingUnknownChannel(t *testing.T) {
	service := newEmbeddingProbeService(t, nil, nil)
	if _, err := service.TestProviderChannelEmbedding(context.Background(), "missing-channel"); err == nil {
		t.Fatal("unknown channel must return an error")
	}
}

// TestSingleEmbeddingVector pins the shape helper directly for the cases a
// provider could still hand back through a plan-provider adapter that skips
// the shared response validation.
func TestSingleEmbeddingVector(t *testing.T) {
	if _, ok := singleEmbeddingVector(nil); ok {
		t.Fatal("nil vector list must be rejected")
	}
	if _, ok := singleEmbeddingVector([][]float64{{}}); ok {
		t.Fatal("empty vector must be rejected")
	}
	if _, ok := singleEmbeddingVector([][]float64{{0, 0}}); ok {
		t.Fatal("all-zero vector must be rejected")
	}
	if _, ok := singleEmbeddingVector([][]float64{{0.1, 0.2}, {0.3}}); ok {
		t.Fatal("more than one vector must be rejected")
	}
	vector, ok := singleEmbeddingVector([][]float64{{0.1, -0.2, 0.3}})
	if !ok || len(vector) != 3 {
		t.Fatalf("valid vector rejected: ok=%v len=%d", ok, len(vector))
	}
}
