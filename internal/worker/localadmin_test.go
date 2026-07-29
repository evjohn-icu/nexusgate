package worker

import (
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ev/timingdex/internal/remote"
)

func writeWorkerConfig(t *testing.T, config Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := SaveConfig(path, config); err != nil {
		t.Fatal(err)
	}
	return path
}

func enrolledConfig() Config {
	return Config{
		HubURL:                 "https://nas:8787",
		CertificateFingerprint: strings.Repeat("ab", 32),
		Token:                  "node-token-secret-value",
		CacheDir:               `D:\cache`,
		Mounts:                 map[string]string{"root-1": `D:\NAS\Footage`},
		Registration:           remote.WorkerRegistration{Name: "studio-windows"},
	}
}

func newTestAdmin(t *testing.T, path string) *LocalAdmin {
	t.Helper()
	admin, err := NewLocalAdmin(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	return admin
}

// The settings server exists because three values cannot be pushed from the Hub:
// the hub URL, the pinned fingerprint and the node token are what a Worker needs
// in order to talk to the Hub at all. Everything else is Hub-decided.
func TestLocalAdminServesOnlyLoopback(t *testing.T) {
	admin := newTestAdmin(t, writeWorkerConfig(t, enrolledConfig()))
	host, _, err := net.SplitHostPort(admin.Address())
	if err != nil {
		t.Fatal(err)
	}
	address, err := net.ResolveIPAddr("ip", host)
	if err != nil {
		t.Fatal(err)
	}
	// Binding anything routable would expose the node token to the whole network.
	if !address.IP.IsLoopback() {
		t.Fatalf("settings server must bind loopback only, got %s", host)
	}
}

// The URL token is what stops any other local process from reading the node
// token out of a listener bound to a well-known interface.
func TestLocalAdminRejectsRequestsWithoutTheURLToken(t *testing.T) {
	admin := newTestAdmin(t, writeWorkerConfig(t, enrolledConfig()))
	handler := admin.Handler()

	for _, path := range []string{"/", "/config", "/s/", "/s/wrong-token/config", "/s/wrong-token/", admin.pathToken[:len(admin.pathToken)-1]} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/"+strings.TrimPrefix(path, "/"), nil))
		// 404 rather than 401: a 401 would confirm that a settings server is
		// listening on this port, which is information a prober should not get.
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s: status=%d want 404", path, response.Code)
		}
	}

	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, httptest.NewRequest(http.MethodGet, admin.pathPrefix()+"/config", nil))
	if authorized.Code != http.StatusOK {
		t.Fatalf("correct token status=%d body=%s", authorized.Code, authorized.Body.String())
	}
}

// The node token is a credential. It may be replaced but never read back — the
// same rule the Hub applies to its own tokens and provider keys.
func TestLocalAdminNeverDisclosesTheNodeToken(t *testing.T) {
	config := enrolledConfig()
	admin := newTestAdmin(t, writeWorkerConfig(t, config))
	handler := admin.Handler()

	for _, target := range []string{"/config", "/"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, admin.pathPrefix()+target, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d", target, response.Code)
		}
		if strings.Contains(response.Body.String(), config.Token) {
			t.Fatalf("%s disclosed the node token", target)
		}
	}

	var view struct {
		HubURL          string `json:"hub_url"`
		Fingerprint     string `json:"certificate_fingerprint"`
		TokenHint       string `json:"token_hint"`
		TokenConfigured bool   `json:"token_configured"`
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, admin.pathPrefix()+"/config", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	// The hub URL and fingerprint are not secret: the fingerprint is the public
	// half of a certificate every client already sees.
	if view.HubURL != config.HubURL || view.Fingerprint != config.CertificateFingerprint {
		t.Fatalf("non-secret fields must be shown: %+v", view)
	}
	if !view.TokenConfigured {
		t.Fatal("the page has to be able to say whether a token exists")
	}
	if view.TokenHint == "" || strings.Contains(config.Token, view.TokenHint) && len(view.TokenHint) > 6 {
		t.Fatalf("token_hint=%q must identify without revealing", view.TokenHint)
	}
}

// Editing the hub URL must not require retyping the token, and must not silently
// discard the mounts or the registration the page never shows.
func TestLocalAdminUpdatePreservesUneditedFields(t *testing.T) {
	original := enrolledConfig()
	path := writeWorkerConfig(t, original)
	admin := newTestAdmin(t, path)
	handler := admin.Handler()

	body := `{"hub_url":"https://nas2:9000","certificate_fingerprint":"` + strings.Repeat("cd", 32) + `","token":""}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPut, admin.pathPrefix()+"/config", strings.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	saved, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.HubURL != "https://nas2:9000" || saved.CertificateFingerprint != strings.Repeat("cd", 32) {
		t.Fatalf("edit did not apply: %+v", saved)
	}
	if saved.Token != original.Token {
		t.Fatal("an empty token field means keep the existing one, not erase it")
	}
	if saved.Mounts["root-1"] != original.Mounts["root-1"] {
		t.Fatalf("mounts were dropped: %+v", saved.Mounts)
	}
	if saved.Registration.Name != original.Registration.Name {
		t.Fatalf("registration was dropped: %+v", saved.Registration)
	}
	if saved.CacheDir != original.CacheDir {
		t.Fatalf("cache dir was dropped: %q", saved.CacheDir)
	}
}

func TestLocalAdminReplacesTokenWhenSupplied(t *testing.T) {
	path := writeWorkerConfig(t, enrolledConfig())
	admin := newTestAdmin(t, path)

	body := `{"hub_url":"https://nas:8787","certificate_fingerprint":"","token":"  replacement-token  "}`
	response := httptest.NewRecorder()
	admin.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPut, admin.pathPrefix()+"/config", strings.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	saved, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Token != "replacement-token" {
		t.Fatalf("token=%q (pasted values carry whitespace and must be trimmed)", saved.Token)
	}
	// Clearing the fingerprint is legitimate: a Hub running with TLS off has none
	// to pin.
	if saved.CertificateFingerprint != "" {
		t.Fatalf("fingerprint should have been cleared, got %q", saved.CertificateFingerprint)
	}
}

// A rejected edit must leave the file exactly as it was. A worker.json written
// half-valid is a Worker that will not start, on a machine the operator may not
// be sitting at.
func TestLocalAdminRejectsInvalidEditsWithoutTouchingTheFile(t *testing.T) {
	original := enrolledConfig()
	path := writeWorkerConfig(t, original)
	admin := newTestAdmin(t, path)
	handler := admin.Handler()

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		"empty hub url":            `{"hub_url":"","token":""}`,
		"hub url without a scheme": `{"hub_url":"nas:8787","token":""}`,
		"hub url with a path":      `{"hub_url":"https://nas:8787/api/v1","token":""}`,
		"non-http scheme":          `{"hub_url":"ftp://nas","token":""}`,
		"short fingerprint":        `{"hub_url":"https://nas","certificate_fingerprint":"abcd","token":""}`,
		"non-hex fingerprint":      `{"hub_url":"https://nas","certificate_fingerprint":"` + strings.Repeat("zz", 32) + `","token":""}`,
		"malformed json":           `{`,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPut, admin.pathPrefix()+"/config", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s want 400", name, response.Code, response.Body.String())
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatalf("%s: rejected edit modified the file", name)
		}
	}
}

// An http Hub URL has to stay possible (TLS off is a supported mode) but the
// combination that silently drops pinning deserves to be visible.
func TestLocalAdminAcceptsPlainHTTPHubWithoutFingerprint(t *testing.T) {
	path := writeWorkerConfig(t, enrolledConfig())
	admin := newTestAdmin(t, path)
	response := httptest.NewRecorder()
	admin.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPut, admin.pathPrefix()+"/config", strings.NewReader(`{"hub_url":"http://127.0.0.1:8787","certificate_fingerprint":"","token":""}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

// The file holds a credential, so its mode matters as much as its contents.
func TestLocalAdminKeepsConfigPrivateAndComplete(t *testing.T) {
	path := writeWorkerConfig(t, enrolledConfig())
	admin := newTestAdmin(t, path)
	response := httptest.NewRecorder()
	admin.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPut, admin.pathPrefix()+"/config", strings.NewReader(`{"hub_url":"https://nas:8787","certificate_fingerprint":"","token":"t2"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeIsWindows() {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	if mode := info.Mode().Perm(); mode != fs.FileMode(0o600) {
		t.Fatalf("worker config mode=%o want 600", mode)
	}
}

func TestLocalAdminURLCarriesTheToken(t *testing.T) {
	admin := newTestAdmin(t, writeWorkerConfig(t, enrolledConfig()))
	url := admin.URL()
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("url=%q must be loopback", url)
	}
	if !strings.Contains(url, admin.pathToken) {
		t.Fatalf("url=%q must carry the token, or the tray cannot open the page", url)
	}
	// Two servers must not share a token; the token is the whole access control.
	other := newTestAdmin(t, writeWorkerConfig(t, enrolledConfig()))
	if other.pathToken == admin.pathToken {
		t.Fatal("tokens must be unique per process")
	}
	if len(admin.pathToken) < 32 {
		t.Fatalf("token %q is too short to resist guessing", admin.pathToken)
	}
}
