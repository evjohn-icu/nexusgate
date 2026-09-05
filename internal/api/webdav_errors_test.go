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

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/media"
	sqliterepo "github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
	"github.com/evjohn-icu/nexusgate/internal/webdavspace"
)

// newWebDAVFixture creates a service, server and handler wired for WebDAV,
// with an asset pre-created for link tests. Returns the handler plus the
// asset ID.
func newWebDAVFixture(t *testing.T) (*app.Service, http.Handler, string) {
	t.Helper()
	ctx := context.Background()
	repo, err := sqliterepo.Open(filepath.Join(t.TempDir(), "webdav-errors.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	assetDir := t.TempDir()
	sourceFile := filepath.Join(assetDir, "raw.mov")
	if err := os.WriteFile(sourceFile, []byte("TEST-BYTES"), 0o644); err != nil {
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
	scanned, err := repo.UpsertScannedFile(ctx, root, "raw.mov", sourceFile, info, "fp-errors")
	if err != nil {
		t.Fatal(err)
	}
	if scanned.AssetID == "" {
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
	return service, server.Handler(), scanned.AssetID
}

func TestWebDAVAccountDuplicate(t *testing.T) {
	service, handler, _ := newWebDAVFixture(t)

	// First create succeeds.
	body := bytes.NewBufferString(`{"username":"editor","password":"s3cret"}`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/accounts", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}

	// Duplicate returns 409.
	body = bytes.NewBufferString(`{"username":"editor","password":"other"}`)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/accounts", body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
}

func TestWebDAVAccountEmptyFields(t *testing.T) {
	service, handler, _ := newWebDAVFixture(t)

	tests := []struct {
		name string
		body string
	}{
		{"empty username", `{"username":"","password":"s3cret"}`},
		{"whitespace username", `{"username":"   ","password":"s3cret"}`},
		{"empty password", `{"username":"editor","password":""}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/accounts", bytes.NewBufferString(tt.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400; body: %s", tt.name, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestWebDAVAccountBadJSON(t *testing.T) {
	service, handler, _ := newWebDAVFixture(t)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/accounts", bytes.NewBufferString(`not json`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestWebDAVLinkUnknownSpace(t *testing.T) {
	service, handler, assetID := newWebDAVFixture(t)

	body := bytes.NewBufferString(`{"asset_id":"` + assetID + `","kind":"original"}`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/spaces/nonexistent/links", body))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown space link = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
}

func TestWebDAVLinkInvalidKind(t *testing.T) {
	service, handler, assetID := newWebDAVFixture(t)

	// First create a space.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/spaces", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create space = %d, want 201", rec.Code)
	}
	var spaceResp struct {
		SpaceID string `json:"space_id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&spaceResp); err != nil {
		t.Fatal(err)
	}

	// Link with invalid kind.
	body := bytes.NewBufferString(`{"asset_id":"` + assetID + `","kind":"thumbnail"}`)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/spaces/"+spaceResp.SpaceID+"/links", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid kind link = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestWebDAVLinkEmptyAssetID(t *testing.T) {
	service, handler, _ := newWebDAVFixture(t)

	// First create a space.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/spaces", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create space = %d, want 201", rec.Code)
	}
	var spaceResp struct {
		SpaceID string `json:"space_id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&spaceResp); err != nil {
		t.Fatal(err)
	}

	// Link with empty asset_id.
	body := bytes.NewBufferString(`{"asset_id":"","kind":"original"}`)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/spaces/"+spaceResp.SpaceID+"/links", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty asset_id link = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestWebDAVDeleteAccount(t *testing.T) {
	service, handler, _ := newWebDAVFixture(t)

	// Create account.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/accounts", bytes.NewBufferString(`{"username":"temp","password":"s3cret"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", rec.Code)
	}

	// Delete returns 204.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodDelete, "/api/v1/admin/webdav/accounts/temp", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}

	// List does not contain deleted account.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodGet, "/api/v1/admin/webdav/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list after delete = %d, want 200", rec.Code)
	}
	var accounts []string
	if err := json.NewDecoder(rec.Body).Decode(&accounts); err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		if a == "temp" {
			t.Fatalf("deleted account 'temp' still appears in list: %v", accounts)
		}
	}
}

func TestWebDAVListSpaces(t *testing.T) {
	service, handler, _ := newWebDAVFixture(t)

	// Initially empty.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodGet, "/api/v1/admin/webdav/spaces", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list spaces = %d, want 200", rec.Code)
	}
	var spaces []string
	if err := json.NewDecoder(rec.Body).Decode(&spaces); err != nil {
		t.Fatal(err)
	}
	if len(spaces) != 0 {
		t.Fatalf("initial spaces = %v, want []", spaces)
	}

	// Create one space.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/spaces", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create space = %d, want 201", rec.Code)
	}
	var created struct {
		SpaceID string `json:"space_id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	// List now contains it.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodGet, "/api/v1/admin/webdav/spaces", nil))
	var after []string
	if err := json.NewDecoder(rec.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range after {
		if id == created.SpaceID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("space %s not found in list: %v", created.SpaceID, after)
	}
}

func TestWebDAVDeleteSpace(t *testing.T) {
	service, handler, _ := newWebDAVFixture(t)

	// Create an account so the WebDAV handler can authenticate us.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/accounts", bytes.NewBufferString(`{"username":"editor","password":"s3cret"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create account = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}

	// Create a space.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/admin/webdav/spaces", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create space = %d, want 201", rec.Code)
	}
	var created struct {
		SpaceID string `json:"space_id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	// Delete returns 204.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodDelete, "/api/v1/admin/webdav/spaces/"+created.SpaceID, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete space = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}

	// Subsequent list does not contain deleted space.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodGet, "/api/v1/admin/webdav/spaces", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list after delete = %d, want 200", rec.Code)
	}
	var spaces []string
	if err := json.NewDecoder(rec.Body).Decode(&spaces); err != nil {
		t.Fatal(err)
	}
	for _, id := range spaces {
		if id == created.SpaceID {
			t.Fatalf("deleted space %s still appears in list: %v", created.SpaceID, spaces)
		}
	}

	// Accessing the deleted space's paths returns 401 (the WebDAV handler
	// returns 401 for both bad credentials and missing spaces to prevent
	// probing which spaces exist).
	rec = httptest.NewRecorder()
	davReq := httptest.NewRequest(http.MethodGet, "/spaces/"+created.SpaceID+"/anything", nil)
	davReq.SetBasicAuth("editor", "s3cret")
	handler.ServeHTTP(rec, davReq)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("access deleted space = %d, want 401", rec.Code)
	}
}
