package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// newRootsHealthFixture opens a real sqlite-backed service (the api tests'
// standard wiring: repository + app.Service + NewServer) and seeds roots in
// the three health states the endpoint must distinguish: one that was healthy
// and went unavailable (last_healthy_at preserved by MarkRootUnavailable),
// one that is currently healthy, and one that was never scanned.
func newRootsHealthFixture(t *testing.T, name string, adminAuth ...string) (*app.Service, *sqlite.Repository) {
	t.Helper()
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	healthyAt := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	downAt := time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC)

	down, err := repo.CreateLibraryRoot(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkRootHealthy(ctx, down.ID, healthyAt); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkRootUnavailable(ctx, down.ID, downAt); err != nil {
		t.Fatal(err)
	}
	ok, err := repo.CreateLibraryRoot(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkRootHealthy(ctx, ok.ID, healthyAt); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateLibraryRoot(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}}
	if len(adminAuth) > 0 {
		cfg.HubSecurity.AdminAuth = adminAuth[0]
	}
	service, err := app.NewService(repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return service, repo
}

// GET /api/v1/roots/health is the compact projection the UIs render: the
// verdict plus the two timestamps, with an unavailable root keeping its
// last_healthy_at — MarkRootUnavailable never clears it, and this endpoint is
// what turns that decision into a "上次正常" display during an outage.
func TestRootsHealthMapsFieldsAndPreservesLastHealthy(t *testing.T) {
	service, repo := newRootsHealthFixture(t, "roots-health-mapping.db")
	ctx := context.Background()
	roots, err := repo.ListLibraryRoots(ctx)
	if err != nil || len(roots) != 3 {
		t.Fatalf("roots=%d err=%v", len(roots), err)
	}
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodGet, "/api/v1/roots/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var health []RootHealth
	if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if len(health) != 3 {
		t.Fatalf("health rows=%d, want 3", len(health))
	}
	byID := map[string]RootHealth{}
	for _, h := range health {
		if h.RootID == "" || h.Path == "" {
			t.Fatalf("root %+v missing root_id or path", h)
		}
		byID[h.RootID] = h
	}
	down := byID[roots[0].ID]
	if down.State != string(domain.RootHealthUnavailable) {
		t.Fatalf("state=%q, want unavailable", down.State)
	}
	if down.LastHealthy == nil {
		t.Fatal("an unavailable root must keep last_healthy_at so the UI can show when it last worked")
	}
	if want := "2026-08-01T10:00:00Z"; *down.LastHealthy != want {
		t.Fatalf("last_healthy_at=%q, want %q", *down.LastHealthy, want)
	}
	if down.LastScan == nil || *down.LastScan != "2026-08-02T09:30:00Z" {
		t.Fatalf("last_scan_at=%v, want the unavailable scan time", down.LastScan)
	}
	ok := byID[roots[1].ID]
	if ok.State != string(domain.RootHealthHealthy) {
		t.Fatalf("state=%q, want healthy", ok.State)
	}
	if ok.LastHealthy == nil || ok.LastScan == nil {
		t.Fatalf("a healthy root must carry both timestamps: %+v", ok)
	}
	// warning_details is the additive structured form of warnings; when the
	// environment's mount table yields any, their codes must stay within the
	// four documented stable codes the browser catalogs translate.
	knownCodes := map[string]bool{
		"root.empty_unmounted": true, "root.network_mount": true,
		"root.staging_copy_disabled": true, "root.mount_writable": true,
	}
	for _, h := range health {
		for _, d := range h.WarningDetails {
			if !knownCodes[d.Code] {
				t.Fatalf("root %s carries unexpected warning code %q", h.RootID, d.Code)
			}
			if d.Message == "" {
				t.Fatalf("root %s warning %q has an empty message", h.RootID, d.Code)
			}
		}
	}
}

// The endpoint's auth posture must match listRoots: a root path is hub
// infrastructure, so neither listing may answer an unauthenticated caller
// from the LAN. TestInspectRootRejectsUnauthenticatedRequest pins the same
// boundary for the inspect route; this one pins it for the read side.
func TestRootsHealthAuthPostureMatchesListRoots(t *testing.T) {
	service, _ := newRootsHealthFixture(t, "roots-health-auth.db", "required")
	handler := NewServer("", service).Handler()
	for _, target := range []string{"/api/v1/roots", "/api/v1/roots/health"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, lanRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a token: status=%d, want 401", target, response.Code)
		}
		authorized := httptest.NewRecorder()
		handler.ServeHTTP(authorized, hubAdminRequest(service, http.MethodGet, target, nil))
		if authorized.Code != http.StatusOK {
			t.Fatalf("%s with the admin token: status=%d, want 200", target, authorized.Code)
		}
	}
}

// listRoots serializes domain.LibraryRoot directly, so the health fields
// (health_state, last_healthy_at, last_scan_at) must reach GET /api/v1/roots
// under their domain JSON names — the roots/health projection is an addition
// for the UIs, not a duplicate data path.
func TestListRootsSerializesRootHealthFields(t *testing.T) {
	service, _ := newRootsHealthFixture(t, "roots-list-fields.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodGet, "/api/v1/roots", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var roots []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &roots); err != nil {
		t.Fatal(err)
	}
	if len(roots) != 3 {
		t.Fatalf("roots=%d, want 3", len(roots))
	}
	seenUnavailable, seenHealthy, seenUnknown := false, false, false
	for _, root := range roots {
		state, _ := root["health_state"].(string)
		if state == "" {
			t.Fatalf("root %v missing health_state", root["id"])
		}
		// time.Time marshals via encoding/json; the endpoint's RFC3339Nano
		// strings must equal what the raw list carries for the same instant.
		healthy, hasHealthy := root["last_healthy_at"]
		scan, hasScan := root["last_scan_at"]
		switch state {
		case string(domain.RootHealthUnavailable):
			seenUnavailable = true
			if !hasHealthy || !hasScan {
				t.Fatalf("unavailable root %v must keep last_healthy_at and last_scan_at", root["id"])
			}
		case string(domain.RootHealthHealthy):
			seenHealthy = true
			if !hasHealthy || !hasScan {
				t.Fatalf("healthy root %v must carry both timestamps", root["id"])
			}
		case string(domain.RootHealthUnknown):
			seenUnknown = true
			// omitempty drops nil *time.Time; an unscanned root has neither.
			if hasHealthy || hasScan {
				t.Fatalf("unknown root %v must omit both timestamps, got healthy=%v scan=%v", root["id"], healthy, scan)
			}
		default:
			t.Fatalf("root %v has unexpected health_state %q", root["id"], state)
		}
	}
	if !seenUnavailable || !seenHealthy || !seenUnknown {
		t.Fatalf("fixture did not exercise all three states: unavailable=%v healthy=%v unknown=%v", seenUnavailable, seenHealthy, seenUnknown)
	}
}
