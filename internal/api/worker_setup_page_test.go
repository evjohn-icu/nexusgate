package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/hubtls"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

func workerSetupHandler(s *Server) http.Handler {
	return s.Handler()
}

// workerSetupTLSServer builds a Hub Server whose TLS identity is real (so
// script generation can derive the certificate fingerprint and SPKI pin) with
// the given platform binaries written into its worker-binaries directory.
func workerSetupTLSServer(t *testing.T, service *app.Service, binaries map[string]string) http.Handler {
	t.Helper()
	dataDir := service.DataDir()
	binDir := filepath.Join(dataDir, "worker-binaries")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for platform, content := range binaries {
		info := workerPlatforms[platform]
		if err := os.WriteFile(filepath.Join(binDir, info.Filename), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	certFile, keyFile, _, err := hubtls.EnsureSelfSigned(filepath.Join(dataDir, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	return NewTLSServer("", service, certFile, keyFile).Handler()
}

// tlsAdminRequest is hubAdminRequest plus a TLS connection state, because
// script generation refuses to produce a certificate-pinned script over a
// plaintext request.
func tlsAdminRequest(service *app.Service, method, target string, body io.Reader) *http.Request {
	request := hubAdminRequest(service, method, target, body)
	request.TLS = &tls.ConnectionState{}
	return request
}

func TestWorkerSetupPageRendersWithStepMarkers(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "worker-setup-page.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/worker-setup", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	contentType := response.Header().Get("Content-Type")
	if contentType != "text/html; charset=utf-8" {
		t.Fatalf("content-type=%q", contentType)
	}
	body := response.Body.String()
	for _, marker := range []string{
		"data-worker-setup-wizard",
		"admin-token",
		`type="password"`,
		"GOOS=windows GOARCH=amd64",
		"worker-binaries",
		"/api/v1/admin/hub/worker-setup/library-roots",
		"X-CSRF-Token",
		"tdApiErrorMessage",
		"localStorage",
		"sessionStorage",
	} {
		if marker == "localStorage" || marker == "sessionStorage" {
			if strings.Contains(body, marker) {
				t.Fatalf("page must not use browser storage API %q", marker)
			}
			continue
		}
		if !strings.Contains(body, marker) {
			t.Fatalf("page missing marker %q", marker)
		}
	}
	// The wizard steps and page title are static copy, resolved server-side
	// from the locale catalog: the page constant must carry the markers, and
	// the served body must never leak an unresolved [[i18n:...]] marker.
	for _, marker := range []string{
		`<title>Timingdex · [[i18n:workerSetup.title]]</title>`,
		`1. [[i18n:workerSetup.environmentOverview]]`,
		`2. [[i18n:workerSetup.configureNode]]`,
		`3. [[i18n:workerSetup.generateScript]]`,
		`4. [[i18n:workerSetup.startNode]]`,
	} {
		if !strings.Contains(workerSetupPageHTML, marker) {
			t.Fatalf("worker setup page missing localization wiring %q", marker)
		}
	}
	if strings.Contains(body, "[[i18n:") {
		t.Fatalf("unresolved marker leaked into the served page")
	}
}

func TestWorkerSetupPagePreservesRedactedRootsWhenAdminDetailsFail(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "worker-setup-page-fallback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/worker-setup", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, "[[i18n:") {
		t.Fatalf("unresolved marker leaked into the served page")
	}
	for _, marker := range []string{
		"tdT('workerSetup.adminDetailsFailed')",
		"tdT('workerSetup.pathRequiresAdmin')",
		"renderMounts()",
	} {
		if !strings.Contains(workerSetupPageHTML, marker) {
			t.Fatalf("page missing redacted-root fallback wiring %q", marker)
		}
	}
}

// workerSetupCJKRE matches the CJK unified-ideograph and CJK-punctuation
// blocks. The page source must carry none of them: every piece of product copy
// now comes from the locale catalog, so a raw Chinese literal in
// workerSetupPageHTML is a migration regression.
var workerSetupCJKRE = regexp.MustCompile(`[\x{3000}-\x{303f}\x{3400}-\x{4dbf}\x{4e00}-\x{9fff}\x{f900}-\x{faff}]`)

// workerSetupFragmentKeys loads the page's fragment file and returns its zh-CN
// key set. The fragment is not merged into the embedded catalogs yet, so the
// page tests assert the key wiring against the fragment on disk rather than a
// resolved zh-CN value.
func workerSetupFragmentKeys(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("locales", "fragments", "worker_setup.json"))
	if err != nil {
		t.Fatalf("read worker setup fragment: %v", err)
	}
	var frag struct {
		Keys map[string]map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(data, &frag); err != nil {
		t.Fatalf("parse worker setup fragment: %v", err)
	}
	keys := make(map[string]bool, len(frag.Keys[string(localeZhCN)]))
	for k := range frag.Keys[string(localeZhCN)] {
		keys[k] = true
	}
	return keys
}

// The page source must be fully migrated: no hardcoded CJK copy, the shared
// tdApiErrorMessage instead of a page-local apiErrMsg, and no browser-locale
// number/date formatters.
func TestWorkerSetupPageI18nHasNoHardcodedChinese(t *testing.T) {
	if hits := workerSetupCJKRE.FindAllString(workerSetupPageHTML, -1); len(hits) > 0 {
		t.Fatalf("workerSetupPageHTML still carries hardcoded CJK copy: %q", hits)
	}
	if strings.Contains(workerSetupPageHTML, "apiErrMsg") {
		t.Fatal("page-local apiErrMsg must be deleted in favor of the shared tdApiErrorMessage")
	}
	if !strings.Contains(workerSetupPageHTML, "tdApiErrorMessage(") {
		t.Fatal("worker setup page must call the shared tdApiErrorMessage helper")
	}
	for _, legacy := range []string{"toLocaleString(", "toLocaleTimeString", "toLocaleDateString"} {
		if strings.Contains(workerSetupPageHTML, legacy) {
			t.Fatalf("worker setup page still uses browser-locale formatter %q", legacy)
		}
	}
}

// Every product string goes through the shared locale machinery: static HTML
// via [[i18n:*]] markers, dynamic copy via tdT/tdPlural, and API failures via
// tdApiErrorMessage. A hardcoded Chinese string here means the migration
// regressed.
func TestWorkerSetupPageLocalizedCopy(t *testing.T) {
	for _, marker := range []string{
		`<title>Timingdex · [[i18n:workerSetup.title]]</title>`,
		`1. [[i18n:workerSetup.environmentOverview]]`,
		`2. [[i18n:workerSetup.configureNode]]`,
		`3. [[i18n:workerSetup.generateScript]]`,
		`4. [[i18n:workerSetup.startNode]]`,
		`<a class="btn btn--ghost" href="/workers">← [[i18n:common.back]]</a>`,
		`[[i18n:workerSetup.hubConnectionInfo]]`,
		`[[i18n:workerSetup.loadingEnvironment]]`,
		`[[i18n:workerSetup.binaries]]`,
		`[[i18n:workerSetup.binariesHint]]`,
		`[[i18n:workerSetup.crossCompileLead]]`,
		`[[i18n:workerSetup.placeFilesLead]]`,
		`[[i18n:workerSetup.placeFilesTrail]]`,
		`[[i18n:workerSetup.configuration]]`,
		`[[i18n:workerSetup.targetPlatform]]`,
		`[[i18n:workerSetup.workerNameOptional]]`,
		`[[i18n:workerSetup.workerNamePlaceholder]]`,
		`[[i18n:workerSetup.cacheDirOptional]]`,
		`[[i18n:workerSetup.cacheDirPlaceholder]]`,
		`[[i18n:workerSetup.mountMapping]]`,
		`[[i18n:workerSetup.mountMappingHint]]`,
		`[[i18n:workerSetup.previous]]`,
		`[[i18n:workerSetup.scriptWarning]]`,
		`[[i18n:workerSetup.generateButton]]`,
		`[[i18n:workerSetup.runCommandHint]]`,
		`[[i18n:workerSetup.doctorHintLead]]`,
		`[[i18n:workerSetup.doctorHintTrail]]`,
		`[[i18n:workerSetup.workersStatusHintLead]]`,
		`<a href="/workers">[[i18n:workers.title]]</a>`,
		`[[i18n:workerSetup.workersStatusHintTrail]]`,
		`[[i18n:common.next]] →`,
		`tdT('workerSetup.binaryAvailable')`,
		`tdT('workerSetup.binaryUnavailable')`,
		`tdT('workerSetup.binaryNotUploaded')`,
		`tdT('workerSetup.noRoots')`,
		`tdT('workerSetup.pathRequiresAdmin')`,
		`tdT('workerSetup.localPath')`,
		`tdT('workerSetup.adminDetailsFailed')`,
		`tdT('workerSetup.hubUrl')`,
		`tdT('workerSetup.tls')`,
		`tdT('workerSetup.fingerprint')`,
		`tdT('workerSetup.mediaFolders')`,
		`tdT('workerSetup.tlsEnabled')`,
		`tdT('workerSetup.tlsNotEnabled')`,
		`tdPlural('workerSetup.rootCount'`,
		`tdT('workerSetup.environmentLoadError'`,
		`tdT('workerSetup.generating')`,
		`tdT('workerSetup.copyScript')`,
		`tdT('workerSetup.scriptOneTimeNote')`,
		`tdT('workerSetup.viewStartupGuide')`,
		`tdT('workerSetup.generateFailed'`,
		`tdT('common.retry')`,
		`tdT('common.copied')`,
		`tdApiErrorMessage(`,
	} {
		if !strings.Contains(workerSetupPageHTML, marker) {
			t.Fatalf("worker setup page missing localization wiring %q", marker)
		}
	}
}

// Every static [[i18n:key]] marker and every literal key passed to
// tdT/tdPlural must resolve: to a key the page fragment carries in all five
// locales, or to one of the shared catalog keys (common.*, status.*,
// workers.*) that every locale already ships. Plural bases are resolved
// through their .one/.other siblings.
func TestWorkerSetupPageI18nKeysResolveFromFragment(t *testing.T) {
	fragKeys := workerSetupFragmentKeys(t)

	markerRE := regexp.MustCompile(`\[\[i18n:([a-zA-Z0-9._-]+)\]\]`)
	callRE := regexp.MustCompile(`td(?:T|Plural)\('([a-zA-Z0-9._-]+)'`)

	seen := make(map[string]bool)
	for _, m := range markerRE.FindAllStringSubmatch(workerSetupPageHTML, -1) {
		seen[m[1]] = true
	}
	for _, m := range callRE.FindAllStringSubmatch(workerSetupPageHTML, -1) {
		seen[m[1]] = true
	}

	if len(seen) == 0 {
		t.Fatal("no keys detected in workerSetupPageHTML; the scan is broken")
	}

	var unresolved []string
	for key := range seen {
		if fragKeys[key] {
			continue
		}
		// A plural base key resolves via its category siblings.
		if fragKeys[key+".one"] || fragKeys[key+".other"] {
			continue
		}
		if catalogs[localeZhCN].has(key) {
			continue
		}
		unresolved = append(unresolved, key)
	}
	sort.Strings(unresolved)
	if len(unresolved) > 0 {
		t.Fatalf("worker setup keys that resolve to nothing (not in fragment, its plural siblings, or a shared catalog): %v", unresolved)
	}
}

// The fragment must be a valid UTF-8 JSON catalog fragment: the page name
// matches, all five locales carry the identical key set, and every key's
// {placeholder} set matches zh-CN. It must define only page-prefixed keys.
func TestWorkerSetupPageI18nFragmentParityAcrossLocales(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("locales", "fragments", "worker_setup.json"))
	if err != nil {
		t.Fatal(err)
	}
	var frag struct {
		Page string                       `json:"page"`
		Keys map[string]map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(data, &frag); err != nil {
		t.Fatal(err)
	}
	if frag.Page != "worker_setup" {
		t.Fatalf("fragment page=%q, want worker_setup", frag.Page)
	}
	for _, loc := range supportedLocales {
		if _, ok := frag.Keys[string(loc)]; !ok {
			t.Fatalf("fragment missing locale %s", loc)
		}
	}

	base := frag.Keys[string(localeZhCN)]
	for _, loc := range supportedLocales[1:] {
		other := frag.Keys[string(loc)]
		if len(other) != len(base) {
			t.Fatalf("locale %s has %d keys, zh-CN has %d", loc, len(other), len(base))
		}
		for k := range base {
			if _, ok := other[k]; !ok {
				t.Fatalf("locale %s missing key %q", loc, k)
			}
		}
	}

	phRE := regexp.MustCompile(`\{[a-zA-Z]+\}`)
	phSet := func(s string) string {
		parts := phRE.FindAllString(s, -1)
		sort.Strings(parts)
		return strings.Join(parts, ",")
	}
	for k, zh := range base {
		want := phSet(zh)
		for _, loc := range supportedLocales[1:] {
			if got := phSet(frag.Keys[string(loc)][k]); got != want {
				t.Fatalf("placeholder set differs for %s in %s: %q vs zh-CN %q", k, loc, frag.Keys[string(loc)][k], zh)
			}
		}
	}

	// The fragment must define page-prefixed keys only, never the shared ones.
	for k := range base {
		for _, prefix := range []string{"common.", "api.", "status.", "facet.", "shell."} {
			if strings.HasPrefix(k, prefix) {
				t.Fatalf("fragment redefines shared key %q", k)
			}
		}
	}
}

func TestWorkerSetupContextAuthMatrixRedactsPaths(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "worker-setup-context-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootPath := t.TempDir()
	root, err := repo.CreateLibraryRoot(ctx, rootPath)
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()
	tests := []struct {
		name       string
		request    func() *http.Request
		wantStatus int
	}{
		{name: "lan", request: func() *http.Request {
			return lanRequest(http.MethodGet, "/api/v1/hub/worker-setup/context", nil)
		}, wantStatus: http.StatusOK},
		{name: "remote", request: func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/api/v1/hub/worker-setup/context", nil)
		}, wantStatus: http.StatusForbidden},
		{name: "agent", request: func() *http.Request {
			return hubAgentRequest(service, http.MethodGet, "/api/v1/hub/worker-setup/context", nil)
		}, wantStatus: http.StatusOK},
		{name: "admin", request: func() *http.Request {
			return hubAdminRequest(service, http.MethodGet, "/api/v1/hub/worker-setup/context", nil)
		}, wantStatus: http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, test.request())
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
			if response.Code != http.StatusOK {
				return
			}
			var data struct {
				LibraryRoots []map[string]any `json:"library_roots"`
			}
			if err := json.NewDecoder(response.Body).Decode(&data); err != nil {
				t.Fatal(err)
			}
			if len(data.LibraryRoots) != 1 || data.LibraryRoots[0]["id"] != root.ID {
				t.Fatalf("library_roots=%+v", data.LibraryRoots)
			}
			if _, exists := data.LibraryRoots[0]["path"]; exists {
				t.Fatalf("context returned path: %+v", data.LibraryRoots[0])
			}
			if strings.Contains(response.Body.String(), rootPath) {
				t.Fatalf("context disclosed root path %q", rootPath)
			}
		})
	}
}

func TestWorkerSetupLibraryRootsAdminAuthMatrix(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "worker-setup-library-roots-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootPath := t.TempDir()
	root, err := repo.CreateLibraryRoot(ctx, rootPath)
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()
	requests := []struct {
		name       string
		request    func() *http.Request
		wantStatus int
	}{
		{name: "no token", request: func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/api/v1/admin/hub/worker-setup/library-roots", nil)
		}, wantStatus: http.StatusUnauthorized},
		{name: "agent token", request: func() *http.Request {
			return hubAgentRequest(service, http.MethodGet, "/api/v1/admin/hub/worker-setup/library-roots", nil)
		}, wantStatus: http.StatusUnauthorized},
		{name: "admin token", request: func() *http.Request {
			return hubAdminRequest(service, http.MethodGet, "/api/v1/admin/hub/worker-setup/library-roots", nil)
		}, wantStatus: http.StatusOK},
	}
	for _, test := range requests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, test.request())
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
			if response.Code != http.StatusOK {
				return
			}
			var data workerSetupLibraryRootsResponse
			if err := json.NewDecoder(response.Body).Decode(&data); err != nil {
				t.Fatal(err)
			}
			if len(data.LibraryRoots) != 1 || data.LibraryRoots[0].ID != root.ID || data.LibraryRoots[0].Path != rootPath {
				t.Fatalf("library_roots=%+v, want id=%q path=%q", data.LibraryRoots, root.ID, rootPath)
			}
		})
	}
}

func TestWorkerSetupContextReturnsHubInfo(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "worker-setup-ctx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootPath := t.TempDir()
	if _, err := repo.CreateLibraryRoot(ctx, rootPath); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/hub/worker-setup/context", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var data struct {
		HubURL            string                    `json:"hub_url"`
		Fingerprint       string                    `json:"fingerprint"`
		TLS               bool                      `json:"tls"`
		LibraryRoots      []domain.LibraryRoot      `json:"library_roots"`
		AvailableBinaries map[string]map[string]any `json:"available_binaries"`
	}
	if err := json.NewDecoder(response.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if data.HubURL == "" {
		t.Fatal("hub_url is empty")
	}
	if data.Fingerprint != "" {
		t.Fatalf("expected empty fingerprint without TLS, got %q", data.Fingerprint)
	}
	if data.TLS {
		t.Fatal("expected tls=false without TLS cert")
	}
	if len(data.LibraryRoots) != 1 || data.LibraryRoots[0].ID == "" {
		t.Fatalf("library_roots=%+v", data.LibraryRoots)
	}
	if strings.Contains(response.Body.String(), rootPath) || strings.Contains(response.Body.String(), `"path"`) {
		t.Fatalf("context disclosed a library path: %s", response.Body.String())
	}
	if len(data.AvailableBinaries) == 0 {
		t.Fatal("available_binaries is empty")
	}
	for _, platform := range []string{"linux-amd64", "linux-arm64", "windows-amd64"} {
		entry, ok := data.AvailableBinaries[platform]
		if !ok {
			t.Fatalf("available_binaries missing platform %q", platform)
		}
		exists, _ := entry["exists"].(bool)
		if exists {
			t.Fatalf("platform %q should not exist without worker-binaries dir", platform)
		}
	}
}

func TestWorkerSetupScriptRequiresAdminToken(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "script-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"platform":"linux-amd64","pairing_token":"test-token"}`
	handler := NewServer("", service).Handler()
	for _, test := range []struct {
		name    string
		request *http.Request
	}{
		{name: "no token", request: httptest.NewRequest(http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(body))},
		{name: "agent token", request: hubAgentRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(body))},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, test.request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s, want 401", response.Code, response.Body.String())
			}
		})
	}
}

func TestWorkerSetupScriptPOSIXIncludesPairingAndMount(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "script-posix.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"platform":"linux-amd64","name":"test-worker","pairing_token":"tok-abc","mounts":[{"root_id":"root1","path":"/mnt/footage"}]}`
	response := httptest.NewRecorder()
	request := tlsAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(body))
	workerSetupTLSServer(t, service, map[string]string{"linux-amd64": "binary-content"}).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	script := response.Body.String()
	if contentType := response.Header().Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
		t.Fatalf("content-type=%q", contentType)
	}
	if !strings.HasPrefix(script, "#!/bin/sh\n") {
		t.Fatalf("script does not start with #!/bin/sh: %s", script[:50])
	}
	if !strings.Contains(script, "set -eu\n") {
		t.Fatalf("script missing set -eu: %s", script[:100])
	}
	if !strings.Contains(script, "--pairing") {
		t.Fatalf("script missing --pairing: %s", script)
	}
	if !strings.Contains(script, "--mount") || !strings.Contains(script, "root1=/mnt/footage") {
		t.Fatalf("script missing mount mapping: %s", script)
	}
	if !strings.Contains(script, "--name") || !strings.Contains(script, "test-worker") {
		t.Fatalf("script missing worker name: %s", script)
	}
	if !strings.Contains(script, "single-use credential") {
		t.Fatalf("script missing credential warning: %s", script)
	}
	if !strings.Contains(script, "worker run") {
		t.Fatalf("script missing worker run instructions: %s", script)
	}
}

func TestWorkerSetupScriptPowerShellIncludesErrorActionPreference(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "script-ps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"platform":"windows-amd64","name":"win-worker","pairing_token":"tok-xyz","mounts":[{"root_id":"root1","path":"C:\\footage"}]}`
	response := httptest.NewRecorder()
	request := tlsAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(body))
	workerSetupTLSServer(t, service, map[string]string{"windows-amd64": "binary-content"}).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	script := response.Body.String()
	if !strings.Contains(script, "$ErrorActionPreference") {
		t.Fatalf("PowerShell script missing $ErrorActionPreference: %s", script)
	}
	if !strings.Contains(script, "ServerCertificateCustomValidationCallback") {
		t.Fatalf("PowerShell script missing the certificate fingerprint callback: %s", script)
	}
	if !strings.Contains(script, "X-Timingdex-Pairing-Token") {
		t.Fatalf("PowerShell script missing the pairing token header: %s", script)
	}
	if !strings.Contains(script, "--pairing") || !strings.Contains(script, "tok-xyz") {
		t.Fatalf("PowerShell script missing pairing: %s", script)
	}
	if !strings.Contains(script, "timingdex-windows-amd64.exe") {
		t.Fatalf("PowerShell script wrong binary name: %s", script)
	}
	if !strings.Contains(script, "single-use credential") {
		t.Fatalf("PowerShell script missing credential warning: %s", script)
	}
}

func TestWorkerSetupScriptShellInjection(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "script-inject.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	server := workerSetupTLSServer(t, service, map[string]string{"linux-amd64": "binary-content", "windows-amd64": "binary-content"})

	dangerousName := "foo'; rm -rf /; '"
	dangerousMount := "'; rm -rf /tmp; '"
	injectBody := `{"platform":"linux-amd64","name":"` + dangerousName + `","pairing_token":"tok-inject","mounts":[{"root_id":"root1","path":"` + dangerousMount + `"}]}`

	posixRec := httptest.NewRecorder()
	server.ServeHTTP(posixRec, tlsAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(injectBody)))
	if posixRec.Code != http.StatusOK {
		t.Fatalf("POSIX script status=%d body=%s", posixRec.Code, posixRec.Body.String())
	}
	posixScript := posixRec.Body.String()
	if !strings.Contains(posixScript, "rm -rf") {
		t.Fatalf("POSIX script should include the dangerous payload as literal text, but got: %s", posixScript)
	}
	if !strings.Contains(posixScript, `'\''`) {
		t.Fatalf("POSIX script missing proper single-quote escaping: %s", posixScript)
	}
	if strings.Contains(posixScript, "--name foo';") && !strings.Contains(posixScript, "--name 'foo'") {
		t.Fatalf("POSIX script appears to have unquoted dangerous payload: %s", posixScript)
	}

	psRec := httptest.NewRecorder()
	psBody := `{"platform":"windows-amd64","name":"` + dangerousName + `","pairing_token":"tok-inject-ps","mounts":[{"root_id":"root1","path":"` + dangerousMount + `"}]}`
	server.ServeHTTP(psRec, tlsAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(psBody)))
	if psRec.Code != http.StatusOK {
		t.Fatalf("PowerShell script status=%d body=%s", psRec.Code, psRec.Body.String())
	}
	psScript := psRec.Body.String()
	if !strings.Contains(psScript, "rm -rf") {
		t.Fatalf("PowerShell script should include the dangerous payload as literal text: %s", psScript)
	}
	if !strings.Contains(psScript, "''") {
		t.Fatalf("PowerShell script missing proper single-quote escaping: %s", psScript)
	}
}

func TestWorkerSetupScriptUnknownPlatform(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "script-unknown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"platform":"darwin-arm64","pairing_token":"tok"}`
	response := httptest.NewRecorder()
	request := hubAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(body))
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown platform status=%d body=%s, want 400", response.Code, response.Body.String())
	}
}

func TestWorkerSetupScriptEmptyPairingToken(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "script-empty-tok.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"platform":"linux-amd64","pairing_token":""}`
	response := httptest.NewRecorder()
	request := hubAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(body))
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("empty pairing_token status=%d body=%s, want 400", response.Code, response.Body.String())
	}
}

func TestWorkerSetupBinaryDownloadUnknownPlatform(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "bin-unknown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/hub/worker-binaries/darwin-arm64", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", response.Code, response.Body.String())
	}
}

func TestWorkerSetupBinaryDownloadPathTraversal(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "bin-traversal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/hub/worker-binaries/../../etc/passwd", nil))
	// Go 1.22 ServeMux path cleaning may redirect (307) or 404; the key
	// invariant is that the response is neither 200 nor carries the file.
	if response.Code == http.StatusOK {
		t.Fatalf("path traversal returned 200, which would serve a file")
	}
}

func TestWorkerSetupBinaryDownloadMissingFile(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "bin-missing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/hub/worker-binaries/linux-amd64", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing binary status=%d body=%s, want 404", response.Code, response.Body.String())
	}
}

func TestWorkerSetupBinaryDownloadSuccess(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "bin-success.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dataDir := secureTestDataDir(t)
	binDir := filepath.Join(dataDir, "worker-binaries")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binaryContent := []byte("fake-timingdex-binary")
	if err := os.WriteFile(filepath.Join(binDir, "timingdex-linux-amd64"), binaryContent, 0o755); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: dataDir, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/hub/worker-binaries/linux-amd64", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/octet-stream" {
		t.Fatalf("content-type=%q", contentType)
	}
	if !bytes.Equal(response.Body.Bytes(), binaryContent) {
		t.Fatalf("binary content mismatch: got %d bytes, want %d bytes", response.Body.Len(), len(binaryContent))
	}
}

func TestWorkerSetupContextRequiresTrustedNetwork(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "ctx-guard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/hub/worker-setup/context", nil)
	req.RemoteAddr = "203.0.113.50:9999"
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("untrusted network status=%d body=%s, want 403", response.Code, response.Body.String())
	}
}

func TestQuotePowerShell(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "'simple'"},
		{"it's", "'it''s'"},
		{"", "''"},
		{"foo'bar'baz", "'foo''bar''baz'"},
		{"a;rm -rf /;b", "'a;rm -rf /;b'"},
	}
	for _, tc := range tests {
		got := quotePowerShell(tc.input)
		if got != tc.expected {
			t.Errorf("quotePowerShell(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestQuotePOSIX(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "'simple'"},
		{"it's", "'it'\\''s'"},
		{"", "''"},
		{"foo'bar", "'foo'\\''bar'"},
		{"a;rm -rf /;b", "'a;rm -rf /;b'"},
	}
	for _, tc := range tests {
		got := quotePOSIX(tc.input)
		if got != tc.expected {
			t.Errorf("quotePOSIX(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestWorkerSetupContextWithBinaryPresent(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "ctx-bin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dataDir := secureTestDataDir(t)
	binDir := filepath.Join(dataDir, "worker-binaries")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "timingdex-linux-amd64"), []byte("binary-content"), 0o755); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: dataDir, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/hub/worker-setup/context", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var data struct {
		AvailableBinaries map[string]map[string]any `json:"available_binaries"`
	}
	if err := json.NewDecoder(response.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	linuxEntry, ok := data.AvailableBinaries["linux-amd64"]
	if !ok {
		t.Fatal("missing linux-amd64 in available_binaries")
	}
	exists, _ := linuxEntry["exists"].(bool)
	if !exists {
		t.Fatal("linux-amd64 should report exists=true")
	}
	size, _ := linuxEntry["size_bytes"].(float64)
	if int64(size) != int64(len("binary-content")) {
		t.Fatalf("size_bytes=%v, want %d", size, len("binary-content"))
	}
}

func TestWorkerSetupScriptRejectsNotAuthorizedReader(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "script-reader-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}}
	cfg.HubSecurity.AdminAuth = "required"
	service, err := app.NewService(repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"platform":"linux-amd64","pairing_token":"tok"}`
	response := httptest.NewRecorder()
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, lanRequest(http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(body)))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401", response.Code, response.Body.String())
	}
}

func TestWorkerSetupScriptGeneratesWithMultipleMounts(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "script-multi-mount.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"platform":"linux-arm64","name":"arm-worker","pairing_token":"tok-multi","mounts":[{"root_id":"r1","path":"/mnt/a"},{"root_id":"r2","path":"/mnt/b"}]}`
	response := httptest.NewRecorder()
	request := tlsAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(body))
	workerSetupTLSServer(t, service, map[string]string{"linux-arm64": "binary-content"}).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	script := response.Body.String()
	if !strings.Contains(script, "r1=/mnt/a") {
		t.Fatalf("script missing first mount: %s", script)
	}
	if !strings.Contains(script, "r2=/mnt/b") {
		t.Fatalf("script missing second mount: %s", script)
	}
	if !strings.Contains(script, "arm-worker") {
		t.Fatalf("script missing worker name: %s", script)
	}
	if !strings.Contains(script, "linux-arm64") {
		t.Fatalf("script missing binary reference for linux-arm64: %s", script)
	}
}

func TestWorkerSetupBinaryDownloadServesCorrectFilePerPlatform(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "bin-multi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dataDir := secureTestDataDir(t)
	binDir := filepath.Join(dataDir, "worker-binaries")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "timingdex-linux-amd64"), []byte("linux-amd64-content"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "timingdex-linux-arm64"), []byte("linux-arm64-content"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "timingdex-windows-amd64.exe"), []byte("windows-content"), 0o755); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: dataDir, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := workerSetupHandler(NewServer("", service))

	tests := []struct {
		platform string
		want     string
	}{
		{"linux-amd64", "linux-amd64-content"},
		{"linux-arm64", "linux-arm64-content"},
		{"windows-amd64", "windows-content"},
	}
	for _, tc := range tests {
		t.Run(tc.platform, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/hub/worker-binaries/"+tc.platform, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if !bytes.Equal(response.Body.Bytes(), []byte(tc.want)) {
				t.Fatalf("content mismatch for %s: got %q, want %q", tc.platform, response.Body.String(), tc.want)
			}
		})
	}
}
