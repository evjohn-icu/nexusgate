package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ev/timingdex/internal/app"
	"github.com/ev/timingdex/internal/config"
	"github.com/ev/timingdex/internal/domain"
	"github.com/ev/timingdex/internal/media"
	"github.com/ev/timingdex/internal/repository/sqlite"
)

// lanRequest stands in for the browser UI on the home network. httptest's
// synthetic RemoteAddr is 192.0.2.1 (TEST-NET-1), which requireTrustedRead
// correctly rejects, so a test that means "a user on the LAN" has to say so.
func lanRequest(method, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.RemoteAddr = "192.168.1.50:54321"
	return request
}

func TestHandlerServesHybridShotSearch(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
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
	if _, err := repo.UpsertScannedFile(ctx, root, "fixture.mp4", videoPath, info, "fixture-fingerprint"); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	if err := repo.ReplaceAssetShots(ctx, assets[0].ID, "", []domain.AssetShot{{ID: "shot-1", StartMS: 0, EndMS: 5000, Description: "雨夜城市街道", Tags: []string{"rain", "urban_night", "street"}}}); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer("", service)
	request := lanRequest(http.MethodGet, "/api/v1/search/shots/hybrid?q=rainy+city+night", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var hits []domain.ShotSearchResult
	if err := json.NewDecoder(response.Body).Decode(&hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "shot-1" || hits[0].SemanticScore == 0 {
		t.Fatalf("hits=%+v", hits)
	}
}

func TestHomePageUsesLibraryFirstShotTimeline(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "library-browser.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, marker := range []string{
		"data-library-browser",
		"loadShots",
		"/api/v1/assets/" + "'+encodeURIComponent(id)+'" + "/shots",
		"semantic-timeline",
		"cut-marker",
		"data-cut-time",
		"素材回收站 · 镜头浏览",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("homepage missing library-first marker %q", marker)
		}
	}
}

func TestWorkerCanPairAndHeartbeatThroughHubAPI(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "worker-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	pair := httptest.NewRecorder()
	pairRequest := httptest.NewRequest(http.MethodPost, "/api/v1/hub/worker-pairings", nil)
	pairRequest.Header.Set("Authorization", "Bearer "+service.AdminToken())
	handler.ServeHTTP(pair, pairRequest)
	if pair.Code != http.StatusCreated {
		t.Fatalf("pair status=%d body=%s", pair.Code, pair.Body.String())
	}
	var pairing struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(pair.Body).Decode(&pairing); err != nil {
		t.Fatal(err)
	}
	enroll := httptest.NewRecorder()
	handler.ServeHTTP(enroll, httptest.NewRequest(http.MethodPost, "/api/v1/worker/enroll", strings.NewReader(`{"pairing_token":"`+pairing.Token+`","name":"windows-gpu","platform":"windows-amd64","capabilities":{"proxy":true,"library_roots":["root-a"]}}`)))
	if enroll.Code != http.StatusCreated {
		t.Fatalf("enroll status=%d body=%s", enroll.Code, enroll.Body.String())
	}
	var enrolled struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(enroll.Body).Decode(&enrolled); err != nil {
		t.Fatal(err)
	}
	heartbeat := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/heartbeat", strings.NewReader(`{"capabilities":{"proxy":true,"max_parallel_proxy_jobs":1}}`))
	req.Header.Set("Authorization", "Bearer "+enrolled.Token)
	handler.ServeHTTP(heartbeat, req)
	if heartbeat.Code != http.StatusNoContent {
		t.Fatalf("heartbeat status=%d body=%s", heartbeat.Code, heartbeat.Body.String())
	}
}

func TestHubRejectsUnauthenticatedWorkerPairing(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "worker-pairing-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/hub/worker-pairings", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated pairing status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCollectionsAndProcessingSummaryExposeOnlySafeLibraryFilters(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "collections-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()
	body := `{"name":"待处理素材","description":"可重复使用的安全筛选","filter":{"status":"queued","region_label":"深圳"}}`
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, lanRequest(http.MethodPost, "/api/v1/collections", strings.NewReader(body)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized collection write=%d", unauthorized.Code)
	}
	created := httptest.NewRecorder()
	request := lanRequest(http.MethodPost, "/api/v1/collections", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+service.AdminToken())
	handler.ServeHTTP(created, request)
	if created.Code != http.StatusCreated {
		t.Fatalf("collection create=%d body=%s", created.Code, created.Body.String())
	}
	var collection domain.AssetCollection
	if err := json.NewDecoder(created.Body).Decode(&collection); err != nil {
		t.Fatal(err)
	}
	if collection.Filter.Status != domain.ProcessingStatusQueued || collection.Filter.RegionLabel != "深圳" || collection.ID == "" {
		t.Fatalf("collection=%+v", collection)
	}
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, lanRequest(http.MethodGet, "/api/v1/collections", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "待处理素材") || strings.Contains(list.Body.String(), "absolute_path") {
		t.Fatalf("collection list=%d body=%s", list.Code, list.Body.String())
	}
	summary := httptest.NewRecorder()
	handler.ServeHTTP(summary, lanRequest(http.MethodGet, "/api/v1/library/processing-summary?status=queued", nil))
	if summary.Code != http.StatusOK || !strings.Contains(summary.Body.String(), "by_status") {
		t.Fatalf("summary=%d body=%s", summary.Code, summary.Body.String())
	}
}

func TestHubRejectsUnauthenticatedLibraryRootWrite(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "root-write-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/roots", strings.NewReader(`{"path":"/not-authorized"}`)))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated root write status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestWorkersPageShowsNodeAndWorkflowProgressSurfaces(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "workers-page.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workers", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, marker := range []string{"处理节点", "工作流进度", "/api/v1/hub/workers", "/api/v1/jobs", "data-workers-page", "admin-token", "生成配对 Token", "Authorization"} {
		if !strings.Contains(response.Body.String(), marker) {
			t.Fatalf("workers page missing %q", marker)
		}
	}
}

func TestHandlerRevisesAndApprovesRepurposePlan(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "review-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer("", service)
	handler := server.Handler()

	create := httptest.NewRecorder()
	handler.ServeHTTP(create, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans", bytes.NewBufferString(`{"brief":"深圳城市宣传片"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var plan domain.RepurposePlan
	if err := json.NewDecoder(create.Body).Decode(&plan); err != nil {
		t.Fatal(err)
	}
	revisionBody := `{"editor_note":"人工调整开场","sections":[{"role":"opening","query":"city","duration_ms":5000,"required":true,"rationale":"建立城市"}]}`
	revise := httptest.NewRecorder()
	handler.ServeHTTP(revise, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+plan.ID+"/revisions", bytes.NewBufferString(revisionBody)))
	if revise.Code != http.StatusCreated {
		t.Fatalf("revise status=%d body=%s", revise.Code, revise.Body.String())
	}
	var revision domain.RepurposePlanRevision
	if err := json.NewDecoder(revise.Body).Decode(&revision); err != nil {
		t.Fatal(err)
	}
	if revision.Revision != 2 || revision.EditorNote != "人工调整开场" {
		t.Fatalf("revision=%+v", revision)
	}
	approve := httptest.NewRecorder()
	handler.ServeHTTP(approve, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+plan.ID+"/revisions/2/approve", nil))
	if approve.Code != http.StatusOK {
		t.Fatalf("approve status=%d body=%s", approve.Code, approve.Body.String())
	}
	locked := httptest.NewRecorder()
	handler.ServeHTTP(locked, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+plan.ID+"/revisions", bytes.NewBufferString(revisionBody)))
	if locked.Code != http.StatusConflict {
		t.Fatalf("locked status=%d body=%s", locked.Code, locked.Body.String())
	}
}

func TestHandlerRejectsRepurposeRevisionSelectingShotOutsideCandidates(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "selected-shot-validation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	create := httptest.NewRecorder()
	handler.ServeHTTP(create, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans", bytes.NewBufferString(`{"brief":"深圳城市宣传片"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var plan domain.RepurposePlan
	if err := json.NewDecoder(create.Body).Decode(&plan); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRecorder()
	body := `{"sections":[{"role":"opening","query":"city","duration_ms":5000,"required":true,"selected_shot_id":"not-a-candidate","candidates":[]}]}`
	handler.ServeHTTP(request, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+plan.ID+"/revisions", bytes.NewBufferString(body)))
	if request.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", request.Code, request.Body.String())
	}
	if !bytes.Contains(request.Body.Bytes(), []byte("selected")) {
		t.Fatalf("expected selected-shot validation error, got %s", request.Body.String())
	}
}

func TestHandlerRequiresExplicitUnlockBeforeReplacingLockedPlanSelection(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "locked-selection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
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
	if _, err := repo.UpsertScannedFile(ctx, root, "fixture.mp4", videoPath, info, "locked-selection-fingerprint"); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 1, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	if err := repo.ReplaceAssetShots(ctx, assets[0].ID, "", []domain.AssetShot{
		{ID: "opening-a", StartMS: 0, EndMS: 5000, Description: "城市夜景开场"},
		{ID: "opening-b", StartMS: 5000, EndMS: 10000, Description: "城市街道人流"},
	}); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	create := httptest.NewRecorder()
	handler.ServeHTTP(create, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans", bytes.NewBufferString(`{"brief":"深圳城市宣传片"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var plan domain.RepurposePlan
	if err := json.NewDecoder(create.Body).Decode(&plan); err != nil {
		t.Fatal(err)
	}

	lock := httptest.NewRecorder()
	lockBody := `{"sections":[{"role":"opening","query":"city","duration_ms":5000,"required":true,"selected_shot_id":"opening-a","locked":true,"candidates":[{"shot_id":"opening-a","asset_id":"` + assets[0].ID + `","start_ms":0,"end_ms":5000},{"shot_id":"opening-b","asset_id":"` + assets[0].ID + `","start_ms":5000,"end_ms":10000}]}]}`
	handler.ServeHTTP(lock, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+plan.ID+"/revisions", bytes.NewBufferString(lockBody)))
	if lock.Code != http.StatusCreated {
		t.Fatalf("lock status=%d body=%s", lock.Code, lock.Body.String())
	}
	if !bytes.Contains(lock.Body.Bytes(), []byte(`"locked":true`)) {
		t.Fatalf("locked state was not persisted: %s", lock.Body.String())
	}

	replace := httptest.NewRecorder()
	replaceBody := `{"sections":[{"role":"opening","query":"city","duration_ms":5000,"required":true,"selected_shot_id":"opening-b","locked":true,"candidates":[{"shot_id":"opening-a","asset_id":"` + assets[0].ID + `","start_ms":0,"end_ms":5000},{"shot_id":"opening-b","asset_id":"` + assets[0].ID + `","start_ms":5000,"end_ms":10000}]}]}`
	handler.ServeHTTP(replace, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+plan.ID+"/revisions", bytes.NewBufferString(replaceBody)))
	if replace.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", replace.Code, replace.Body.String())
	}
	if !bytes.Contains(replace.Body.Bytes(), []byte("locked")) {
		t.Fatalf("expected lock validation error, got %s", replace.Body.String())
	}

	unlock := httptest.NewRecorder()
	unlockBody := `{"sections":[{"role":"opening","query":"city","duration_ms":5000,"required":true,"selected_shot_id":"opening-b","unlock":true,"candidates":[{"shot_id":"opening-a","asset_id":"` + assets[0].ID + `","start_ms":0,"end_ms":5000},{"shot_id":"opening-b","asset_id":"` + assets[0].ID + `","start_ms":5000,"end_ms":10000}]}]}`
	handler.ServeHTTP(unlock, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+plan.ID+"/revisions", bytes.NewBufferString(unlockBody)))
	if unlock.Code != http.StatusCreated {
		t.Fatalf("unlock status=%d body=%s", unlock.Code, unlock.Body.String())
	}
	if bytes.Contains(unlock.Body.Bytes(), []byte(`"unlock":true`)) || bytes.Contains(unlock.Body.Bytes(), []byte(`"locked":true`)) {
		t.Fatalf("unlock action leaked into the saved revision: %s", unlock.Body.String())
	}
}

func TestHandlerRejectsRepurposeRevisionExcludingSelectedShot(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "excluded-selection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
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
	if _, err := repo.UpsertScannedFile(ctx, root, "fixture.mp4", videoPath, info, "excluded-selection-fingerprint"); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 1, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	if err := repo.ReplaceAssetShots(ctx, assets[0].ID, "", []domain.AssetShot{{ID: "opening-a", StartMS: 0, EndMS: 5000, Description: "城市夜景开场"}}); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	create := httptest.NewRecorder()
	handler.ServeHTTP(create, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans", bytes.NewBufferString(`{"brief":"深圳城市宣传片"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var plan domain.RepurposePlan
	if err := json.NewDecoder(create.Body).Decode(&plan); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRecorder()
	body := `{"sections":[{"role":"opening","query":"city","duration_ms":5000,"required":true,"selected_shot_id":"opening-a","excluded_shot_ids":["opening-a"],"candidates":[{"shot_id":"opening-a","asset_id":"` + assets[0].ID + `","start_ms":0,"end_ms":5000}]}]}`
	handler.ServeHTTP(request, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+plan.ID+"/revisions", bytes.NewBufferString(body)))
	if request.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", request.Code, request.Body.String())
	}
	if !bytes.Contains(request.Body.Bytes(), []byte("excluded")) {
		t.Fatalf("expected excluded-shot validation error, got %s", request.Body.String())
	}
}

func TestHandlerRejectsApprovalWhenRequiredSectionHasCandidateButNoSelection(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "approval-needs-selection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
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
	if _, err := repo.UpsertScannedFile(ctx, root, "fixture.mp4", videoPath, info, "approval-needs-selection-fingerprint"); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 1, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	if err := repo.ReplaceAssetShots(ctx, assets[0].ID, "", []domain.AssetShot{{ID: "opening-a", StartMS: 0, EndMS: 5000, Description: "城市夜景开场"}}); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	create := httptest.NewRecorder()
	handler.ServeHTTP(create, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans", bytes.NewBufferString(`{"brief":"深圳城市宣传片"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var plan domain.RepurposePlan
	if err := json.NewDecoder(create.Body).Decode(&plan); err != nil {
		t.Fatal(err)
	}

	revise := httptest.NewRecorder()
	revisionBody := `{"sections":[{"role":"opening","query":"city","duration_ms":5000,"required":true,"candidates":[{"shot_id":"opening-a","asset_id":"` + assets[0].ID + `","start_ms":0,"end_ms":5000}]}]}`
	handler.ServeHTTP(revise, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+plan.ID+"/revisions", bytes.NewBufferString(revisionBody)))
	if revise.Code != http.StatusCreated {
		t.Fatalf("revise status=%d body=%s", revise.Code, revise.Body.String())
	}
	var revision domain.RepurposePlanRevision
	if err := json.NewDecoder(revise.Body).Decode(&revision); err != nil {
		t.Fatal(err)
	}

	approve := httptest.NewRecorder()
	handler.ServeHTTP(approve, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+plan.ID+"/revisions/"+strconv.Itoa(revision.Revision)+"/approve", nil))
	if approve.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", approve.Code, approve.Body.String())
	}
	if !bytes.Contains(approve.Body.Bytes(), []byte("selection")) {
		t.Fatalf("expected selection error, got %s", approve.Body.String())
	}
}

func hubAdminRequest(service *app.Service, method, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.Header.Set("Authorization", "Bearer "+service.AdminToken())
	return request
}

func hubAgentRequest(service *app.Service, method, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.Header.Set("Authorization", "Bearer "+service.AgentToken())
	return request
}

// TestAgentTokenScope proves the boundary requireAgentOrAdmin is supposed to
// enforce: the agent token can create and revise a draft repurpose plan (the
// two routes documented in skills/timingdex) but is refused, by access
// control rather than convention, on approval and pipeline runs — the two
// actions /api/v1/agent/capabilities lists under denied_actions. It also
// checks the admin token still does all four, and that no credential at all
// is refused everywhere.
func TestAgentTokenScope(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "agent-token-scope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
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
	if _, err := repo.UpsertScannedFile(ctx, root, "fixture.mp4", videoPath, info, "agent-token-scope-fingerprint"); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 1, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	if err := repo.ReplaceAssetShots(ctx, assets[0].ID, "", []domain.AssetShot{{ID: "opening-a", StartMS: 0, EndMS: 5000, Description: "城市夜景开场"}}); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	if service.AgentToken() == "" || service.AgentToken() == service.AdminToken() {
		t.Fatalf("agent token must be a non-empty credential distinct from the admin token")
	}
	handler := NewServer("", service).Handler()

	// (a) the agent token can create a draft plan.
	create := httptest.NewRecorder()
	handler.ServeHTTP(create, hubAgentRequest(service, http.MethodPost, "/api/v1/repurpose/plans", bytes.NewBufferString(`{"brief":"深圳城市宣传片"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("agent create plan status=%d body=%s", create.Code, create.Body.String())
	}
	var plan domain.RepurposePlan
	if err := json.NewDecoder(create.Body).Decode(&plan); err != nil {
		t.Fatal(err)
	}

	// (a) the agent token can revise that draft plan.
	revisionBody := `{"sections":[{"role":"opening","query":"city","duration_ms":5000,"required":true,"selected_shot_id":"opening-a","candidates":[{"shot_id":"opening-a","asset_id":"` + assets[0].ID + `","start_ms":0,"end_ms":5000}]}]}`
	revise := httptest.NewRecorder()
	handler.ServeHTTP(revise, hubAgentRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+plan.ID+"/revisions", bytes.NewBufferString(revisionBody)))
	if revise.Code != http.StatusCreated {
		t.Fatalf("agent revise plan status=%d body=%s", revise.Code, revise.Body.String())
	}
	var revision domain.RepurposePlanRevision
	if err := json.NewDecoder(revise.Body).Decode(&revision); err != nil {
		t.Fatal(err)
	}

	// (b) the agent token cannot approve that revision.
	agentApprove := httptest.NewRecorder()
	handler.ServeHTTP(agentApprove, hubAgentRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+plan.ID+"/revisions/"+strconv.Itoa(revision.Revision)+"/approve", nil))
	if agentApprove.Code != http.StatusUnauthorized {
		t.Fatalf("agent approve status=%d body=%s, want 401", agentApprove.Code, agentApprove.Body.String())
	}

	// (c) the agent token cannot run the pipeline.
	agentPipeline := httptest.NewRecorder()
	handler.ServeHTTP(agentPipeline, hubAgentRequest(service, http.MethodPost, "/api/v1/pipeline/run", nil))
	if agentPipeline.Code != http.StatusUnauthorized {
		t.Fatalf("agent pipeline/run status=%d body=%s, want 401", agentPipeline.Code, agentPipeline.Body.String())
	}

	// (d) the admin token can still do all four: create, revise, approve, run.
	adminCreate := httptest.NewRecorder()
	handler.ServeHTTP(adminCreate, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans", bytes.NewBufferString(`{"brief":"深圳城市宣传片 2"}`)))
	if adminCreate.Code != http.StatusCreated {
		t.Fatalf("admin create plan status=%d body=%s", adminCreate.Code, adminCreate.Body.String())
	}
	var adminPlan domain.RepurposePlan
	if err := json.NewDecoder(adminCreate.Body).Decode(&adminPlan); err != nil {
		t.Fatal(err)
	}
	adminRevise := httptest.NewRecorder()
	handler.ServeHTTP(adminRevise, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+adminPlan.ID+"/revisions", bytes.NewBufferString(revisionBody)))
	if adminRevise.Code != http.StatusCreated {
		t.Fatalf("admin revise plan status=%d body=%s", adminRevise.Code, adminRevise.Body.String())
	}
	var adminRevision domain.RepurposePlanRevision
	if err := json.NewDecoder(adminRevise.Body).Decode(&adminRevision); err != nil {
		t.Fatal(err)
	}
	adminApprove := httptest.NewRecorder()
	handler.ServeHTTP(adminApprove, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+adminPlan.ID+"/revisions/"+strconv.Itoa(adminRevision.Revision)+"/approve", nil))
	if adminApprove.Code != http.StatusOK {
		t.Fatalf("admin approve status=%d body=%s", adminApprove.Code, adminApprove.Body.String())
	}
	adminPipeline := httptest.NewRecorder()
	handler.ServeHTTP(adminPipeline, hubAdminRequest(service, http.MethodPost, "/api/v1/pipeline/run", nil))
	if adminPipeline.Code != http.StatusAccepted {
		t.Fatalf("admin pipeline/run status=%d body=%s", adminPipeline.Code, adminPipeline.Body.String())
	}

	// (e) no credential at all still 401s on all four routes.
	noAuthCases := []struct {
		method string
		target string
		body   string
	}{
		{http.MethodPost, "/api/v1/repurpose/plans", `{"brief":"深圳城市宣传片"}`},
		{http.MethodPost, "/api/v1/repurpose/plans/" + plan.ID + "/revisions", revisionBody},
		{http.MethodPost, "/api/v1/repurpose/plans/" + plan.ID + "/revisions/" + strconv.Itoa(revision.Revision) + "/approve", ""},
		{http.MethodPost, "/api/v1/pipeline/run", ""},
	}
	for _, tc := range noAuthCases {
		response := httptest.NewRecorder()
		var body io.Reader
		if tc.body != "" {
			body = bytes.NewBufferString(tc.body)
		}
		handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.target, body))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("no-credential %s %s status=%d body=%s, want 401", tc.method, tc.target, response.Code, response.Body.String())
		}
	}
}

func TestHandlerServesLocalWorkspacePages(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "workspace-pages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	for _, page := range []struct {
		path   string
		marker string
	}{
		{path: "/", marker: "素材库"},
		{path: "/setup", marker: "启动配置"},
		{path: "/progress", marker: "处理进度"},
		{path: "/repurpose", marker: "翻新工作台"},
	} {
		t.Run(page.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, page.path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if contentType := response.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
				t.Fatalf("content-type=%q", contentType)
			}
			if !bytes.Contains(response.Body.Bytes(), []byte(page.marker)) {
				t.Fatalf("page %s missing marker %q", page.path, page.marker)
			}
		})
	}
}

func TestProvidersPageShowsCapabilityGroupsAndEphemeralAdminToken(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "providers-page.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/providers", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
		t.Fatalf("content-type=%q", contentType)
	}
	body := response.Body.String()
	for _, marker := range []string{
		"能力与服务",
		"Video analysis",
		"ASR",
		"Embedding",
		"Tag curation",
		"Repurpose",
		`id="admin-token"`,
		`type="password"`,
		"当前页面",
		"API Key",
		"adminHeaders",
		`id="channel-form"`,
		`id="capability"`,
		`id="provider-name"`,
		`id="api-key"`,
		"loadChannels",
		"saveChannel",
		"fetch('/api/v1/admin/provider-channels",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("providers page missing %q", marker)
		}
	}
	if strings.Contains(body, "localStorage") || strings.Contains(body, "sessionStorage") {
		t.Fatalf("providers page must not persist credentials in browser storage")
	}
}

func TestSetupPageDelegatesProviderKeysToProtectedChannelManager(t *testing.T) {
	response := httptest.NewRecorder()
	NewServer("", nil).setupPage(response, httptest.NewRequest(http.MethodGet, "/setup", nil))
	body := response.Body.String()
	if !strings.Contains(body, `href="/providers"`) || !strings.Contains(body, "模型通道") {
		t.Fatalf("setup page must direct provider configuration to the protected channel manager")
	}
	for _, leakedPattern := range []string{`id="key"`, "GEMINI_API_KEY", "DASHSCOPE_API_KEY", "ARK_API_KEY"} {
		if strings.Contains(body, leakedPattern) {
			t.Fatalf("setup page still generates plaintext provider credentials via %q", leakedPattern)
		}
	}
}

func TestPublicAssetDetailHidesPreciseLocationAndAbsolutePath(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "asset-location-privacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootPath := t.TempDir()
	videoPath := filepath.Join(rootPath, "DJI_0001.mp4")
	if err := os.WriteFile(videoPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := service.AddLibraryRoot(ctx, rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ScanLibraryRoot(ctx, root.ID); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	latitude, longitude := 22.543096, 114.057865
	if err := repo.SaveMediaMetadata(ctx, assets[0].ID, domain.MediaMetadata{Latitude: &latitude, Longitude: &longitude, CameraModel: "DJI Mavic 3"}, "privacy-fixture"); err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	public := httptest.NewRecorder()
	handler.ServeHTTP(public, lanRequest(http.MethodGet, "/api/v1/assets/"+assets[0].ID, nil))
	if public.Code != http.StatusOK {
		t.Fatalf("public detail status=%d body=%s", public.Code, public.Body.String())
	}
	if strings.Contains(public.Body.String(), "22.543096") || strings.Contains(public.Body.String(), "114.057865") || strings.Contains(public.Body.String(), rootPath) {
		t.Fatalf("public asset detail leaked precise location or absolute path: %s", public.Body.String())
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, lanRequest(http.MethodGet, "/api/v1/admin/assets/"+assets[0].ID+"/capture-location", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("precise location without admin status=%d", unauthorized.Code)
	}

	admin := httptest.NewRecorder()
	request := lanRequest(http.MethodGet, "/api/v1/admin/assets/"+assets[0].ID+"/capture-location", nil)
	request.Header.Set("Authorization", "Bearer "+service.AdminToken())
	handler.ServeHTTP(admin, request)
	if admin.Code != http.StatusOK || !strings.Contains(admin.Body.String(), "22.543096") || !strings.Contains(admin.Body.String(), "114.057865") {
		t.Fatalf("admin precise location status=%d body=%s", admin.Code, admin.Body.String())
	}
}

func TestLibraryPageLinksToProviders(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "providers-nav.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `href="/providers"`) {
		t.Fatalf("library page missing providers navigation link")
	}
}

func TestLibraryPageRendersOptionalAssetMetadataWithoutChangingLayout(t *testing.T) {
	for _, field := range []string{
		"camera_model",
		"region_label",
		"session_id",
		"source_color",
		"color_profile",
		"raw_format",
		"preview_status",
	} {
		if !strings.Contains(libraryIndexHTML, "x."+field) {
			t.Fatalf("library page missing optional card field %q", field)
		}
	}
	if !strings.Contains(libraryIndexHTML, "optionalDetails") || !strings.Contains(libraryIndexHTML, ".filter(([,value])=>value)") {
		t.Fatalf("library page must render optional metadata only when present")
	}
	for _, marker := range []string{
		".asset-row{display:grid;grid-template-columns:220px minmax(220px,.75fr) minmax(380px,1.75fr)",
		".asset-info",
		".timeline-card",
		"asset-details",
		`id="date-from"`,
		`id="date-to"`,
		`id="region-filter"`,
		`id="camera-filter"`,
		`id="session-filter"`,
		"filterQuery",
		"groupKey",
		"date_from",
		"region",
		"camera",
		"session",
		"loadSessions",
		"/api/v1/shoot-sessions?limit=500",
		`<select id="session-filter"`,
		"option.value=s.id",
		"const details=[date,s.camera_label,s.region_label]",
		"document.getElementById('session-filter').value",
	} {
		if !strings.Contains(libraryIndexHTML, marker) {
			t.Fatalf("library layout missing %q", marker)
		}
	}
}

func TestShootSessionBrowseEndpointFiltersSafely(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "shoot-sessions-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	primaryStart := time.Date(2026, time.July, 25, 10, 5, 0, 0, time.UTC)
	if err := repo.SaveShootSession(ctx, domain.ShootSession{
		ID:          "session-shenzhen-01",
		Title:       "深圳夜景拍摄",
		State:       "automatic",
		StartsAt:    &primaryStart,
		RegionLabel: "中国 · 深圳 · 南山",
		CameraLabel: "Blackmagic Pocket Cinema Camera 6K",
		Confidence:  0.94,
	}); err != nil {
		t.Fatal(err)
	}
	otherStart := primaryStart.AddDate(0, 0, -2)
	if err := repo.SaveShootSession(ctx, domain.ShootSession{
		ID:          "session-singapore-01",
		Title:       "Singapore daylight",
		State:       "manual",
		StartsAt:    &otherStart,
		RegionLabel: "Singapore",
		CameraLabel: "DJI Osmo Pocket 3",
		Confidence:  0.71,
	}); err != nil {
		t.Fatal(err)
	}

	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	request := lanRequest(http.MethodGet, "/api/v1/shoot-sessions?region="+url.QueryEscape("中国 · 深圳 · 南山")+"&camera="+url.QueryEscape("Blackmagic Pocket Cinema Camera 6K")+"&date_from=2026-07-25&date_to=2026-07-25", nil)
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var sessions []domain.ShootSession
	if err := json.NewDecoder(response.Body).Decode(&sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "session-shenzhen-01" {
		t.Fatalf("sessions=%+v", sessions)
	}
	body := response.Body.String()
	for _, forbidden := range []string{"latitude", "longitude", "absolute_path", "/Volumes/", "/mnt/"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("browse response leaked %q: %s", forbidden, body)
		}
	}
}

func TestProviderChannelAPIRequiresAdminAndNeverReturnsKeys(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "provider-channel-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()
	body := `{"capability":"video_analysis","label":"Gemini Flash","provider_name":"gemini","protocol":"gemini_generate_content","endpoint":"https://example.invalid","model":"gemini-flash","enabled":true,"members":[{"label":"primary","api_key":"api-key-must-not-return","enabled":true,"weight":1,"max_inflight":2}]}`

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels", strings.NewReader(body)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized create status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	create := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+service.AdminToken())
	handler.ServeHTTP(create, request)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	if strings.Contains(create.Body.String(), "api-key-must-not-return") || strings.Contains(create.Body.String(), "secret_ref") {
		t.Fatalf("create response leaked secret material: %s", create.Body.String())
	}

	list := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/provider-channels?capability=video_analysis", nil)
	request.Header.Set("Authorization", "Bearer "+service.AdminToken())
	handler.ServeHTTP(list, request)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "api-key-must-not-return") || strings.Contains(list.Body.String(), "secret_ref") {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), `"secret_ready":true`) {
		t.Fatalf("list did not report secret readiness: %s", list.Body.String())
	}
}

func TestProviderChannelOperationsRequireAdminAndNeverExposeSecrets(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "provider-channel-operations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()
	admin := service.AdminToken()
	createBody := `{"capability":"video_analysis","label":"Video primary","provider_name":"gemini","endpoint":"://invalid","model":"gemini-flash","enabled":true,"members":[{"label":"primary","api_key":"operation-secret","enabled":true,"weight":1,"max_inflight":2}]}`
	create := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels", strings.NewReader(createBody))
	request.Header.Set("Authorization", "Bearer "+admin)
	handler.ServeHTTP(create, request)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var channel domain.ProviderChannel
	if err := json.NewDecoder(create.Body).Decode(&channel); err != nil {
		t.Fatal(err)
	}
	if channel.ID == "" {
		t.Fatal("create did not return channel id")
	}
	if strings.Contains(create.Body.String(), "operation-secret") || strings.Contains(create.Body.String(), "secret_ref") {
		t.Fatalf("create response exposed secret material: %s", create.Body.String())
	}

	for _, method := range []string{http.MethodPatch, http.MethodPost, http.MethodDelete} {
		unauthorized := httptest.NewRecorder()
		path := "/api/v1/admin/provider-channels/" + channel.ID
		if method == http.MethodPost {
			path += "/disable"
		}
		handler.ServeHTTP(unauthorized, httptest.NewRequest(method, path, strings.NewReader(`{"label":"should-not-apply"}`)))
		if unauthorized.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized %s status=%d body=%s", method, unauthorized.Code, unauthorized.Body.String())
		}
	}

	patch := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPatch, "/api/v1/admin/provider-channels/"+channel.ID, strings.NewReader(`{"label":"Video fallback","model":"gemini-2.5-flash"}`))
	request.Header.Set("Authorization", "Bearer "+admin)
	handler.ServeHTTP(patch, request)
	if patch.Code != http.StatusOK || strings.Contains(patch.Body.String(), "operation-secret") || strings.Contains(patch.Body.String(), "secret_ref") {
		t.Fatalf("patch status=%d body=%s", patch.Code, patch.Body.String())
	}
	if !strings.Contains(patch.Body.String(), "Video fallback") {
		t.Fatalf("patch did not update label: %s", patch.Body.String())
	}

	disable := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels/"+channel.ID+"/disable", nil)
	request.Header.Set("Authorization", "Bearer "+admin)
	handler.ServeHTTP(disable, request)
	if disable.Code != http.StatusOK || !strings.Contains(disable.Body.String(), `"enabled":false`) || strings.Contains(disable.Body.String(), "secret_ref") {
		t.Fatalf("disable status=%d body=%s", disable.Code, disable.Body.String())
	}

	enable := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels/"+channel.ID+"/enable", nil)
	request.Header.Set("Authorization", "Bearer "+admin)
	handler.ServeHTTP(enable, request)
	if enable.Code != http.StatusOK || !strings.Contains(enable.Body.String(), `"enabled":true`) || strings.Contains(enable.Body.String(), "secret_ref") {
		t.Fatalf("enable status=%d body=%s", enable.Code, enable.Body.String())
	}

	testEndpoint := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/provider-channels/"+channel.ID+"/test", nil)
	request.Header.Set("Authorization", "Bearer "+admin)
	handler.ServeHTTP(testEndpoint, request)
	if testEndpoint.Code != http.StatusOK || !strings.Contains(testEndpoint.Body.String(), `"status":"invalid_endpoint"`) || strings.Contains(testEndpoint.Body.String(), "operation-secret") || strings.Contains(testEndpoint.Body.String(), "secret_ref") {
		t.Fatalf("connection test status=%d body=%s", testEndpoint.Code, testEndpoint.Body.String())
	}

	deleted := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodDelete, "/api/v1/admin/provider-channels/"+channel.ID, nil)
	request.Header.Set("Authorization", "Bearer "+admin)
	handler.ServeHTTP(deleted, request)
	if deleted.Code != http.StatusNoContent || strings.Contains(deleted.Body.String(), "operation-secret") || strings.Contains(deleted.Body.String(), "secret_ref") {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}

	list := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/provider-channels?capability=video_analysis", nil)
	request.Header.Set("Authorization", "Bearer "+admin)
	handler.ServeHTTP(list, request)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), channel.ID) || strings.Contains(list.Body.String(), "operation-secret") || strings.Contains(list.Body.String(), "secret_ref") {
		t.Fatalf("deleted channel still visible or leaked secret: status=%d body=%s", list.Code, list.Body.String())
	}
}

func TestHandlerServesRepurposeSelectionWorkspace(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "repurpose-selection-page.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/repurpose", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, marker := range []string{"保存编辑版", "选择此镜头", "锁定选择", "排除"} {
		if !bytes.Contains(response.Body.Bytes(), []byte(marker)) {
			t.Fatalf("repurpose selection workspace missing marker %q", marker)
		}
	}
}

func TestHandlerDeclaresBoundedAgentCapabilities(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "agent-capabilities.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/agent/capabilities", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var capabilities struct {
		Version        string   `json:"version"`
		ApprovalMode   string   `json:"approval_mode"`
		AllowedActions []string `json:"allowed_actions"`
		DeniedActions  []string `json:"denied_actions"`
	}
	if err := json.NewDecoder(response.Body).Decode(&capabilities); err != nil {
		t.Fatal(err)
	}
	if capabilities.Version != "v0.13" || capabilities.ApprovalMode != "human_required" {
		t.Fatalf("capabilities=%+v", capabilities)
	}
	if !containsString(capabilities.AllowedActions, "create_draft_plan") || containsString(capabilities.AllowedActions, "approve_plan") || !containsString(capabilities.DeniedActions, "approve_plan") {
		t.Fatalf("capabilities=%+v", capabilities)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestHandlerListsEmptyJobsAsJSONArray(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "empty-jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, hubAdminRequest(service, http.MethodGet, "/api/v1/jobs", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Body.String(); got != "[]\n" {
		t.Fatalf("empty jobs response=%q, want JSON array", got)
	}
}

// v0.14.1 put the admin token in front of tag curation, pipeline runs and
// repurpose writes but only taught /workers to send it, so every write button on
// these three pages returned 401 for several releases. Pin the plumbing: each
// page must expose a token field and attach an Authorization header.
func TestAdminGatedPagesCarryTokenPlumbing(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "page-token.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()
	for path, markers := range map[string][]string{
		"/tags":      {"admin-token", "Authorization", "/api/v1/tags/curate"},
		"/repurpose": {"admin-token", "Authorization", "/api/v1/repurpose/plans"},
		"/progress":  {"admin-token", "Authorization", "/api/v1/pipeline/run"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, response.Code)
		}
		for _, marker := range markers {
			if !strings.Contains(response.Body.String(), marker) {
				t.Fatalf("%s page missing %q", path, marker)
			}
		}
	}
}

// The progress page polls the queue every few seconds, so job status is public.
// The failure text is not: last_error_message can hold a truncated upstream
// provider body, which is exactly where a relay's echoed key would surface.
func TestJobsEndpointRedactsFailureTextFromPublicCallers(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "jobs-redaction.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "DJI_0002.mp4"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := service.AddLibraryRoot(ctx, rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ScanLibraryRoot(ctx, root.ID); err != nil {
		t.Fatal(err)
	}
	leased, err := repo.LeaseNextJob(ctx, "redaction-test", time.Minute)
	if err != nil || leased == nil {
		t.Fatalf("lease job=%+v err=%v", leased, err)
	}
	const upstream = "provider echoed sk-live-DEADBEEF0123"
	if err := repo.CompleteJob(ctx, leased.ID, domain.JobFailed, upstream); err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	public := httptest.NewRecorder()
	handler.ServeHTTP(public, lanRequest(http.MethodGet, "/api/v1/jobs", nil))
	if public.Code != http.StatusOK {
		t.Fatalf("public jobs status=%d body=%s", public.Code, public.Body.String())
	}
	if strings.Contains(public.Body.String(), upstream) {
		t.Fatalf("public jobs response leaked upstream failure text: %s", public.Body.String())
	}
	if !strings.Contains(public.Body.String(), `"has_error":true`) {
		t.Fatalf("public jobs response should still flag the failure: %s", public.Body.String())
	}

	admin := httptest.NewRecorder()
	handler.ServeHTTP(admin, hubAdminRequest(service, http.MethodGet, "/api/v1/jobs", nil))
	if admin.Code != http.StatusOK || !strings.Contains(admin.Body.String(), upstream) {
		t.Fatalf("admin jobs status=%d body=%s", admin.Code, admin.Body.String())
	}
}

// The library browse, search and media routes carry no token so the browser UI
// works without one on the LAN. That is a sound home-network trade-off and a
// full disclosure of the library the moment the port is forwarded, so the
// source network is the boundary that keeps it a trade-off.
func TestTrustedNetworkGuardOnUnauthenticatedReads(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "trusted-read.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	guarded := []string{"/api/v1/assets", "/api/v1/search?q=x", "/api/v1/jobs", "/api/v1/tags", "/api/v1/shoot-sessions", "/api/v1/collections", "/api/v1/library/summary", "/api/v1/hardware"}
	for _, target := range guarded {
		remote := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.RemoteAddr = "203.0.113.7:44321"
		handler.ServeHTTP(remote, request)
		if remote.Code != http.StatusForbidden {
			t.Fatalf("%s must not answer an anonymous internet caller: status=%d body=%s", target, remote.Code, remote.Body.String())
		}

		lan := httptest.NewRecorder()
		handler.ServeHTTP(lan, lanRequest(http.MethodGet, target, nil))
		if lan.Code == http.StatusForbidden {
			t.Fatalf("%s must stay usable from the LAN without a token: body=%s", target, lan.Body.String())
		}

		// A credential outranks topology: an admin working away from home is
		// still an admin. Otherwise the guard would break remote use entirely
		// rather than close the anonymous hole.
		token := httptest.NewRecorder()
		handler.ServeHTTP(token, hubAdminRequest(service, http.MethodGet, target, nil))
		if token.Code == http.StatusForbidden {
			t.Fatalf("%s must admit an off-network admin token: body=%s", target, token.Body.String())
		}
	}

	// A forwarded header is attacker-controlled on a directly exposed
	// listener, so claiming a LAN address in one must change nothing.
	spoofed := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/assets", nil)
	request.RemoteAddr = "203.0.113.7:44321"
	request.Header.Set("X-Forwarded-For", "192.168.1.50")
	request.Header.Set("X-Real-IP", "127.0.0.1")
	handler.ServeHTTP(spoofed, request)
	if spoofed.Code != http.StatusForbidden {
		t.Fatalf("forwarded headers must not grant trust: status=%d", spoofed.Code)
	}

	// The pages themselves carry no library data and are where the token is
	// pasted, so locking them to the LAN would only make remote access
	// impossible without protecting anything.
	page := httptest.NewRecorder()
	pageRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	pageRequest.RemoteAddr = "203.0.113.7:44321"
	handler.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK {
		t.Fatalf("UI shell status=%d", page.Code)
	}
}

// An explicit allowlist replaces the built-in ranges instead of extending them.
func TestTrustedReadNetworksConfigReplacesDefaults(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "trusted-read-config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}}
	cfg.HubSecurity.TrustedReadNetworks = []string{"10.9.0.0/16"}
	service, err := app.NewService(repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	for address, want := range map[string]int{"10.9.4.4:1": http.StatusOK, "192.168.1.50:1": http.StatusForbidden, "127.0.0.1:1": http.StatusForbidden} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/assets", nil)
		request.RemoteAddr = address
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s: status=%d want=%d body=%s", address, response.Code, want, response.Body.String())
		}
	}
}

func TestHandlerSuppressesMissingFaviconNoise(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "favicon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want no-content favicon response", response.Code)
	}
}
