package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

func newAdminSessionTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "admin-session.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(context.Background()); err != nil {
		repo.Close()
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		repo.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	server := NewServer("", service)
	return server, server.Handler()
}

func secureSessionRequest(method, target string, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.TLS = &tls.ConnectionState{}
	return r
}

func TestAdminSessionLoginSetsOpaqueSecureCookies(t *testing.T) {
	server, handler := newAdminSessionTestServer(t)
	request := secureSessionRequest(http.MethodPost, "/api/v1/auth/admin/session", `{"token":"`+server.service.AdminToken()+`"}`)
	request.Header.Set("Origin", "https://example.com")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("login status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), server.service.AdminToken()) {
		t.Fatal("admin token appeared in session response")
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("cookies=%v, want session and CSRF cookies", cookies)
	}
	var session, csrf *http.Cookie
	for _, cookie := range cookies {
		switch cookie.Name {
		case adminSessionCookie:
			session = cookie
		case adminCSRFCookie:
			csrf = cookie
		}
	}
	if session == nil || csrf == nil || session.Value == "" || csrf.Value == "" || session.Value == server.service.AdminToken() {
		t.Fatalf("unexpected session cookies: session=%v csrf=%v", session, csrf)
	}
	if !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteStrictMode || session.Path != "/" {
		t.Fatalf("session cookie flags=%+v", session)
	}
	if csrf.HttpOnly || !csrf.Secure || csrf.SameSite != http.SameSiteStrictMode || csrf.Path != "/" {
		t.Fatalf("CSRF cookie flags=%+v", csrf)
	}
}

func TestAdminSessionAllowsGetAndRequiresOriginAndCSRFForWrites(t *testing.T) {
	server, handler := newAdminSessionTestServer(t)
	login := secureSessionRequest(http.MethodPost, "/api/v1/auth/admin/session", `{"token":"`+server.service.AdminToken()+`"}`)
	login.Header.Set("Origin", "https://example.com")
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusCreated {
		t.Fatalf("login status=%d body=%s", loginResponse.Code, loginResponse.Body.String())
	}
	cookies := loginResponse.Result().Cookies()
	var cookieHeader, csrf string
	for _, cookie := range cookies {
		cookieHeader += cookie.Name + "=" + cookie.Value + "; "
		if cookie.Name == adminCSRFCookie {
			csrf = cookie.Value
		}
	}

	get := secureSessionRequest(http.MethodGet, "/api/v1/roots", "")
	get.Header.Set("Cookie", cookieHeader)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("session GET status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}

	write := secureSessionRequest(http.MethodPost, "/api/v1/roots", `{"path":"/tmp"}`)
	write.Header.Set("Cookie", cookieHeader)
	write.Header.Set("Origin", "https://example.com")
	noCSRF := httptest.NewRecorder()
	handler.ServeHTTP(noCSRF, write)
	if noCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s", noCSRF.Code, noCSRF.Body.String())
	}

	write = secureSessionRequest(http.MethodPost, "/api/v1/roots", `{"path":"/tmp"}`)
	write.Header.Set("Cookie", cookieHeader)
	write.Header.Set("Origin", "https://example.com")
	write.Header.Set("X-CSRF-Token", csrf)
	withCSRF := httptest.NewRecorder()
	handler.ServeHTTP(withCSRF, write)
	if withCSRF.Code != http.StatusCreated {
		t.Fatalf("session write status=%d body=%s", withCSRF.Code, withCSRF.Body.String())
	}
}

func TestAdminSessionCannotBeUsedOverHTTPOrByWorkerRoute(t *testing.T) {
	server, handler := newAdminSessionTestServer(t)
	login := secureSessionRequest(http.MethodPost, "/api/v1/auth/admin/session", `{"token":"`+server.service.AdminToken()+`"}`)
	login.Header.Set("Origin", "https://example.com")
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, login)
	var cookieHeader string
	for _, cookie := range loginResponse.Result().Cookies() {
		cookieHeader += cookie.Name + "=" + cookie.Value + "; "
	}

	httpRequest := httptest.NewRequest(http.MethodGet, "/api/v1/roots", nil)
	httpRequest.Header.Set("Cookie", cookieHeader)
	httpResponse := httptest.NewRecorder()
	handler.ServeHTTP(httpResponse, httpRequest)
	if httpResponse.Code != http.StatusUnauthorized {
		t.Fatalf("HTTP session status=%d body=%s", httpResponse.Code, httpResponse.Body.String())
	}

	worker := secureSessionRequest(http.MethodPost, "/api/v1/worker/heartbeat", `{}`)
	worker.Header.Set("Cookie", cookieHeader)
	workerResponse := httptest.NewRecorder()
	handler.ServeHTTP(workerResponse, worker)
	if workerResponse.Code == http.StatusOK || workerResponse.Code == http.StatusCreated {
		t.Fatalf("admin session unexpectedly authenticated worker route: status=%d body=%s", workerResponse.Code, workerResponse.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(workerResponse.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
}

func TestAdminSessionLogoutRevokesSession(t *testing.T) {
	server, handler := newAdminSessionTestServer(t)
	login := secureSessionRequest(http.MethodPost, "/api/v1/auth/admin/session", `{"token":"`+server.service.AdminToken()+`"}`)
	login.Header.Set("Origin", "https://example.com")
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, login)
	var cookieHeader, csrf string
	for _, cookie := range loginResponse.Result().Cookies() {
		cookieHeader += cookie.Name + "=" + cookie.Value + "; "
		if cookie.Name == adminCSRFCookie {
			csrf = cookie.Value
		}
	}
	logout := secureSessionRequest(http.MethodDelete, "/api/v1/auth/admin/session", "")
	logout.Header.Set("Cookie", cookieHeader)
	logout.Header.Set("Origin", "https://example.com")
	logout.Header.Set("X-CSRF-Token", csrf)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, logout)
	if logoutResponse.Code != http.StatusOK {
		t.Fatalf("logout status=%d body=%s", logoutResponse.Code, logoutResponse.Body.String())
	}
	get := secureSessionRequest(http.MethodGet, "/api/v1/roots", "")
	get.Header.Set("Cookie", cookieHeader)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}
}

func TestAdminSessionLoginRejectsCrossOriginRequest(t *testing.T) {
	server, handler := newAdminSessionTestServer(t)
	request := secureSessionRequest(http.MethodPost, "/api/v1/auth/admin/session", `{"token":"`+server.service.AdminToken()+`"}`)
	request.Header.Set("Origin", "https://attacker.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAdminSessionLoginRateLimitAndSessionCap(t *testing.T) {
	server, handler := newAdminSessionTestServer(t)
	for i := 0; i < adminLoginMaxFails; i++ {
		request := secureSessionRequest(http.MethodPost, "/api/v1/auth/admin/session", `{"token":"wrong"}`)
		request.RemoteAddr = "198.51.100.20:1234"
		request.Header.Set("Origin", "https://example.com")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d status=%d body=%s", i+1, response.Code, response.Body.String())
		}
	}
	limited := secureSessionRequest(http.MethodPost, "/api/v1/auth/admin/session", `{"token":"`+server.service.AdminToken()+`"}`)
	limited.RemoteAddr = "198.51.100.20:4321"
	limited.Header.Set("Origin", "https://example.com")
	limitedResponse := httptest.NewRecorder()
	handler.ServeHTTP(limitedResponse, limited)
	if limitedResponse.Code != http.StatusTooManyRequests || limitedResponse.Header().Get("Retry-After") == "" {
		t.Fatalf("rate limit status=%d retry-after=%q body=%s", limitedResponse.Code, limitedResponse.Header().Get("Retry-After"), limitedResponse.Body.String())
	}

	for i := 0; i < adminSessionLimit+1; i++ {
		if _, _, err := server.createAdminSessionRecord(time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	server.adminSessionsMu.Lock()
	count := len(server.adminSessions)
	server.adminSessionsMu.Unlock()
	if count != adminSessionLimit {
		t.Fatalf("session count=%d, want cap %d", count, adminSessionLimit)
	}
}
