package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
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

// TestAdminSessionLoginGlobalCapBlocksAcrossKeys pins the API-002 global cap:
// when total failures across all keys cross adminLoginGlobalMaxFails, every
// key — including a fresh one presenting the correct token — is briefly
// blocked. Each source below gets a single failure, far under the per-key cap,
// so only the aggregate counter can be what trips the block.
func TestAdminSessionLoginGlobalCapBlocksAcrossKeys(t *testing.T) {
	server, handler := newAdminSessionTestServer(t)
	for i := 0; i < adminLoginGlobalMaxFails; i++ {
		request := secureSessionRequest(http.MethodPost, "/api/v1/auth/admin/session", `{"token":"wrong"}`)
		request.RemoteAddr = fmt.Sprintf("198.51.100.%d:1234", 10+i)
		request.Header.Set("Origin", "https://example.com")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d status=%d body=%s", i+1, response.Code, response.Body.String())
		}
	}
	// A fresh address whose own per-key count is zero — and even the correct
	// token — must now be refused: the global cap overrides every key.
	limited := secureSessionRequest(http.MethodPost, "/api/v1/auth/admin/session", `{"token":"`+server.service.AdminToken()+`"}`)
	limited.RemoteAddr = "198.51.100.99:4321"
	limited.Header.Set("Origin", "https://example.com")
	limitedResponse := httptest.NewRecorder()
	handler.ServeHTTP(limitedResponse, limited)
	if limitedResponse.Code != http.StatusTooManyRequests || limitedResponse.Header().Get("Retry-After") == "" {
		t.Fatalf("global cap status=%d retry-after=%q body=%s", limitedResponse.Code, limitedResponse.Header().Get("Retry-After"), limitedResponse.Body.String())
	}
}

// TestAdminLoginKeyHonorsForwardedClientOnlyBehindTrustedProxy pins the
// proxy-awareness of adminLoginKey: X-Forwarded-For / X-Real-IP are only ever
// trusted when the immediate peer is a configured trusted proxy, and are
// otherwise ignored (an arbitrary internet client cannot spoof the key).
func TestAdminLoginKeyHonorsForwardedClientOnlyBehindTrustedProxy(t *testing.T) {
	// A bare Server is enough: adminLoginKey only reads trustedProxyNetworks
	// and the request peer; no service or database is involved.
	server := &Server{}

	// No trusted proxy configured: the forwarded header must be ignored and
	// the RemoteAddr is the key.
	direct := httptest.NewRequest(http.MethodPost, "/", nil)
	direct.RemoteAddr = "203.0.113.10:1111"
	direct.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := server.adminLoginKey(direct); got != "203.0.113.10" {
		t.Fatalf("no-proxy key=%q, want RemoteAddr 203.0.113.10", got)
	}

	// Behind a trusted proxy: key on the forwarded client, not the proxy.
	server.SetTrustedProxyNetworks([]netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")})
	behind := httptest.NewRequest(http.MethodPost, "/", nil)
	behind.RemoteAddr = "203.0.113.10:1111"
	behind.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := server.adminLoginKey(behind); got != "198.51.100.7" {
		t.Fatalf("trusted-proxy key=%q, want forwarded client 198.51.100.7", got)
	}

	// A peer outside the trusted set still keys on RemoteAddr even with the
	// header present: an arbitrary internet client cannot spoof the key.
	spoofed := httptest.NewRequest(http.MethodPost, "/", nil)
	spoofed.RemoteAddr = "198.51.100.200:2222"
	spoofed.Header.Set("X-Forwarded-For", "127.0.0.1")
	if got := server.adminLoginKey(spoofed); got != "198.51.100.200" {
		t.Fatalf("non-proxy key=%q, want RemoteAddr 198.51.100.200", got)
	}
}

// TestAdminLoginRateLimitSeparatesClientsBehindTrustedProxy is the end-to-end
// behaviour the proxy-aware keying exists for: in the documented reverse-proxy
// deployment every request arrives from the proxy's RemoteAddr, so without
// proxy-aware keying five failures from one admin lock out every other admin.
// With the proxy configured, one client's per-key budget must not affect
// another client behind the same proxy.
func TestAdminLoginRateLimitSeparatesClientsBehindTrustedProxy(t *testing.T) {
	server, handler := newAdminSessionTestServer(t)
	server.SetTrustedProxyNetworks([]netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")})
	proxyLogin := func(clientAddr, token string) *http.Request {
		request := secureSessionRequest(http.MethodPost, "/api/v1/auth/admin/session", `{"token":"`+token+`"}`)
		request.RemoteAddr = "203.0.113.5:443"
		request.Header.Set("X-Forwarded-For", clientAddr)
		request.Header.Set("Origin", "https://example.com")
		return request
	}
	// Five failures from one client behind the proxy.
	for i := 0; i < adminLoginMaxFails; i++ {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, proxyLogin("198.51.100.10", "wrong"))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("client A failure %d status=%d body=%s", i+1, response.Code, response.Body.String())
		}
	}
	// A second client behind the same proxy must still be able to log in:
	// its own per-key budget is untouched.
	ok := httptest.NewRecorder()
	handler.ServeHTTP(ok, proxyLogin("198.51.100.11", server.service.AdminToken()))
	if ok.Code != http.StatusCreated {
		t.Fatalf("client B blocked by client A's failures: status=%d body=%s", ok.Code, ok.Body.String())
	}
}
