package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type apiRouteInventoryEntry struct {
	method, path, guard, body string
	want                      int
}

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
		{http.MethodGet, "/api/v1/admin/provider-channels", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/admin/provider-channels/status", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/admin/provider-channels", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodPatch, "/api/v1/admin/provider-channels/x", "admin", "JSON", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/admin/provider-channels/x/enable", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/admin/provider-channels/x/disable", "admin", "none", http.StatusUnauthorized},
		{http.MethodDelete, "/api/v1/admin/provider-channels/x", "admin", "none", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/admin/provider-channels/x/test", "admin", "none", http.StatusUnauthorized},
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
		{http.MethodGet, "/api/v1/repurpose/plans/x", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/repurpose/plans/x/revisions", "trusted", "none", http.StatusForbidden},
		{http.MethodPost, "/api/v1/repurpose/plans/x/revisions", "agent/admin", "JSON", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/repurpose/plans/x/revisions/1/approve", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/repurpose/plans/x/export.edl", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/repurpose/plans/x/export.fcpxml", "admin", "none", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/hub/worker-setup/context", "trusted", "none", http.StatusForbidden},
		{http.MethodGet, "/api/v1/hub/worker-binaries/linux-amd64", "trusted", "none", http.StatusForbidden},
		{http.MethodPost, "/api/v1/hub/worker-setup/script", "admin", "JSON", http.StatusUnauthorized},
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
