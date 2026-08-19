package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// pageRoutes is the closed set of GET routes in the route inventory that serve an HTML
// page. Handler() registers many more routes — JSON endpoints, asset file
// serving, worker enrollment — but only these render inline <script> blocks
// that must parse. The list mirrors the route table rather than a hand-picked
// sample: it includes /workers, which a shorter enumeration of the pages can
// easily skip. When a page route is added to or removed from the table, this
// list must follow.
var pageRoutes = []struct {
	route string
	page  string
}{
	{"/", "library index"},
	{"/setup", "setup wizard"},
	{"/progress", "progress monitor"},
	{"/workers", "worker fleet"},
	{"/library-roots", "library roots wizard"},
	{"/repurpose", "repurpose workspace"},
	{"/tags", "tag curator"},
	{"/providers", "provider channels"},
	{"/collections", "collections basket"},
	{"/settings", "throttle settings"},
	{"/worker-setup", "worker setup wizard"},
}

var pageScriptBlockRE = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)

// TestPageScriptsParseWithNode extracts the contents of every <script> block
// from every served page and syntax-checks them with node --check. The
// grep-for-markers tests cannot see a syntax error: /worker-setup shipped
// three versions with its whole script block dead (c9dba75) while every
// marker stayed present. This test guards where node happens to be installed
// and skips cleanly where it is not, exactly like the ffmpeg/ffprobe
// integration test in internal/media.
func TestPageScriptsParseWithNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to syntax-check the inline JavaScript of every served page")
	}
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "page-scripts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Fresh-install routing redirects a rootless hub's / to /setup; the route
	// table below includes /, so the hub needs a root for the page to serve.
	if _, err := repo.CreateLibraryRoot(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	for _, route := range pageRoutes {
		t.Run(route.route, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, route.route, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("route %s (%s) served status %d", route.route, route.page, response.Code)
			}
			blocks := pageScriptBlockRE.FindAllStringSubmatch(response.Body.String(), -1)
			if len(blocks) == 0 {
				return
			}
			dir := t.TempDir()
			for i, block := range blocks {
				slug := strings.TrimPrefix(route.route, "/")
				if slug == "" {
					slug = "root"
				}
				path := filepath.Join(dir, fmt.Sprintf("%s-script-%d.js", strings.ReplaceAll(slug, "/", "_"), i))
				if err := os.WriteFile(path, []byte(block[1]), 0o600); err != nil {
					t.Fatal(err)
				}
				output, err := exec.Command(node, "--check", path).CombinedOutput()
				if err != nil {
					t.Fatalf("route %s (%s) script block %d does not parse: %v\n%s", route.route, route.page, i+1, err, output)
				}
			}
		})
	}
}
