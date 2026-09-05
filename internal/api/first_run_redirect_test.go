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

// Fresh-install routing (C1c): a hub with no library roots must land the
// first page load on /setup instead of an empty library, and a hub that
// already has a root must keep serving the library page. The decision is a
// single ListLibraryRoots call; anything else renders as today.
func TestIndexRedirectsToSetupWhenNoLibraryRoots(t *testing.T) {
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "first-run-redirect.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusFound {
		t.Fatalf("status=%d want 302 body=%s", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "/setup" {
		t.Fatalf("Location=%q want /setup", location)
	}
}

// The redirect must not fire once the hub is pointed at at least one root:
// an existing library is the normal state, and bouncing it to the setup
// wizard would make the hub unusable until a fresh install.
func TestIndexServesLibraryPageWhenRootExists(t *testing.T) {
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "first-run-root.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateLibraryRoot(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", response.Code, response.Body.String())
	}
}

// The setup wizards leave the shared sidebar (UI-001): the shell nav carries
// no setup anchor, so the wizard is reached from the workers page's Add
// Worker button instead. The anchors are exact-match strings in app_shell.go
// — the same silent-no-op hazard as the library page patches — hence the pin
// on their absence here.
func TestShellNavOmitsSetupLinks(t *testing.T) {
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "shell-nav-setup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateLibraryRoot(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, marker := range []string{
		`href="/setup" data-nav="/setup" class="nav-link">启动配置`,
		`href="/worker-setup" data-nav="/worker-setup" class="nav-link">节点安装`,
	} {
		if strings.Contains(response.Body.String(), marker) {
			t.Fatalf("shell nav must not link to setup wizard anchor %q", marker)
		}
	}
}
