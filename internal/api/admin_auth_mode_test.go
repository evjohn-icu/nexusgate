package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

// newAdminAuthModeServer wires the real repository + app.Service + NewServer
// stack with hub_security.admin_auth pinned to the given mode. The mode is set
// directly on the config (not through config.Load) because the API tests build
// the service by hand; config.Load's normalization is covered in
// internal/config/admin_auth_test.go.
func newAdminAuthModeServer(t *testing.T, adminAuth string) (*Server, http.Handler) {
	t.Helper()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "admin-auth-mode.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(context.Background()); err != nil {
		repo.Close()
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}}
	cfg.HubSecurity.AdminAuth = adminAuth
	service, err := app.NewService(repo, cfg)
	if err != nil {
		repo.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	server := NewServer("", service)
	return server, server.Handler()
}

// TestAdminAuthModeRootWriteGuard drives POST /api/v1/roots — a requireHubAdmin
// route — through the three admin_auth modes. The waiver is decided from
// RemoteAddr alone, and a presented credential is never waived, so a wrong
// token on a trusted network still fails rather than silently succeeding as an
// anonymous admin.
func TestAdminAuthModeRootWriteGuard(t *testing.T) {
	cases := []struct {
		name       string
		adminAuth  string
		remoteAddr string
		authHeader string
		wantStatus int
	}{
		{
			name:       "required rejects unauthenticated even on trusted network",
			adminAuth:  "required",
			remoteAddr: "10.0.0.5:1234",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "trusted_network waives a trusted peer",
			adminAuth:  "trusted_network",
			remoteAddr: "10.0.0.5:1234",
			wantStatus: http.StatusCreated,
		},
		{
			name:       "trusted_network rejects an untrusted peer",
			adminAuth:  "trusted_network",
			remoteAddr: "203.0.113.9:1234",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "trusted_network never waives on a presented credential",
			adminAuth:  "trusted_network",
			remoteAddr: "10.0.0.5:1234",
			authHeader: "Bearer wrong-token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "off waives an untrusted peer",
			adminAuth:  "off",
			remoteAddr: "203.0.113.9:1234",
			wantStatus: http.StatusCreated,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, handler := newAdminAuthModeServer(t, tc.adminAuth)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/roots", strings.NewReader(`{"path":"`+t.TempDir()+`"}`))
			request.RemoteAddr = tc.remoteAddr
			if tc.authHeader != "" {
				request.Header.Set("Authorization", tc.authHeader)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.wantStatus, response.Body.String())
			}
		})
	}
}

// TestAdminAuthModeDoesNotRelaxWorkerTrustChain pins that the worker routes
// keep their own node-token trust chain in every admin_auth mode. A worker
// lease without a node token must stay 401 even when admin writes are fully
// open (off) and the peer is on a trusted network.
func TestAdminAuthModeDoesNotRelaxWorkerTrustChain(t *testing.T) {
	for _, adminAuth := range []string{"required", "trusted_network", "off"} {
		t.Run(adminAuth, func(t *testing.T) {
			_, handler := newAdminAuthModeServer(t, adminAuth)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/worker/lease", nil)
			request.RemoteAddr = "10.0.0.5:1234"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("worker lease without node token status=%d want=401 body=%s", response.Code, response.Body.String())
			}
		})
	}
}

// TestAdminAuthModeWaiverIssuesSessionAndRejectsCrossOrigin drives
// createAdminSession in waiver mode. A bare login with no token is admitted so
// the browser still receives its session + CSRF cookies and every downstream
// write stays behind requireSessionCSRF; the origin check is not relaxed, so a
// cross-origin login is still refused even though no password is required.
func TestAdminAuthModeWaiverIssuesSessionAndRejectsCrossOrigin(t *testing.T) {
	_, handler := newAdminAuthModeServer(t, "off")

	login := secureSessionRequest(http.MethodPost, "/api/v1/auth/admin/session", `{"token":""}`)
	login.Header.Set("Origin", "https://example.com")
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusCreated {
		t.Fatalf("waived login status=%d body=%s", loginResponse.Code, loginResponse.Body.String())
	}
	var session, csrf *http.Cookie
	for _, cookie := range loginResponse.Result().Cookies() {
		switch cookie.Name {
		case adminSessionCookie:
			session = cookie
		case adminCSRFCookie:
			csrf = cookie
		}
	}
	if session == nil || csrf == nil || session.Value == "" || csrf.Value == "" {
		t.Fatalf("waived login must still issue session and CSRF cookies: session=%v csrf=%v", session, csrf)
	}

	cross := secureSessionRequest(http.MethodPost, "/api/v1/auth/admin/session", `{"token":""}`)
	cross.Header.Set("Origin", "https://attacker.example")
	crossResponse := httptest.NewRecorder()
	handler.ServeHTTP(crossResponse, cross)
	if crossResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-origin waived login status=%d want=403 body=%s", crossResponse.Code, crossResponse.Body.String())
	}
}
