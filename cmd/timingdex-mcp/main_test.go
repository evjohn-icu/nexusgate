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
}

func (f *fakeHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /api/v1/hardware", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"hardware":"software"}`))
	})
	mux.HandleFunc("GET /api/v1/search/shots/hybrid", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(f.searchHits)
			return
		}
		http.Error(w, "missing q", http.StatusBadRequest)
	})
	mux.HandleFunc("POST /api/v1/repurpose/plans", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-tok" {
			http.Error(w, "bad agent auth", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"` + f.planID + `"}`))
	})
	mux.HandleFunc("POST /api/v1/repurpose/plans/{id}/revisions", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-tok" {
			http.Error(w, "bad agent auth", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"` + f.planID + `"}`))
	})
	mux.HandleFunc("POST /api/v1/admin/webdav/spaces/{id}/links", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer admin-tok" {
			http.Error(w, "bad admin auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"path":"/assets/asset-1/original.mov"}`))
	})
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
