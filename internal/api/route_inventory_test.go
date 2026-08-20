package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type apiRouteInventoryEntry struct {
	method, path, guard, body string
	want                      int
}

var expectedRouteInventoryPatterns = strings.TrimSpace(`
GET /api/v1/health
POST /api/v1/auth/admin/session
GET /api/v1/auth/admin/session
DELETE /api/v1/auth/admin/session
GET /api/v1/hardware
GET /api/v1/setup/status
GET /api/v1/agent/capabilities
GET /favicon.ico
GET /api/v1/roots
POST /api/v1/hub/worker-pairings
GET /api/v1/admin/webdav/accounts
POST /api/v1/admin/webdav/accounts
DELETE /api/v1/admin/webdav/accounts/{username}
POST /api/v1/admin/webdav/spaces
GET /api/v1/admin/webdav/spaces
DELETE /api/v1/admin/webdav/spaces/{id}
POST /api/v1/admin/webdav/spaces/{id}/links
GET /api/v1/hub/workers
POST /api/v1/hub/workers/{id}/revoke
GET /api/v1/admin/provider-channels
GET /api/v1/admin/provider-channels/status
POST /api/v1/admin/provider-channels
PATCH /api/v1/admin/provider-channels/{id}
POST /api/v1/admin/provider-channels/{id}/enable
POST /api/v1/admin/provider-channels/{id}/disable
DELETE /api/v1/admin/provider-channels/{id}
POST /api/v1/admin/provider-channels/{id}/test
POST /api/v1/admin/provider-channels/probe-models
GET /api/v1/admin/assets/{id}/capture-location
POST /api/v1/worker/enroll
POST /api/v1/worker/heartbeat
POST /api/v1/worker/lease
POST /api/v1/worker/jobs/{id}/complete
POST /api/v1/worker/jobs/{id}/progress
POST /api/v1/worker/jobs/{id}/credentials/{operation}
POST /api/v1/worker/jobs/{id}/provider/{operation}
POST /api/v1/worker/jobs/{id}/artifacts
PUT /api/v1/worker/jobs/{id}/artifacts/{type}
POST /api/v1/roots
POST /api/v1/roots/{id}/scan
POST /api/v1/roots/inspect
GET /api/v1/roots/health
GET /{$}
GET /setup
GET /progress
GET /workers
GET /library-roots
GET /repurpose
GET /tags
GET /providers
GET /collections
GET /api/v1/assets
GET /api/v1/library/processing-summary
GET /api/v1/collections
GET /api/v1/collections/{id}
GET /api/v1/collections/{id}/assets
POST /api/v1/collections
DELETE /api/v1/collections/{id}
GET /api/v1/collections/{id}/shots
POST /api/v1/collections/{id}/shots
DELETE /api/v1/collections/{id}/shots/{shot_id}
POST /api/v1/collections/{id}/shots/reorder
GET /api/v1/shoot-sessions
GET /api/v1/assets/{id}
GET /api/v1/assets/{id}/shots
GET /api/v1/assets/{id}/transcript
GET /api/v1/assets/{id}/thumbnail
GET /api/v1/assets/{id}/proxy
GET /api/v1/jobs
GET /api/v1/jobs/summary
GET /api/v1/cost/summary
GET /api/v1/issues
GET /api/v1/admin/worker-jobs/{id}
POST /api/v1/admin/worker-jobs/{id}/assignment
POST /api/v1/pipeline/run
POST /api/v1/pipeline/retry-failed
POST /api/v1/pipeline/resume-deferred
POST /api/v1/test-drive
GET /api/v1/test-drive/suggestions
GET /api/v1/pipeline/supervisor
GET /api/v1/pipeline/throttle
PUT /api/v1/pipeline/throttle
GET /api/v1/storage/overview
GET /settings
GET /api/v1/search
GET /api/v1/search/shots
GET /api/v1/search/shots/hybrid
POST /api/v1/search/shots
GET /api/v1/shots/{id}/similar
GET /api/v1/discover/rare-shots
GET /api/v1/tags
GET /api/v1/tags/unresolved
POST /api/v1/tags/curate
POST /api/v1/tags/clusters
GET /api/v1/tags/proposals
POST /api/v1/tags/proposals/{id}/review
GET /api/v1/library/summary
POST /api/v1/library/summary/generate
POST /api/v1/repurpose/plans
GET /api/v1/repurpose/plans
GET /api/v1/repurpose/plans/{id}
GET /api/v1/repurpose/plans/{id}/revisions
POST /api/v1/repurpose/plans/{id}/revisions
POST /api/v1/repurpose/plans/{id}/revisions/{revision}/approve
GET /api/v1/repurpose/plans/{id}/export.edl
GET /api/v1/repurpose/plans/{id}/export.fcpxml
/api/v1/
GET /worker-setup
GET /api/v1/hub/worker-setup/context
GET /api/v1/admin/hub/worker-setup/library-roots
GET /api/v1/hub/worker-binaries/{platform}
POST /api/v1/hub/worker-setup/script
`)

// Keep this table in lockstep with Handler. The body column is deliberately
// explicit: it documents which routes are JSON, opaque provider relay data,
// multipart, raw artifacts, or bodyless.
func TestAPIRouteInventoryGuardMatrix(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "route-matrix")
	handler := NewServer("admin-token", service).Handler()
	const remoteAddr = "192.0.2.1:1"
	tests := []apiRouteInventoryEntry{
		{http.MethodGet, "/api/v1/health", "public", "JSON", http.StatusOK},
		{http.MethodGet, "/api/v1/hardware", "trusted", "JSON", http.StatusForbidden},
		{http.MethodGet, "/api/v1/setup/status", "trusted", "JSON", http.StatusForbidden},
		{http.MethodGet, "/api/v1/agent/capabilities", "public", "JSON", http.StatusOK},
		{http.MethodGet, "/api/v1/roots", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/hub/worker-pairings", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/admin/webdav/accounts", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/admin/webdav/accounts", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodDelete, "/api/v1/admin/webdav/accounts/u", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/admin/webdav/spaces", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/admin/webdav/spaces", "admin", "none", http.StatusUnauthorized},
		{http.MethodDelete, "/api/v1/admin/webdav/spaces/x", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/admin/webdav/spaces/x/links", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/hub/workers", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/hub/workers/x/revoke", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/admin/provider-channels", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/admin/provider-channels/status", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/admin/provider-channels", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodPatch, "/api/v1/admin/provider-channels/x", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/admin/provider-channels/x/enable", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/admin/provider-channels/x/disable", "admin", "none", http.StatusUnauthorized},
		{http.MethodDelete, "/api/v1/admin/provider-channels/x", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/admin/provider-channels/x/test", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/admin/provider-channels/probe-models", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/admin/assets/x/capture-location", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/worker/enroll", "worker", "JSON", http.StatusBadRequest},
		{http.MethodPost, "/api/v1/worker/heartbeat", "worker", "JSON", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/worker/lease", "worker", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/worker/jobs/x/complete", "worker", "JSON", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/worker/jobs/x/progress", "worker", "JSON", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/worker/jobs/x/credentials/video_analysis", "worker", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/worker/jobs/x/provider/video_analysis", "worker", "opaque", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/worker/jobs/x/artifacts", "worker", "multipart", http.StatusUnauthorized},
		{http.MethodPut, "/api/v1/worker/jobs/x/artifacts/thumbnail", "worker", "raw", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/roots", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/roots/x/scan", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/roots/inspect", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/roots/health", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/assets", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/library/processing-summary", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/collections", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/collections/x", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/collections/x/assets", "trusted", "none", http.StatusForbidden},
		{http.MethodPost, "/api/v1/collections", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodDelete, "/api/v1/collections/x", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/collections/x/shots", "trusted", "none", http.StatusForbidden},
		{http.MethodPost, "/api/v1/collections/x/shots", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodDelete, "/api/v1/collections/x/shots/y", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/collections/x/shots/reorder", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/shoot-sessions", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/assets/x", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/assets/x/shots", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/assets/x/transcript", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/assets/x/thumbnail", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/assets/x/proxy", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/jobs", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/jobs/summary", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/cost/summary", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/issues", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/admin/worker-jobs/x", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/admin/worker-jobs/x/assignment", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/pipeline/run", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/pipeline/retry-failed", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/pipeline/resume-deferred", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/test-drive", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/test-drive/suggestions", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/pipeline/supervisor", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/pipeline/throttle", "trusted", "none", http.StatusForbidden},
		{http.MethodPut, "/api/v1/pipeline/throttle", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/storage/overview", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/search", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/search/shots", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/search/shots/hybrid", "trusted", "none", http.StatusForbidden},
		{http.MethodPost, "/api/v1/search/shots", "trusted", "JSON", http.StatusForbidden},
		{http.MethodGet, "/api/v1/shots/x/similar", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/discover/rare-shots", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/tags", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/tags/unresolved", "trusted", "none", http.StatusForbidden},
		{http.MethodPost, "/api/v1/tags/curate", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/tags/clusters", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/tags/proposals", "trusted", "none", http.StatusForbidden},
		{http.MethodPost, "/api/v1/tags/proposals/x/review", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/library/summary", "trusted", "none", http.StatusForbidden},
		{http.MethodPost, "/api/v1/library/summary/generate", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/repurpose/plans", "agent/admin", "JSON", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/repurpose/plans", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/repurpose/plans/x", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/repurpose/plans/x/revisions", "trusted", "none", http.StatusForbidden},
		{http.MethodPost, "/api/v1/repurpose/plans/x/revisions", "agent/admin", "JSON", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/repurpose/plans/x/revisions/1/approve", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/repurpose/plans/x/export.edl", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/repurpose/plans/x/export.fcpxml", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/hub/worker-setup/context", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/admin/hub/worker-setup/library-roots", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/hub/worker-binaries/linux-amd64", "trusted", "none", http.StatusForbidden},
		{http.MethodPost, "/api/v1/hub/worker-setup/script", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/unknown", "catch-all", "none", http.StatusNotFound},
		{http.MethodPost, "/api/v1/unknown", "catch-all", "none", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, nil)
			r.RemoteAddr = remoteAddr
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("guard=%s body=%s status=%d, want %d", tt.guard, tt.body, w.Code, tt.want)
			}
		})
	}
}

func TestAPIRouteInventoryIsComplete(t *testing.T) {
	server := NewServer("admin-token", newErrorEnvelopeTestService(t, "route-inventory"))
	specs := server.routeInventory()
	if got, want := len(specs), 112; got != want {
		t.Fatalf("route inventory contains %d routes, want %d", got, want)
	}
	expected := strings.Split(expectedRouteInventoryPatterns, "\n")
	if len(expected) != len(specs) {
		t.Fatalf("route inventory snapshot contains %d routes, want %d", len(expected), len(specs))
	}
	expectedPatterns := make(map[string]bool, len(expected))
	for _, pattern := range expected {
		expectedPatterns[pattern] = true
	}

	allowedAuth := map[routeAuthClass]bool{
		routeAuthPublic:         true,
		routeAuthTrustedRead:    true,
		routeAuthHubAdmin:       true,
		routeAuthAgentOrAdmin:   true,
		routeAuthWorker:         true,
		routeAuthWorkerEnroll:   true,
		routeAuthBrowserSession: true,
		routeAuthBrowserPage:    true,
		routeAuthCatchAll:       true,
	}
	seenNames := make(map[string]bool, len(specs))
	seenPatterns := make(map[string]bool, len(specs))
	for _, spec := range specs {
		if strings.TrimSpace(spec.Name) == "" {
			t.Error("route has empty name")
		}
		if strings.TrimSpace(spec.Pattern) == "" {
			t.Errorf("route %q has empty pattern", spec.Name)
		}
		if !allowedAuth[spec.Auth] {
			t.Errorf("route %q has unknown auth class %q", spec.Name, spec.Auth)
		}
		if spec.Handler == nil {
			t.Errorf("route %q has nil handler", spec.Name)
		}
		if seenNames[spec.Name] {
			t.Errorf("duplicate route name %q", spec.Name)
		}
		if seenPatterns[spec.Pattern] {
			t.Errorf("duplicate route pattern %q", spec.Pattern)
		}
		if !expectedPatterns[spec.Pattern] {
			t.Errorf("route %q is missing from the route inventory snapshot", spec.Pattern)
		}
		seenNames[spec.Name] = true
		seenPatterns[spec.Pattern] = true
	}
	for pattern := range expectedPatterns {
		if !seenPatterns[pattern] {
			t.Errorf("route inventory lost snapshot pattern %q", pattern)
		}
	}

	// Registering the same inventory in a fresh ServeMux catches overlapping
	// method/path patterns before a production request reaches them.
	mux := http.NewServeMux()
	server.registerRoutes(mux)
}

func TestAPIRouteInventoryAuthBoundaries(t *testing.T) {
	server := NewServer("admin-token", newErrorEnvelopeTestService(t, "route-auth-classes"))
	counts := make(map[routeAuthClass]int)
	for _, spec := range server.routeInventory() {
		counts[spec.Auth]++
	}
	for _, auth := range []routeAuthClass{
		routeAuthPublic,
		routeAuthTrustedRead,
		routeAuthHubAdmin,
		routeAuthAgentOrAdmin,
		routeAuthWorker,
		routeAuthWorkerEnroll,
		routeAuthBrowserSession,
		routeAuthBrowserPage,
		routeAuthCatchAll,
	} {
		if counts[auth] == 0 {
			t.Errorf("route inventory has no %q auth classification", auth)
		}
	}
}
