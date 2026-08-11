package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

func workerSetupHandler(s *Server) http.Handler {
	return s.Handler()
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
		"Worker 安装向导",
		"环境概览",
		"配置 Worker",
		"生成安装脚本",
		"启动 Worker",
		"admin-token",
		`type="password"`,
		"GOOS=windows GOARCH=amd64",
		"worker-binaries",
		"/api/v1/admin/hub/worker-setup/library-roots",
		"X-CSRF-Token",
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
	for _, marker := range []string{
		"管理员路径详情加载失败，已保留脱敏素材目录。",
		"路径需管理员 Token",
		"renderMounts()",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("page missing redacted-root fallback marker %q", marker)
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
	request := hubAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(body))
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, request)
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
	request := hubAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(body))
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	script := response.Body.String()
	if !strings.Contains(script, "$ErrorActionPreference") {
		t.Fatalf("PowerShell script missing $ErrorActionPreference: %s", script)
	}
	if !strings.Contains(script, "Invoke-WebRequest") {
		t.Fatalf("PowerShell script missing Invoke-WebRequest: %s", script)
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
	server := NewServer("", service)

	dangerousName := "foo'; rm -rf /; '"
	dangerousMount := "'; rm -rf /tmp; '"
	injectBody := `{"platform":"linux-amd64","name":"` + dangerousName + `","pairing_token":"tok-inject","mounts":[{"root_id":"root1","path":"` + dangerousMount + `"}]}`

	posixRec := httptest.NewRecorder()
	workerSetupHandler(server).ServeHTTP(posixRec, hubAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(injectBody)))
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
	workerSetupHandler(server).ServeHTTP(psRec, hubAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(psBody)))
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
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
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
	request := hubAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(body))
	workerSetupHandler(NewServer("", service)).ServeHTTP(response, request)
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
