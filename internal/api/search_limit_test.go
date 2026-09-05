package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

// seedSearchClampFixtures creates n assets that all carry one shared scene
// tag, so a single-token search matches all of them through SearchFiltered's
// tag-UNION path. One fixture fs.FileInfo is stat'd once and reused for every
// asset — UpsertScannedFile only reads Size/ModTime off it, and the assets it
// creates are kept distinct by fingerprint, not by a file that has to exist
// at every path — so seeding hundreds of rows stays fast enough for a table
// test instead of needing hundreds of real files on disk.
func seedSearchClampFixtures(t *testing.T, repo *sqlite.Repository, n int, token string) {
	t.Helper()
	ctx := context.Background()
	rootPath := t.TempDir()
	fixturePath := filepath.Join(rootPath, "fixture.mp4")
	if err := os.WriteFile(fixturePath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, rootPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		relPath := fmt.Sprintf("clamp-%04d.mp4", i)
		scanned, err := repo.UpsertScannedFile(ctx, root, relPath, filepath.Join(rootPath, relPath), info, fmt.Sprintf("clamp-fingerprint-%04d", i))
		if err != nil {
			t.Fatal(err)
		}
		analysis := domain.StructuredAnalysis{SceneTags: []string{token}}
		if err := repo.SyncAnalysisTags(ctx, scanned.AssetID, fmt.Sprintf("clamp-run-%04d", i), analysis); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSearchLimitClampsToMaxListedAssetIDs is O1's falsifiability check #1:
// a limit far above maxListedAssetIDs must not error and must not return
// more ids than the browser's follow-up GET /api/v1/assets?ids=... can ever
// accept. Run against unmodified server.go this fails because search's limit
// is unclamped and 250 tagged fixtures all match — proving today's handler
// really does hand back an id list the second leg of the search flow cannot
// consume.
func TestSearchLimitClampsToMaxListedAssetIDs(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "search-limit-clamp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const seeded = maxListedAssetIDs + 50
	seedSearchClampFixtures(t, repo, seeded, "clamptoken")

	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer("", service)
	request := lanRequest(http.MethodGet, "/api/v1/search?q=clamptoken&limit=500", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var ids []string
	if err := json.NewDecoder(response.Body).Decode(&ids); err != nil {
		t.Fatal(err)
	}
	if len(ids) > maxListedAssetIDs {
		t.Fatalf("got %d ids for limit=500, want at most maxListedAssetIDs=%d: the browser's follow-up GET /api/v1/assets?ids=... 400s past that cap", len(ids), maxListedAssetIDs)
	}
}

// TestSearchLimitDefaultUnaffectedByClamp pins today's unlimited-request
// default (100) so clamping an explicit over-cap limit doesn't also change
// behavior for the common case of no limit= at all.
func TestSearchLimitDefaultUnaffectedByClamp(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "search-limit-default.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const seeded = maxListedAssetIDs + 50
	seedSearchClampFixtures(t, repo, seeded, "defaulttoken")

	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer("", service)
	request := lanRequest(http.MethodGet, "/api/v1/search?q=defaulttoken", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var ids []string
	if err := json.NewDecoder(response.Body).Decode(&ids); err != nil {
		t.Fatal(err)
	}
	const wantDefault = 100
	if len(ids) != wantDefault {
		t.Fatalf("got %d ids with no limit=, want unchanged default of %d", len(ids), wantDefault)
	}
}
