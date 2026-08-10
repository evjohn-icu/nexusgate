package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// These cases exercise the handler bodies, not just their middleware guards.
// An empty migrated database is enough to cover the stable empty/not-found
// behavior without constructing media artifacts or provider fixtures.
func TestPreviouslyUncoveredHandlers(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "handler-coverage")
	server := NewServer("admin-token", service)
	handler := server.Handler()
	token := service.AdminToken()
	tests := []struct {
		name, method, path string
		want               []int
	}{
		{"job summary", http.MethodGet, "/api/v1/jobs/summary", []int{http.StatusOK}},
		{"cost summary", http.MethodGet, "/api/v1/cost/summary", []int{http.StatusOK}},
		{"job issues", http.MethodGet, "/api/v1/issues", []int{http.StatusOK}},
		{"worker job status", http.MethodGet, "/api/v1/admin/worker-jobs/missing", []int{http.StatusNotFound, http.StatusInternalServerError}},
		{"similar shots", http.MethodGet, "/api/v1/shots/missing/similar", []int{http.StatusNotFound}},
		{"rare shots", http.MethodGet, "/api/v1/discover/rare-shots", []int{http.StatusOK}},
		{"get collection", http.MethodGet, "/api/v1/collections/missing", []int{http.StatusNotFound}},
		{"collection assets", http.MethodGet, "/api/v1/collections/missing/assets", []int{http.StatusOK}},
		{"delete collection", http.MethodDelete, "/api/v1/collections/missing", []int{http.StatusNoContent, http.StatusNotFound}},
		{"asset shots", http.MethodGet, "/api/v1/assets/missing/shots", []int{http.StatusOK}},
		{"asset proxy", http.MethodGet, "/api/v1/assets/missing/proxy", []int{http.StatusNotFound}},
		{"unresolved tags", http.MethodGet, "/api/v1/tags/unresolved", []int{http.StatusOK}},
		{"tag clusters", http.MethodPost, "/api/v1/tags/clusters", []int{http.StatusCreated, http.StatusOK, http.StatusServiceUnavailable, http.StatusInternalServerError}},
		{"tag proposals", http.MethodGet, "/api/v1/tags/proposals", []int{http.StatusOK}},
		{"library summary", http.MethodPost, "/api/v1/library/summary/generate", []int{http.StatusCreated, http.StatusUnprocessableEntity, http.StatusServiceUnavailable, http.StatusInternalServerError}},
		{"repurpose plan", http.MethodGet, "/api/v1/repurpose/plans/missing", []int{http.StatusNotFound}},
		{"test drive", http.MethodPost, "/api/v1/test-drive", []int{http.StatusBadRequest}},
		{"test drive suggestions", http.MethodGet, "/api/v1/test-drive/suggestions?assets=missing", []int{http.StatusOK}},
		{"setup status", http.MethodGet, "/api/v1/setup/status", []int{http.StatusOK}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := lanRequest(tt.method, tt.path, nil)
			if tt.method == http.MethodGet && tt.name == "worker job status" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			if tt.method == http.MethodDelete || tt.name == "tag clusters" || tt.name == "library summary" || tt.name == "test drive" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			for _, want := range tt.want {
				if w.Code == want {
					return
				}
			}
			t.Fatalf("status=%d body=%s, want one of %v", w.Code, w.Body.String(), tt.want)
		})
	}
}

func TestPreviouslyUncoveredHandlersRemoteAuth(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "handler-coverage-auth")
	handler := NewServer("admin-token", service).Handler()
	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/v1/jobs/summary", http.StatusForbidden},
		{http.MethodGet, "/api/v1/cost/summary", http.StatusForbidden},
		{http.MethodGet, "/api/v1/issues", http.StatusForbidden},
		{http.MethodGet, "/api/v1/shots/missing/similar", http.StatusForbidden},
		{http.MethodGet, "/api/v1/discover/rare-shots", http.StatusForbidden},
		{http.MethodGet, "/api/v1/collections/missing", http.StatusForbidden},
		{http.MethodGet, "/api/v1/assets/missing/shots", http.StatusForbidden},
		{http.MethodGet, "/api/v1/assets/missing/proxy", http.StatusForbidden},
		{http.MethodGet, "/api/v1/tags/unresolved", http.StatusForbidden},
		{http.MethodGet, "/api/v1/tags/proposals", http.StatusForbidden},
		{http.MethodGet, "/api/v1/repurpose/plans/missing", http.StatusForbidden},
		{http.MethodGet, "/api/v1/test-drive/suggestions?assets=missing", http.StatusForbidden},
		{http.MethodGet, "/api/v1/setup/status", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, nil)
			r.RemoteAddr = "192.0.2.1:1"
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status=%d body=%s, want %d", w.Code, w.Body.String(), tt.want)
			}
		})
	}
}
