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
	planID     string

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

	// /health is anonymous by design; /hardware and the hybrid search route
	// are behind requireTrustedRead, which admits a bearer agent token from
	// anywhere — a remote (off-LAN) MCP client must carry one or get 403.
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
	mux.HandleFunc("GET /api/v1/search/shots/hybrid", errWrapper(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-tok" {
			http.Error(w, "search requires trusted read", http.StatusForbidden)
			return
		}
		if got := r.URL.Query().Get("q"); got != "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(f.searchHits)
			return
		}
		http.Error(w, "missing q", http.StatusBadRequest)
	}))
	mux.HandleFunc("POST /api/v1/repurpose/plans", errWrapper(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-tok" {
			http.Error(w, "bad agent auth", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"` + f.planID + `"}`))
	}))
	mux.HandleFunc("GET /api/v1/repurpose/plans/{id}", errWrapper(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"` + r.PathValue("id") + `","status":"draft","sections":[]}`))
	}))
	mux.HandleFunc("POST /api/v1/repurpose/plans/{id}/revisions", errWrapper(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-tok" {
			http.Error(w, "bad agent auth", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"` + f.planID + `"}`))
	}))
	mux.HandleFunc("POST /api/v1/admin/webdav/spaces/{id}/links", errWrapper(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer admin-tok" {
			http.Error(w, "bad admin auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"path":"/assets/asset-1/original.mov"}`))
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
		adminToken: "admin-tok",
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

func TestSearchFootage(t *testing.T) {
	hub := &fakeHub{searchHits: []map[string]any{
		{"id": "shot-1", "asset_id": "asset-1", "score": 1.5},
	}}
	c := newTestClient(t, hub)
	hits, err := c.searchFootage(context.Background(), "sunset", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0]["id"] != "shot-1" {
		t.Fatalf("hits = %v", hits)
	}
}

func TestSearchFootageEmptyResults(t *testing.T) {
	// Empty searchHits should return an empty slice, not an error.
	hub := &fakeHub{searchHits: []map[string]any{}}
	c := newTestClient(t, hub)
	hits, err := c.searchFootage(context.Background(), "nonexistent", 10)
	if err != nil {
		t.Fatalf("empty search should not error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("empty search returned %d hits, want 0", len(hits))
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
	if _, err := client.searchFootage(context.Background(), "sunset", 10); err == nil {
		t.Fatal("searchFootage without agent token must fail on a remote Hub")
	}
}

func TestHub5xxPropagatesStatus(t *testing.T) {
	// do() must include the HTTP status code in the error when Hub returns 5xx.
	hub := &fakeHub{forceStatus: http.StatusInternalServerError, forceBody: "boom"}
	c := newTestClient(t, hub)
	_, err := c.searchFootage(context.Background(), "sunset", 10)
	if err == nil {
		t.Fatal("expected error for 5xx response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error should contain status code '500', got: %v", err)
	}
}

func TestRequestSourceMedia4xxPropagated(t *testing.T) {
	// A 4xx from the Hub (e.g. 404 for unknown space) should be propagated as an error.
	hub := &fakeHub{forceStatus: http.StatusNotFound, forceBody: "space not found"}
	c := newTestClient(t, hub)
	_, err := c.requestSourceMedia(context.Background(), "bad-space", "asset-1")
	if err == nil {
		t.Fatal("expected error for 4xx response")
	}
	if !strings.Contains(err.Error(), "space not found") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("error should contain body and status '404', got: %v", err)
	}
}

func TestCreateEditPlanUsesAgentToken(t *testing.T) {
	hub := &fakeHub{planID: "plan-42"}
	c := newTestClient(t, hub)
	plan, err := c.createEditPlan(context.Background(), "城市宣传片")
	if err != nil {
		t.Fatal(err)
	}
	if plan.ID != "plan-42" {
		t.Fatalf("plan id = %q", plan.ID)
	}
}

func TestRequestSourceMediaUsesAdminToken(t *testing.T) {
	hub := &fakeHub{}
	c := newTestClient(t, hub)
	out, err := c.requestSourceMedia(context.Background(), "space-9", "asset-1")
	if err != nil {
		t.Fatal(err)
	}
	if out["path"] != "/assets/asset-1/original.mov" {
		t.Fatalf("path = %v", out)
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

// TestToolNamesRegistered verifies the MCP server exposes exactly the tools we
// intend, and that the stdio server can be constructed without panicking.
func TestToolNamesRegistered(t *testing.T) {
	client := newHubClient()
	srv := server.NewMCPServer("timingdex", "v0.1", server.WithToolCapabilities(true))
	registerTools(srv, client)

	var names []string
	for name := range srv.ListTools() {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{"create_edit_plan", "inspect_library", "request_source_media", "revise_edit_plan", "search_footage"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", names, want)
	}
}
