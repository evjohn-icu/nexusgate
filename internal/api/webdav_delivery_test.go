package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/app"
	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/media"
	sqliterepo "github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
	"github.com/evjohn-icu/nexusslate/internal/webdavspace"
)

// TestWebDAVDeliveryEndToEnd drives the full on-demand delivery path:
// admin creates an account and a space, links an asset, then a WebDAV client
// with that account's Basic credentials streams the original file bytes.
func TestWebDAVDeliveryEndToEnd(t *testing.T) {
	ctx := context.Background()
	repo, err := sqliterepo.Open(filepath.Join(t.TempDir(), "webdav-e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// A real asset with a primary location pointing at a real file on disk.
	assetDir := t.TempDir()
	sourceFile := filepath.Join(assetDir, "raw.mov")
	if err := os.WriteFile(sourceFile, []byte("ORIGINAL-BYTES-RAW"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, assetDir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sourceFile)
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := repo.UpsertScannedFile(ctx, root, "raw.mov", sourceFile, info, "fp-e2e")
	if err != nil {
		t.Fatal(err)
	}
	assetID := scanned.AssetID
	if assetID == "" {
		t.Fatal("UpsertScannedFile returned no asset id")
	}

	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer("", service)
	manager := webdavspace.NewManager(app.WebDAVLinker{Service: service}, sqliterepo.WebDAVAccountStore{Repo: repo})
	service.SetWebDAVSpaceManager(manager, sqliterepo.WebDAVAccountStore{Repo: repo})
	server.SetWebDAVSpaceManager(manager)
	foundDeliveryRoute := false
	for _, spec := range server.routeInventory() {
		if spec.Pattern == "/spaces/" {
			foundDeliveryRoute = true
			if spec.Auth != routeAuthWebDAVBasic || spec.Handler == nil {
				t.Fatalf("WebDAV delivery route = %+v, want explicit Basic Auth classification", spec)
			}
		}
	}
	if !foundDeliveryRoute {
		t.Fatal("WebDAV delivery mount is missing from route inventory")
	}
	handler := server.Handler()

	// 1. Create account (admin).
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/accounts", bytes.NewBufferString(`{"username":"editor","password":"s3cret"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create account = %d, body: %s", rec.Code, rec.Body.String())
	}

	// 2. Create space.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/spaces", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create space = %d, body: %s", rec.Code, rec.Body.String())
	}
	var spaceResp struct {
		SpaceID string `json:"space_id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&spaceResp); err != nil {
		t.Fatal(err)
	}

	// 3. Link the asset's original media.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/spaces/"+spaceResp.SpaceID+"/links", bytes.NewBufferString(`{"asset_id":"`+assetID+`","kind":"original"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("link = %d, body: %s", rec.Code, rec.Body.String())
	}
	var linkResp struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&linkResp); err != nil {
		t.Fatal(err)
	}
	if linkResp.Path != "/assets/"+assetID+"/original.mov" {
		t.Fatalf("link path = %q, want /assets/%s/original.mov", linkResp.Path, assetID)
	}

	// 4. WebDAV GET with the account's Basic credentials streams original bytes.
	rec = httptest.NewRecorder()
	davReq := httptest.NewRequest(http.MethodGet, "/spaces/"+spaceResp.SpaceID+linkResp.Path, nil)
	davReq.SetBasicAuth("editor", "s3cret")
	handler.ServeHTTP(rec, davReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("dav GET = %d, body: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "ORIGINAL-BYTES-RAW" {
		t.Fatalf("dav bytes = %q, want ORIGINAL-BYTES-RAW", rec.Body.String())
	}

	// 5. Wrong password is rejected.
	rec = httptest.NewRecorder()
	davReq = httptest.NewRequest(http.MethodGet, "/spaces/"+spaceResp.SpaceID+linkResp.Path, nil)
	davReq.SetBasicAuth("editor", "nope")
	handler.ServeHTTP(rec, davReq)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-password dav GET = %d, want 401", rec.Code)
	}

	// 6. Unlinked asset is not reachable.
	rec = httptest.NewRecorder()
	davReq = httptest.NewRequest(http.MethodGet, "/spaces/"+spaceResp.SpaceID+"/assets/"+assetID+"/original.mp4", nil)
	davReq.SetBasicAuth("editor", "s3cret")
	handler.ServeHTTP(rec, davReq)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unlinked path = %d, want 404", rec.Code)
	}
}
