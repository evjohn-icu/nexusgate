package hubproxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testOrigin = "http://wails.localhost"

func newTestProxy(t *testing.T, upstream *httptest.Server) *Proxy {
	t.Helper()
	parsed, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := New(Config{
		Upstream:   parsed,
		Bearer:     "hub-secret",
		Origin:     testOrigin,
		CSRFHeader: "X-NexusGate-Desktop",
		CSRFNonce:  "nonce-1",
		Routes: []Route{
			{Method: "GET", Path: "/api/v1/assets/{id}/timeline"},
			{Method: "POST", Path: "/api/v1/search/shots"},
			{Method: "GET", Path: "/api/v1/assets/{id}/proxy"},
			{Method: "GET", Path: "/search"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return proxy
}

func doProxyRequest(proxy *Proxy, method, target string, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.RequestURI = target
	if mutate != nil {
		mutate(request)
	}
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	return recorder
}

func TestProxyInjectsBearerAndStripsClientCredential(t *testing.T) {
	var seenAuthorization, seenClientCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuthorization = r.Header.Get("Authorization")
		seenClientCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	proxy := newTestProxy(t, upstream)
	recorder := doProxyRequest(proxy, http.MethodGet, "/api/v1/assets/a/timeline", "",
		func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer attacker")
			r.Header.Set("Cookie", "session=attacker")
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if seenAuthorization != "Bearer hub-secret" {
		t.Fatalf("upstream saw %q", seenAuthorization)
	}
	if seenClientCookie != "" {
		t.Fatalf("client cookie reached the upstream: %q", seenClientCookie)
	}
}

func TestProxyRejectsForeignOriginAndWrongNonce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the upstream must not be reached")
	}))
	defer upstream.Close()
	proxy := newTestProxy(t, upstream)

	noOrigin := doProxyRequest(proxy, http.MethodPost, "/api/v1/search/shots", `{}`, nil)
	if noOrigin.Code != http.StatusForbidden {
		t.Fatalf("unsafe request without origin: status=%d", noOrigin.Code)
	}
	foreign := doProxyRequest(proxy, http.MethodPost, "/api/v1/search/shots", `{}`, func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("X-NexusGate-Desktop", "nonce-1")
	})
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("foreign origin: status=%d", foreign.Code)
	}
	wrongNonce := doProxyRequest(proxy, http.MethodPost, "/api/v1/search/shots", `{}`, func(r *http.Request) {
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("X-NexusGate-Desktop", "nonce-2")
	})
	if wrongNonce.Code != http.StatusForbidden {
		t.Fatalf("wrong nonce: status=%d", wrongNonce.Code)
	}
	nullOrigin := doProxyRequest(proxy, http.MethodPost, "/api/v1/search/shots", `{}`, func(r *http.Request) {
		r.Header.Set("Origin", "null")
		r.Header.Set("X-NexusGate-Desktop", "nonce-1")
	})
	if nullOrigin.Code != http.StatusForbidden {
		t.Fatalf("null origin: status=%d", nullOrigin.Code)
	}
}

func TestProxyRejectsAbsoluteFormAndTraversal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("the upstream must not be reached: %s", r.URL.String())
	}))
	defer upstream.Close()
	proxy := newTestProxy(t, upstream)

	absolute := httptest.NewRequest(http.MethodGet, "http://evil.example/api/v1/assets/a/timeline", nil)
	absolute.RequestURI = "http://evil.example/api/v1/assets/a/timeline"
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, absolute)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("absolute-form request: status=%d", recorder.Code)
	}

	traversal := doProxyRequest(proxy, http.MethodGet, "/api/v1/assets/../../etc/passwd", "", nil)
	if traversal.Code != http.StatusForbidden {
		t.Fatalf("traversal: status=%d", traversal.Code)
	}

	escape := doProxyRequest(proxy, http.MethodGet, "/api/v1/assets/a/timeline/../proxy", "", nil)
	if escape.Code != http.StatusForbidden {
		t.Fatalf("dot segment: status=%d", escape.Code)
	}
}

func TestProxyRefusesRoutesOutsideTheAllowlist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the upstream must not be reached")
	}))
	defer upstream.Close()
	proxy := newTestProxy(t, upstream)
	for _, target := range []string{
		"/api/v1/worker/lease",
		"/api/v1/worker/jobs/x/inference-result",
		"/api/v1/admin/speaker-profiles",
		"/health",
	} {
		recorder := doProxyRequest(proxy, http.MethodGet, target, "", func(r *http.Request) {
			r.Header.Set("Origin", testOrigin)
		})
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s: status=%d, want 403", target, recorder.Code)
		}
	}
}

func TestProxyForwardsRangeAndPreservesPartialContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-3" {
			t.Errorf("upstream did not receive the range: %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", "bytes 0-3/100")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("0123"))
	}))
	defer upstream.Close()
	proxy := newTestProxy(t, upstream)
	recorder := doProxyRequest(proxy, http.MethodGet, "/api/v1/assets/a/proxy", "", func(r *http.Request) {
		r.Header.Set("Range", "bytes=0-3")
	})
	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status=%d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 0-3/100" {
		t.Fatalf("Content-Range=%q", got)
	}
	if recorder.Body.String() != "0123" {
		t.Fatalf("body=%q", recorder.Body.String())
	}
}

func TestProxyDoesNotFollowRedirects(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/steal", http.StatusFound)
	}))
	defer upstream.Close()
	proxy := newTestProxy(t, upstream)
	recorder := doProxyRequest(proxy, http.MethodGet, "/api/v1/assets/a/timeline", "", nil)
	if recorder.Code != http.StatusFound {
		t.Fatalf("status=%d, want the 302 handed back", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != "https://evil.example/steal" {
		t.Fatalf("Location=%q", location)
	}
	// The upstream's own 302 body is passed through; what must not happen is
	// following the 302 with the credential attached. The proxy's client never
	// follows, so the foreign host is never contacted with a bearer token.
}

func TestProxyRejectsUpgrades(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the upstream must not be reached")
	}))
	defer upstream.Close()
	proxy := newTestProxy(t, upstream)
	recorder := doProxyRequest(proxy, http.MethodGet, "/api/v1/assets/a/timeline", "", func(r *http.Request) {
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("upgrade: status=%d", recorder.Code)
	}
}

func TestNewRefusesIncompleteConfiguration(t *testing.T) {
	upstream, _ := url.Parse("https://hub.example")
	cases := []Config{
		{Upstream: upstream, Origin: testOrigin, CSRFHeader: "X", CSRFNonce: "n"},
		{Upstream: upstream, Bearer: "b", Origin: testOrigin, CSRFHeader: "X"},
		{Upstream: nil, Bearer: "b", Origin: testOrigin, CSRFHeader: "X", CSRFNonce: "n"},
	}
	for i, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("case %d must be rejected", i)
		}
	}
	insecure, _ := url.Parse("http://hub.example")
	if _, err := New(Config{Upstream: insecure, Bearer: "b", Origin: testOrigin, CSRFHeader: "X", CSRFNonce: "n"}); err == nil {
		t.Error("a non-loopback plaintext upstream must be rejected")
	}
}
