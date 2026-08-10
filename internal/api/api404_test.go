package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

func newAPICatchAllTestService(t *testing.T) *app.Service {
	t.Helper()
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "api404.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// TestAPIUnknownRouteJSON404 pins the contract for unmatched /api/v1/* paths:
// a JSON 404 envelope, so a client can tell "no such endpoint" apart from an
// empty result list without sniffing Content-Type.
func TestAPIUnknownRouteJSON404(t *testing.T) {
	handler := NewServer("", newAPICatchAllTestService(t)).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/definitely-not-a-route", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type=%q, want application/json", got)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, `"code":"not_found"`) {
		t.Fatalf("body=%s, want JSON envelope with error code not_found", body)
	}
}

// TestUnknownPagePathKeepsPlain404 pins the other side of the boundary: page
// routes that do not exist keep the mux's default plain-text 404, untouched
// by the API catch-all.
func TestUnknownPagePathKeepsPlain404(t *testing.T) {
	handler := NewServer("", newAPICatchAllTestService(t)).Handler()
	request := httptest.NewRequest(http.MethodGet, "/definitely-not-a-page", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

// TestAPICatchAllDoesNotShadowRealRoutes guards the catch-all against
// swallowing a registered endpoint: more specific patterns must keep winning.
func TestAPICatchAllDoesNotShadowRealRoutes(t *testing.T) {
	handler := NewServer("", newAPICatchAllTestService(t)).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, `"ok"`) {
		t.Fatalf("body=%s, want health response", body)
	}
}
