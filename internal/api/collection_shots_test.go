package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// newShotBasketFixture drives the real repository through the same setup the
// other real-service API tests use: a library root, one scanned asset, and
// three shots with known timings. The basket writes validate shot existence in
// asset_shots, so the shots must be real rows before any basket call.
func newShotBasketFixture(t *testing.T) (*app.Service, string, map[string]domain.AssetShot) {
	t.Helper()
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "shot-basket-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootPath := t.TempDir()
	videoPath := filepath.Join(rootPath, "fixture.mp4")
	if err := os.WriteFile(videoPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, rootPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertScannedFile(ctx, root, "fixture.mp4", videoPath, info, "shot-basket-api-fingerprint"); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	shots := []domain.AssetShot{
		{ID: "shot-1", StartMS: 0, EndMS: 4000, Description: "城市夜景开场"},
		{ID: "shot-2", StartMS: 4000, EndMS: 9000, Description: "街道人流"},
		{ID: "shot-3", StartMS: 9000, EndMS: 12000, Description: "地铁站台"},
	}
	if err := repo.ReplaceAssetShots(ctx, assets[0].ID, "", shots, "", ""); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]domain.AssetShot, len(shots))
	for _, shot := range shots {
		byID[shot.ID] = shot
	}
	return service, assets[0].ID, byID
}

// createCollectionThroughAPI creates a collection through the real
// authenticated route, so the id the basket tests use is one the API itself
// minted.
func createCollectionThroughAPI(t *testing.T, handler http.Handler, service *app.Service) string {
	t.Helper()
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, hubAdminRequest(service, http.MethodPost, "/api/v1/collections", bytes.NewBufferString(`{"name":"素材回收篮"}`)))
	if created.Code != http.StatusCreated {
		t.Fatalf("collection create=%d body=%s", created.Code, created.Body.String())
	}
	var collection domain.AssetCollection
	if err := json.NewDecoder(created.Body).Decode(&collection); err != nil {
		t.Fatal(err)
	}
	if collection.ID == "" {
		t.Fatal("collection id must not be empty")
	}
	return collection.ID
}

// TestCollectionShotBasketAPI drives the four basket routes end to end against
// a real repository: add, list with joined shot fields, idempotent duplicate
// add, reorder, delete, and the 404 a collection id naming nothing must
// answer. The statuses are the contract; the joined fields prove the list
// answers the basket view, not just the pin rows.
func TestCollectionShotBasketAPI(t *testing.T) {
	service, _, shots := newShotBasketFixture(t)
	handler := NewServer("", service).Handler()
	collectionID := createCollectionThroughAPI(t, handler, service)

	shoot := func(shotID string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/collections/"+collectionID+"/shots", bytes.NewBufferString(`{"shot_id":"`+shotID+`"}`)))
		return response
	}
	list := func() []domain.CollectionShotDetail {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/collections/"+collectionID+"/shots", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("basket list=%d body=%s", response.Code, response.Body.String())
		}
		var basket []domain.CollectionShotDetail
		if err := json.NewDecoder(response.Body).Decode(&basket); err != nil {
			t.Fatal(err)
		}
		return basket
	}

	// Adding a shot answers 201 with the ok envelope.
	added := shoot("shot-1")
	if added.Code != http.StatusCreated {
		t.Fatalf("add status=%d body=%s", added.Code, added.Body.String())
	}
	if !bytes.Contains(added.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("add body=%s, want the ok envelope", added.Body.String())
	}

	// The list contains the shot with the joined fields a basket view needs.
	basket := list()
	if len(basket) != 1 || basket[0].ShotID != "shot-1" || basket[0].Position != 0 {
		t.Fatalf("basket=%+v, want shot-1 at position 0", basket)
	}
	if basket[0].AssetID == "" || basket[0].StartMS != shots["shot-1"].StartMS || basket[0].EndMS != shots["shot-1"].EndMS || basket[0].Description != shots["shot-1"].Description {
		t.Fatalf("basket entry=%+v, want the joined asset/shot fields", basket[0])
	}

	// A duplicate add is absorbed by the primary key: same 201, still one pin.
	duplicate := shoot("shot-1")
	if duplicate.Code != http.StatusCreated {
		t.Fatalf("duplicate add status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	if basket = list(); len(basket) != 1 {
		t.Fatalf("basket after duplicate add=%+v, want one pin", basket)
	}

	shoot("shot-2")
	shoot("shot-3")
	if basket = list(); len(basket) != 3 {
		t.Fatalf("basket=%+v, want three pins", basket)
	}

	// Reorder replaces the display order wholesale.
	reorder := httptest.NewRecorder()
	handler.ServeHTTP(reorder, hubAdminRequest(service, http.MethodPost, "/api/v1/collections/"+collectionID+"/shots/reorder", bytes.NewBufferString(`{"shot_ids":["shot-3","shot-1","shot-2"]}`)))
	if reorder.Code != http.StatusOK {
		t.Fatalf("reorder status=%d body=%s", reorder.Code, reorder.Body.String())
	}
	basket = list()
	want := []string{"shot-3", "shot-1", "shot-2"}
	for i, detail := range basket {
		if detail.ShotID != want[i] || detail.Position != i {
			t.Fatalf("reordered basket[%d]=%+v, want %s at position %d", i, detail, want[i], i)
		}
	}

	// Deleting a pin answers 204 and removes it from the list.
	removed := httptest.NewRecorder()
	handler.ServeHTTP(removed, hubAdminRequest(service, http.MethodDelete, "/api/v1/collections/"+collectionID+"/shots/shot-1", nil))
	if removed.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", removed.Code, removed.Body.String())
	}
	if basket = list(); len(basket) != 2 {
		t.Fatalf("basket after delete=%+v, want two pins", basket)
	}
	for _, detail := range basket {
		if detail.ShotID == "shot-1" {
			t.Fatalf("deleted shot-1 still in basket: %+v", basket)
		}
	}

	// A collection id naming nothing is a 404 envelope, not a 500.
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, hubAdminRequest(service, http.MethodPost, "/api/v1/collections/does-not-exist/shots", bytes.NewBufferString(`{"shot_id":"shot-1"}`)))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing collection status=%d body=%s, want 404", missing.Code, missing.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(missing.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "not_found" {
		t.Fatalf("missing collection envelope=%+v, want code not_found", envelope)
	}
	for _, tc := range []struct {
		method string
		target string
		body   string
	}{
		{http.MethodGet, "/api/v1/collections/does-not-exist/shots", ""},
		{http.MethodDelete, "/api/v1/collections/does-not-exist/shots/shot-1", ""},
		{http.MethodPost, "/api/v1/collections/does-not-exist/shots/reorder", `{"shot_ids":[]}`},
	} {
		var body io.Reader = http.NoBody
		if tc.body != "" {
			body = bytes.NewBufferString(tc.body)
		}
		response := httptest.NewRecorder()
		request := hubAdminRequest(service, tc.method, tc.target, body)
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("missing collection %s %s status=%d body=%s, want 404", tc.method, tc.target, response.Code, response.Body.String())
		}
	}

	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, hubAdminRequest(service, http.MethodPost, "/api/v1/collections/"+collectionID+"/shots/reorder", bytes.NewBufferString(`{"shot_ids":["shot-3","shot-3","shot-2"]}`)))
	if invalid.Code != http.StatusConflict {
		t.Fatalf("invalid reorder status=%d body=%s, want 409", invalid.Code, invalid.Body.String())
	}
	if !bytes.Contains(invalid.Body.Bytes(), []byte(`"retryable":true`)) {
		t.Fatalf("invalid reorder body=%s, want retryable conflict", invalid.Body.String())
	}
}

// TestCollectionShotBasketRequiresHubAdmin pins the auth posture of the
// basket mutations: same as the collection writes, an unauthenticated caller
// is refused 401 with the admin envelope on every one of them.
func TestCollectionShotBasketRequiresHubAdmin(t *testing.T) {
	service, _, _ := newShotBasketFixture(t)
	handler := NewServer("", service).Handler()

	for _, tc := range []struct {
		method string
		target string
		body   string
	}{
		{http.MethodPost, "/api/v1/collections/some-collection/shots", `{"shot_id":"shot-1"}`},
		{http.MethodDelete, "/api/v1/collections/some-collection/shots/shot-1", ""},
		{http.MethodPost, "/api/v1/collections/some-collection/shots/reorder", `{"shot_ids":["shot-1"]}`},
	} {
		var body *bytes.Reader
		if tc.body != "" {
			body = bytes.NewReader([]byte(tc.body))
		} else {
			body = bytes.NewReader(nil)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.target, body))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d body=%s, want 401", tc.method, tc.target, response.Code, response.Body.String())
		}
		if !bytes.Contains(response.Body.Bytes(), []byte(`"admin_authentication_required"`)) {
			t.Fatalf("%s %s body=%s, want the admin envelope", tc.method, tc.target, response.Body.String())
		}
	}

	// A shot_id missing from the body is a 400, reached only after auth.
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/collections/some-collection/shots", bytes.NewBufferString(`{}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("empty shot_id status=%d body=%s, want 400", response.Code, response.Body.String())
	}
}
