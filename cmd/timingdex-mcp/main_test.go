package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

// fakeHub is a minimal in-test Timingdex Hub API.
type fakeHub struct {
	searchHits []map[string]any

	// evidenceState is the evidence state returned in every search hit's
	// evidence array. Defaults to "confirmed" when empty.
	evidenceState string

	// forceStatus, when non-zero, makes every handler return this HTTP status.
	// Set alongside forceBody to simulate error responses.
	forceStatus int
	forceBody   string
}

func (f *fakeHub) handler() http.Handler {
	mux := http.NewServeMux()

	// errWrapper intercepts all requests when forceStatus is set, simulating
	// Hub-wide error modes (5xx, 4xx).
	errWrapper := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if f.forceStatus > 0 {
				if f.forceBody != "" {
					http.Error(w, f.forceBody, f.forceStatus)
				} else {
					w.WriteHeader(f.forceStatus)
				}
				return
			}
			h(w, r)
		}
	}

	// /health is anonymous by design; /hardware, the structured search route
	// and the asset routes are behind requireTrustedRead, which admits a
	// bearer agent token from anywhere — a remote (off-LAN) MCP client must
	// carry one or get 403.
	mux.HandleFunc("GET /api/v1/health", errWrapper(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	mux.HandleFunc("GET /api/v1/hardware", errWrapper(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-tok" {
			http.Error(w, "hardware requires trusted read", http.StatusForbidden)
			return
		}
		w.Write([]byte(`{"hardware":"software"}`))
	}))
	mux.HandleFunc("POST /api/v1/search/shots", errWrapper(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-tok" {
			http.Error(w, "search requires trusted read", http.StatusForbidden)
			return
		}
		results := make([]map[string]any, 0, len(f.searchHits))
		state := f.evidenceState
		if state == "" {
			state = "confirmed"
		}
		for _, hit := range f.searchHits {
			results = append(results, map[string]any{
				"shot_id":  hit["id"],
				"asset_id": hit["asset_id"],
				"start_ms": 0,
				"end_ms":   5000,
				"score":    hit["score"],
				"evidence": []map[string]any{{"constraint_type": "object", "constraint": "person", "state": state, "sources": []string{"objects"}}},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"query":      map[string]any{"raw": "sunset", "intent": "semantic"},
			"search_id":  "s-1",
			"query_hash": "0123456789abcdef",
			"results":    results,
		})
	}))
	mux.HandleFunc("GET /api/v1/assets/{id}/shots", errWrapper(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-tok" {
			http.Error(w, "shots requires trusted read", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":"shot-1","asset_id":"` + r.PathValue("id") + `","start_ms":0,"end_ms":5000,"description":"雨夜城市街道","tags":["rain","urban_night"]}]`))
	}))
	mux.HandleFunc("GET /api/v1/assets/{id}/transcript", errWrapper(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-tok" {
			http.Error(w, "transcript requires trusted read", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"asset_id":"` + r.PathValue("id") + `","source":"aligned","language":"zh","text":"你好 世界","segments":[{"start_ms":0,"end_ms":260,"text":"你好"},{"start_ms":260,"end_ms":620,"text":"世界"}],"words":[{"start_ms":0,"end_ms":260,"text":"你好","confidence":0.94},{"start_ms":260,"end_ms":620,"text":"世界"}]}`))
	}))
	mux.HandleFunc("GET /api/v1/assets/{id}", errWrapper(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-tok" {
			http.Error(w, "asset requires trusted read", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"` + r.PathValue("id") + `","duration":180000,"state":"analyzed","shot_count":12}`))
	}))
	mux.HandleFunc("GET /api/v1/shots/{id}", errWrapper(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-tok" {
			http.Error(w, "shot requires trusted read", http.StatusForbidden)
			return
		}
		shotID := r.PathValue("id")
		if shotID == "nonexistent" {
			http.Error(w, "shot not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"shot_id":"` + shotID + `","asset_id":"asset-1","start_ms":0,"end_ms":5000,"description":"雨夜城市街道","tags":["rain","urban_night"],"thumbnail":"/thumbnails/` + shotID + `.jpg","evidence":[{"constraint_type":"object","constraint":"person","state":"confirmed","sources":["objects"]}]}`))
	}))
	return mux
}

func newTestClient(t *testing.T, hub *fakeHub) *hubClient {
	t.Helper()
	srv := httptest.NewServer(hub.handler())
	t.Cleanup(srv.Close)
	return &hubClient{
		baseURL:    srv.URL,
		agentToken: "agent-tok",
		http:       srv.Client(),
	}
}

func TestInspectLibrary(t *testing.T) {
	hub := &fakeHub{}
	c := newTestClient(t, hub)
	info, err := c.inspectLibrary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info["health"] == nil || info["hardware"] == nil {
		t.Fatalf("inspect result missing sections: %v", info)
	}
}

func TestSearchShots(t *testing.T) {
	hub := &fakeHub{searchHits: []map[string]any{
		{"id": "shot-1", "asset_id": "asset-1", "score": 1.5},
	}}
	c := newTestClient(t, hub)
	result, err := c.searchShots(context.Background(), "sunset", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, ok := result["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("results = %v, want one structured result", result["results"])
	}
	hit := results[0].(map[string]any)
	if hit["shot_id"] != "shot-1" || hit["asset_id"] != "asset-1" {
		t.Fatalf("hit = %v, want shot_id=shot-1", hit)
	}
	// The structured endpoint carries per-constraint evidence, not just a score.
	if evidence, ok := hit["evidence"].([]any); !ok || len(evidence) != 1 {
		t.Fatalf("evidence = %v, want the evidence gate's verdict", hit["evidence"])
	}
}

func TestSearchShotsEmptyResults(t *testing.T) {
	// Empty searchHits should return an empty results list, not an error.
	hub := &fakeHub{searchHits: []map[string]any{}}
	c := newTestClient(t, hub)
	result, err := c.searchShots(context.Background(), "nonexistent", 10, nil)
	if err != nil {
		t.Fatalf("empty search should not error: %v", err)
	}
	results, ok := result["results"].([]any)
	if !ok || len(results) != 0 {
		t.Fatalf("empty search returned %v, want 0 results", result["results"])
	}
}

func TestGetTimeline(t *testing.T) {
	hub := &fakeHub{}
	c := newTestClient(t, hub)
	shots, err := c.getTimeline(context.Background(), "asset-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 1 || shots[0]["id"] != "shot-1" || shots[0]["start_ms"] != float64(0) {
		t.Fatalf("shots = %v, want the asset's full shot list", shots)
	}
}

func TestGetTranscript(t *testing.T) {
	hub := &fakeHub{}
	c := newTestClient(t, hub)
	transcript, err := c.getTranscript(context.Background(), "asset-1")
	if err != nil {
		t.Fatal(err)
	}
	if transcript["asset_id"] != "asset-1" || transcript["source"] != "aligned" {
		t.Fatalf("transcript = %v, want asset_id=asset-1 source=aligned", transcript)
	}
	words, ok := transcript["words"].([]any)
	if !ok || len(words) != 2 {
		t.Fatalf("words = %v, want the word stream decoded", transcript["words"])
	}
}

// get_transcript is a trusted read like search_footage: a client without the
// agent token must surface the Hub's remote 403 rather than passing silently.
func TestGetTranscriptWithoutAgentTokenSurfacesRemote403(t *testing.T) {
	hub := &fakeHub{}
	srv := httptest.NewServer(hub.handler())
	t.Cleanup(srv.Close)
	client := &hubClient{baseURL: srv.URL, http: srv.Client()}
	if _, err := client.getTranscript(context.Background(), "asset-1"); err == nil {
		t.Fatal("getTranscript without agent token must fail on a remote Hub")
	}
}

// A word-level transcript for a long asset can exceed do()'s 1 MiB
// success-body bound. getTranscript must decode the full body — truncating a
// valid transcript would break the full-transcript contract.
func TestGetTranscriptDecodesLargeTranscript(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/assets/{id}/transcript", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-tok" {
			http.Error(w, "transcript requires trusted read", http.StatusForbidden)
			return
		}
		const n = 30000
		words := make([]map[string]any, 0, n)
		for i := range n {
			words = append(words, map[string]any{"start_ms": i * 100, "end_ms": i*100 + 90, "text": "词语"})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"asset_id": r.PathValue("id"), "source": "aligned", "language": "zh", "words": words})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := &hubClient{baseURL: srv.URL, agentToken: "agent-tok", http: srv.Client()}
	transcript, err := client.getTranscript(context.Background(), "asset-1")
	if err != nil {
		t.Fatalf("large transcript must decode without truncation: %v", err)
	}
	words, ok := transcript["words"].([]any)
	if !ok || len(words) != 30000 {
		t.Fatalf("words = %d, want all 30000 decoded", len(words))
	}
}

// A client that never configured TIMINGDEX_AGENT_TOKEN is exactly the remote
// 403 the v0.23.1 changelog claimed to have fixed and did not: the read routes
// behind requireTrustedRead reject empty tokens off the trusted network, and
// the MCP tool must surface that as a clear error instead of silently passing.
func TestReadToolsWithoutAgentTokenSurfaceRemote403(t *testing.T) {
	hub := &fakeHub{searchHits: []map[string]any{}}
	srv := httptest.NewServer(hub.handler())
	t.Cleanup(srv.Close)
	client := &hubClient{baseURL: srv.URL, http: srv.Client()}
	if _, err := client.inspectLibrary(context.Background()); err == nil {
		t.Fatal("inspectLibrary without agent token must fail on a remote Hub")
	}
	if _, err := client.searchShots(context.Background(), "sunset", 10, nil); err == nil {
		t.Fatal("searchShots without agent token must fail on a remote Hub")
	}
	if _, err := client.getTimeline(context.Background(), "asset-1"); err == nil {
		t.Fatal("getTimeline without agent token must fail on a remote Hub")
	}
}

func TestHub5xxPropagatesStatus(t *testing.T) {
	// do() must include the HTTP status code in the error when Hub returns 5xx.
	hub := &fakeHub{forceStatus: http.StatusInternalServerError, forceBody: "boom"}
	c := newTestClient(t, hub)
	_, err := c.searchShots(context.Background(), "sunset", 10, nil)
	if err == nil {
		t.Fatal("expected error for 5xx response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error should contain status code '500', got: %v", err)
	}
}

func TestGetAsset(t *testing.T) {
	hub := &fakeHub{}
	c := newTestClient(t, hub)
	asset, err := c.getAsset(context.Background(), "asset-1")
	if err != nil {
		t.Fatal(err)
	}
	if asset["id"] != "asset-1" {
		t.Fatalf("asset id = %v, want asset-1", asset["id"])
	}
	// The description tells agents to use get_asset's shot_count to decide
	// whether to fetch the timeline — it must round-trip from the Hub.
	if asset["shot_count"] != float64(12) {
		t.Fatalf("shot_count = %v, want 12", asset["shot_count"])
	}
}

func TestGetShot(t *testing.T) {
	hub := &fakeHub{}
	c := newTestClient(t, hub)
	shot, err := c.getShot(context.Background(), "shot-1")
	if err != nil {
		t.Fatal(err)
	}
	if shot["shot_id"] != "shot-1" {
		t.Fatalf("shot id = %v, want shot-1", shot["shot_id"])
	}
}

func TestGetShotNotFound(t *testing.T) {
	hub := &fakeHub{}
	c := newTestClient(t, hub)
	_, err := c.getShot(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent shot")
	}
}

func TestNoWriteTools(t *testing.T) {
	client, err := newHubClient()
	if err != nil {
		t.Fatal(err)
	}
	srv := server.NewMCPServer("timingdex", "v0.1", server.WithToolCapabilities(true))
	registerTools(srv, client)
	var names []string
	for name := range srv.ListTools() {
		names = append(names, name)
	}
	for _, prohibited := range []string{"create_edit_plan", "revise_edit_plan", "request_source_media"} {
		for _, name := range names {
			if name == prohibited {
				t.Fatalf("write tool %q must not be registered", prohibited)
			}
		}
	}
}

func TestEvidenceStatePreserved(t *testing.T) {
	hub := &fakeHub{
		searchHits: []map[string]any{
			{"id": "shot-1", "asset_id": "asset-1", "score": 1.5},
		},
		evidenceState: "unknown",
	}
	c := newTestClient(t, hub)
	result, err := c.searchShots(context.Background(), "sunset", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, ok := result["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("results = %v, want one result", result["results"])
	}
	hit := results[0].(map[string]any)
	evidence, ok := hit["evidence"].([]any)
	if !ok || len(evidence) != 1 {
		t.Fatalf("evidence = %v, want evidence array", hit["evidence"])
	}
	// The unknown evidence state must be preserved as-is, not converted to
	// false or stripped.
	state, ok := evidence[0].(map[string]any)["state"].(string)
	if !ok || state != "unknown" {
		t.Fatalf("evidence state = %v, want 'unknown' (string)", evidence[0].(map[string]any)["state"])
	}
}

func TestRequireString(t *testing.T) {
	if _, rerr := requireString(map[string]any{}, "q"); rerr == nil {
		t.Fatal("missing arg should produce an error result")
	}
	if v, rerr := requireString(map[string]any{"q": "sunset"}, "q"); rerr != nil || v != "sunset" {
		t.Fatalf("got %q, %v", v, rerr)
	}
	if _, rerr := requireString(map[string]any{"q": "  "}, "q"); rerr == nil {
		t.Fatal("blank arg should produce an error result")
	}
}

// TestNewHubClientRequiresFingerprintForHTTPS pins the cross-machine trust
// boundary: an https base URL without TIMINGDEX_HUB_FINGERPRINT must refuse
// to start rather than silently accept any Hub certificate, and a malformed
// fingerprint is refused too. Plain http (local development) stays usable
// without one.
func TestNewHubClientRequiresFingerprintForHTTPS(t *testing.T) {
	t.Setenv("TIMINGDEX_BASE_URL", "https://nas.lan:8787")
	t.Setenv("TIMINGDEX_HUB_FINGERPRINT", "")
	if _, err := newHubClient(); err == nil || !strings.Contains(err.Error(), "TIMINGDEX_HUB_FINGERPRINT") {
		t.Fatalf("https without fingerprint must fail startup, got %v", err)
	}

	t.Setenv("TIMINGDEX_HUB_FINGERPRINT", "not-a-fingerprint")
	if _, err := newHubClient(); err == nil || !strings.Contains(err.Error(), "64-character SHA-256") {
		t.Fatalf("malformed fingerprint must fail startup, got %v", err)
	}

	t.Setenv("TIMINGDEX_HUB_FINGERPRINT", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	client, err := newHubClient()
	if err != nil {
		t.Fatalf("https with a valid fingerprint must construct: %v", err)
	}
	if client.http.Transport.(*http.Transport).TLSClientConfig == nil {
		t.Fatal("expected a pinned TLS client config for https + fingerprint")
	}

	t.Setenv("TIMINGDEX_BASE_URL", "")
	t.Setenv("TIMINGDEX_HUB_FINGERPRINT", "")
	if _, err := newHubClient(); err != nil {
		t.Fatalf("default http base URL must construct without a fingerprint: %v", err)
	}
}

// TestNewHubClientRejectsRemoteHTTP pins the cleartext boundary: the agent
// and administrator tokens must not cross the network in cleartext, so an
// http:// base URL pointing at anything other than loopback or link-local
// fails startup — mirroring the worker CLI's https-only enrollment — while the
// loopback/local development forms keep working.
func TestNewHubClientRejectsRemoteHTTP(t *testing.T) {
	t.Setenv("TIMINGDEX_HUB_FINGERPRINT", "")

	t.Setenv("TIMINGDEX_BASE_URL", "http://192.168.1.50:8787")
	if _, err := newHubClient(); err == nil || !strings.Contains(err.Error(), "cleartext") {
		t.Fatalf("remote LAN http must fail startup, got %v", err)
	}

	t.Setenv("TIMINGDEX_BASE_URL", "http://hub.example.com:8787")
	if _, err := newHubClient(); err == nil || !strings.Contains(err.Error(), "cleartext") {
		t.Fatalf("remote http hostname must fail startup, got %v", err)
	}

	t.Setenv("TIMINGDEX_BASE_URL", "http://127.0.0.1:8787")
	if _, err := newHubClient(); err != nil {
		t.Fatalf("loopback http must construct: %v", err)
	}

	t.Setenv("TIMINGDEX_BASE_URL", "http://localhost:8787")
	if _, err := newHubClient(); err != nil {
		t.Fatalf("localhost http must construct: %v", err)
	}

	t.Setenv("TIMINGDEX_BASE_URL", "http://[::1]:8787")
	if _, err := newHubClient(); err != nil {
		t.Fatalf("IPv6 loopback http must construct: %v", err)
	}

	t.Setenv("TIMINGDEX_BASE_URL", "http://169.254.10.10:8787")
	if _, err := newHubClient(); err != nil {
		t.Fatalf("link-local http must construct: %v", err)
	}
}

func TestValidFingerprint(t *testing.T) {
	if ValidFingerprint("") || ValidFingerprint("abc") || ValidFingerprint("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz") {
		t.Fatal("invalid fingerprints must be rejected")
	}
	// Like the Worker's ValidFingerprint, surrounding whitespace is trimmed
	// before the length/hex check.
	if !ValidFingerprint("  0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  ") {
		t.Fatal("a 64-char SHA-256 hex fingerprint must be accepted")
	}
}

// TestToolNamesRegistered verifies the MCP server exposes exactly the tools we
// intend, and that the stdio server can be constructed without panicking.
func TestToolNamesRegistered(t *testing.T) {
	client, err := newHubClient()
	if err != nil {
		t.Fatal(err)
	}
	srv := server.NewMCPServer("timingdex", "v0.1", server.WithToolCapabilities(true))
	registerTools(srv, client)

	var names []string
	for name := range srv.ListTools() {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{"get_asset", "get_shot", "get_timeline", "get_transcript", "inspect_library", "search_shots"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", names, want)
	}
}

// TestGetAssetDecodesLargeBody verifies that getAsset decodes through getLarge
// (64 MiB cap) rather than do()'s 1 MiB bound, which would silently truncate
// an AssetDetail response whose embedded transcript fills the response body.
func TestGetAssetDecodesLargeBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/assets/{id}", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-tok" {
			http.Error(w, "asset requires trusted read", http.StatusForbidden)
			return
		}
		// Generate a response comfortably over 1 MiB (do()'s bound) so the
		// decode path must be getLarge to succeed. Keep the padding well under
		// maxTranscriptBytes (64 MiB).
		const target = 2 << 20 // 2 MiB
		padding := strings.Repeat("x", target-200)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"` + r.PathValue("id") + `","state":"analyzed","duration":180000,"shot_count":999,"transcript":{"text":"` + padding + `"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := &hubClient{baseURL: srv.URL, agentToken: "agent-tok", http: srv.Client()}
	asset, err := client.getAsset(context.Background(), "big-asset")
	if err != nil {
		t.Fatalf("getAsset must decode a >1MiB body: %v", err)
	}
	if asset["shot_count"] != float64(999) {
		t.Fatalf("shot_count = %v, want 999", asset["shot_count"])
	}
}
