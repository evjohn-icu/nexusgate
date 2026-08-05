package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/remote"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// --- P2-1: writeError 500 regression -------------------------------------------------

// workerCompleteJob: invalid state returns 400, not 500.
func TestWorkerCompleteJobRejectsInvalidState(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "complete-invalid-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, workerToken, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{
		Name:         "p2-worker",
		Platform:     "linux-amd64",
		Capabilities: remote.WorkerCapabilities{Proxy: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	// An invalid state (not succeeded or failed) must be rejected as 400.
	body := `{"state":"running","message":"done"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/job-1/complete", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+workerToken)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("invalid state: got %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
}

// workerCompleteJob: ErrJobLeaseLost returns 409 Conflict.
func TestWorkerCompleteJobReportsLeaseLostAs409(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "complete-lease-lost.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	assetID := scanOneAsset(t, repo, "lease-clip.mov")
	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "lease-input", 90); err != nil {
		t.Fatal(err)
	}

	pairingA, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, workerTokenA, err := repo.EnrollWorker(ctx, pairingA.Token, remote.WorkerRegistration{
		Name:         "worker-a",
		Platform:     "linux-amd64",
		Capabilities: remote.WorkerCapabilities{Proxy: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Enroll a second worker.
	pairingB, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, workerTokenB, err := repo.EnrollWorker(ctx, pairingB.Token, remote.WorkerRegistration{
		Name:         "worker-b",
		Platform:     "linux-amd64",
		Capabilities: remote.WorkerCapabilities{Proxy: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	// Worker A leases a derive job.
	leased, err := repo.LeaseNextJob(ctx, "worker-a", 30*time.Second, domain.LeaseFilter{})
	if err != nil || leased == nil {
		t.Fatalf("lease failed: err=%v job=%+v", err, leased)
	}
	_ = workerTokenA

	// Worker B tries to complete Worker A's job — lease ownership fails.
	completeBody := `{"state":"succeeded","message":"done"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+leased.ID+"/complete", strings.NewReader(completeBody))
	req.Header.Set("Authorization", "Bearer "+workerTokenB)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusConflict {
		t.Fatalf("non-owner complete: got %d, want 409; body=%s", resp.Code, resp.Body.String())
	}
}

// setWorkerJobAssignment: invalid mode returns 400.
func TestSetWorkerJobAssignmentRejectsInvalidMode(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "assign-bad-mode.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	body := `{"mode":"banana","worker_id":"w-1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/worker-jobs/job-1/assignment", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+service.AdminToken())
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: got %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "unsupported worker assignment mode") {
		t.Fatalf("bad mode: body should mention unsupported mode; body=%s", resp.Body.String())
	}
}

// setWorkerJobAssignment: missing worker_id when mode is not "any" returns 400.
func TestSetWorkerJobAssignmentRejectsEmptyWorkerIDForNonAnyMode(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "assign-empty-worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	for _, mode := range []string{"preferred", "required"} {
		body := `{"mode":"` + mode + `","worker_id":""}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/worker-jobs/job-1/assignment", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+service.AdminToken())
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("mode=%s empty worker_id: got %d, want 400; body=%s", mode, resp.Code, resp.Body.String())
		}
	}
}

// setWorkerJobAssignment: non-existent derive job returns 400.
func TestSetWorkerJobAssignmentRejectsNonExistentJob(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "assign-no-job.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	body := `{"mode":"preferred","worker_id":"w-1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/worker-jobs/nonexistent/assignment", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+service.AdminToken())
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("no such job: got %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
}

// saveCollection: empty name returns 400.
func TestSaveCollectionRejectsEmptyName(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "collection-empty-name.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	body := `{"name":"  ","description":"test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/collections", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+service.AdminToken())
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("empty name: got %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "name is required") {
		t.Fatalf("empty name: body should mention name required; body=%s", resp.Body.String())
	}
}

// saveCollection: too-long name returns 422.
func TestSaveCollectionRejectsTooLongName(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "collection-long-name.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	body := `{"name":"` + strings.Repeat("n", 201) + `","description":"test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/collections", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+service.AdminToken())
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("long name: got %d, want 422; body=%s", resp.Code, resp.Body.String())
	}
}

// saveCollection: too-long description returns 422.
func TestSaveCollectionRejectsTooLongDescription(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "collection-long-desc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	body := `{"name":"valid","description":"` + strings.Repeat("d", 2001) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/collections", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+service.AdminToken())
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("long description: got %d, want 422; body=%s", resp.Code, resp.Body.String())
	}
}

// --- P2-2: serveArtifact path validation ---------------------------------------------

// serveArtifact: refuses a path outside DataDir (string-prefix bypass).
func TestServeArtifactRejectsPathOutsideDataDir(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "artifact-boundary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	evilPath := dataDir + "-evil/secret.txt"
	if err := os.MkdirAll(filepath.Dir(evilPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evilPath, []byte("stolen"), 0o600); err != nil {
		t.Fatal(err)
	}

	assetID := scanOneAsset(t, repo, "boundary-clip.mov")
	// Create a derive job, lease it, then save artifact through the
	// lease-owning worker so the row passes ownership validation.
	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "boundary-input", 90); err != nil {
		t.Fatal(err)
	}
	leased, err := repo.LeaseNextJob(ctx, "worker-boundary", 30*time.Second, domain.LeaseFilter{})
	if err != nil || leased == nil {
		t.Fatalf("lease failed: err=%v job=%+v", err, leased)
	}
	if err := repo.SaveArtifact(ctx, domain.DerivedArtifact{
		ID:        "art-boundary-1",
		AssetID:   assetID,
		Type:      "thumbnail",
		LocalPath: evilPath,
		SizeBytes: 6,
	}, leased.ID, "worker-boundary"); err != nil {
		t.Fatal(err)
	}

	service, err := app.NewService(repo, config.Config{
		DataDir:  dataDir,
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	req := lanRequest(http.MethodGet, "/api/v1/assets/"+assetID+"/thumbnail", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("path outside DataDir: got %d, want 404; body=%s", resp.Code, resp.Body.String())
	}
}

// serveArtifact: refuses when DataDir is empty.
func TestServeArtifactRejectsWhenDataDirIsEmpty(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "artifact-no-datadir.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	assetID := scanOneAsset(t, repo, "nodir-clip.mov")
	validPath := filepath.Join(t.TempDir(), "valid-thumb.jpg")
	if err := os.WriteFile(validPath, []byte("jpeg-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "nodir-input", 90); err != nil {
		t.Fatal(err)
	}
	leased, err := repo.LeaseNextJob(ctx, "worker-nodir", 30*time.Second, domain.LeaseFilter{})
	if err != nil || leased == nil {
		t.Fatalf("lease failed: err=%v job=%+v", err, leased)
	}
	if err := repo.SaveArtifact(ctx, domain.DerivedArtifact{
		ID:        "art-nodir-1",
		AssetID:   assetID,
		Type:      "thumbnail",
		LocalPath: validPath,
		SizeBytes: 9,
	}, leased.ID, "worker-nodir"); err != nil {
		t.Fatal(err)
	}

	// Build a service with no DataDir.
	service, err := app.NewService(repo, config.Config{
		DataDir:  "",
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	req := lanRequest(http.MethodGet, "/api/v1/assets/"+assetID+"/thumbnail", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("empty DataDir: got %d, want 404; body=%s", resp.Code, resp.Body.String())
	}
}

// serveArtifact: serves a valid path inside DataDir.
func TestServeArtifactServesValidPathInsideDataDir(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "artifact-valid.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	cacheDir := filepath.Join(dataDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}

	assetID := scanOneAsset(t, repo, "valid-clip.mov")
	thumbPath := filepath.Join(cacheDir, assetID, "thumbnail-software.jpg")
	if err := os.MkdirAll(filepath.Dir(thumbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumbPath, []byte("jpeg-data"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "valid-input", 90); err != nil {
		t.Fatal(err)
	}
	leased, err := repo.LeaseNextJob(ctx, "worker-valid", 30*time.Second, domain.LeaseFilter{})
	if err != nil || leased == nil {
		t.Fatalf("lease failed: err=%v job=%+v", err, leased)
	}
	if err := repo.SaveArtifact(ctx, domain.DerivedArtifact{
		ID:        "art-valid-1",
		AssetID:   assetID,
		Type:      "thumbnail",
		LocalPath: thumbPath,
		SizeBytes: 9,
	}, leased.ID, "worker-valid"); err != nil {
		t.Fatal(err)
	}

	service, err := app.NewService(repo, config.Config{
		DataDir:  dataDir,
		CacheDir: cacheDir,
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	req := lanRequest(http.MethodGet, "/api/v1/assets/"+assetID+"/thumbnail", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("valid path: got %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	if resp.Body.Len() == 0 {
		t.Fatal("valid path: body should not be empty")
	}
}

// --- P2-3: MaxBytesReader tail-bypass ------------------------------------------------

// workerCompleteJob: valid JSON followed by trailing bytes within the limit returns
// 400 Bad Request (garbage), not 413 (size).
func TestWorkerCompleteJobRejectsTrailingBytes(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "complete-trailing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, workerToken, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{
		Name:         "trailing-worker",
		Platform:     "linux-amd64",
		Capabilities: remote.WorkerCapabilities{Proxy: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	// A valid JSON object followed by 16 KiB of trailing garbage — total
	// is well under the 32 KiB limit, so the size check passes and the
	// trailing content is rejected as a malformed body (400).
	body := `{"state":"succeeded","message":"done"}` + strings.Repeat("x", 16<<10)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/job-1/complete", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+workerToken)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("trailing bytes within limit: got %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "trailing content") {
		t.Fatalf("trailing bytes: body should mention trailing content; body=%s", resp.Body.String())
	}
}

// workerCompleteJob: body exceeding the size limit returns 413, not 400.
func TestWorkerCompleteJobRejectsOversizedBody(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "complete-oversized.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, workerToken, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{
		Name:         "oversized-worker",
		Platform:     "linux-amd64",
		Capabilities: remote.WorkerCapabilities{Proxy: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	// Body exceeds the 32 KiB limit — MaxBytesError must yield 413.
	body := `{"state":"succeeded","message":"` + strings.Repeat("x", 33<<10) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/job-1/complete", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+workerToken)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: got %d, want 413; body=%s", resp.Code, resp.Body.String())
	}
}

// workerHeartbeat: valid JSON followed by trailing bytes within the limit returns
// 400 Bad Request.
func TestWorkerHeartbeatRejectsTrailingBytes(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "heartbeat-trailing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, workerToken, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{
		Name:         "hb-trailing",
		Platform:     "linux-amd64",
		Capabilities: remote.WorkerCapabilities{Proxy: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	// A valid JSON object followed by 8 KiB of trailing garbage within
	// the 16 KiB limit → 400.
	body := `{"capabilities":{}}` + strings.Repeat("x", 8<<10)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/heartbeat", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+workerToken)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("trailing bytes heartbeat within limit: got %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "trailing content") {
		t.Fatalf("trailing bytes heartbeat: body should mention trailing content; body=%s", resp.Body.String())
	}
}

// workerProgress: oversized body returns 413 (MaxBytesError), not a generic 400.
func TestWorkerProgressRejectsOversizedBody(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "progress-oversized.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, workerToken, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{
		Name:         "progress-oversized",
		Platform:     "linux-amd64",
		Capabilities: remote.WorkerCapabilities{Proxy: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	// Body exceeds the 16 KiB progress limit → must be 413, not 400.
	body := `{"stage":"` + strings.Repeat("x", 17<<10) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/job-1/progress", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+workerToken)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("progress oversized body: got %d, want 413; body=%s", resp.Code, resp.Body.String())
	}
}

// workerProgress: valid JSON followed by trailing bytes within limit returns 400.
func TestWorkerProgressRejectsTrailingBytes(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "progress-trailing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, workerToken, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{
		Name:         "progress-trailing",
		Platform:     "linux-amd64",
		Capabilities: remote.WorkerCapabilities{Proxy: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	// Valid JSON + 4 KiB trailing within 16 KiB limit → 400.
	body := `{"stage":"derive","progress":0.5}` + strings.Repeat("x", 4<<10)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/job-1/progress", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+workerToken)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("progress trailing bytes: got %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
}

// --- P2-4: denied_actions completeness -------------------------------------------------

func TestAgentCapabilitiesDeniedActionsCoverAllAdminRoutes(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "capabilities-p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/v1/agent/capabilities", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}

	var capabilities struct {
		DeniedActions []string `json:"denied_actions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&capabilities); err != nil {
		t.Fatal(err)
	}

	// Every entry added in P2-4 must be present.
	required := []string{
		"manage_provider_channels",
		"manage_webdav_spaces",
		"manage_pipeline_throttle",
		"manage_library_summary",
	}
	for _, action := range required {
		if !containsString(capabilities.DeniedActions, action) {
			t.Fatalf("denied_actions missing %q; have %+v", action, capabilities.DeniedActions)
		}
	}

	// Count: the original 10 + 4 new = 14.
	if len(capabilities.DeniedActions) != 14 {
		t.Fatalf("denied_actions count=%d, want 14; list=%+v", len(capabilities.DeniedActions), capabilities.DeniedActions)
	}
}

// Agent token must not access any of the newly-declared denied actions.
func TestAgentTokenCannotAccessNewAdminActions(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "agent-denied-new.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	tests := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPut, "/api/v1/pipeline/throttle", `{"max_concurrent_jobs":1}`},
		{http.MethodPost, "/api/v1/library/summary/generate", `{}`},
		{http.MethodPost, "/api/v1/admin/provider-channels", `{"capability":"asr","label":"test","provider_name":"openai","protocol":"openai_whisper","endpoint":"https://example.invalid","enabled":false}`},
		{http.MethodPost, "/api/v1/admin/webdav/spaces", `{"label":"test-space"}`},
	}

	for _, tc := range tests {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+service.AgentToken())
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)

		if resp.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with agent token: got %d, want 401; body=%s", tc.method, tc.path, resp.Code, resp.Body.String())
		}
	}
}

// --- P2-5: CacheDir outside DataDir --------------------------------------------------

// serveArtifact: serves an artifact in CacheDir when CacheDir is outside DataDir.
func TestServeArtifactServesFromCacheDirOutsideDataDir(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "artifact-cachedir.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// Separate DataDir and CacheDir — CacheDir is not under DataDir.
	dataDir := t.TempDir()
	cacheDir := t.TempDir()

	assetID := scanOneAsset(t, repo, "cache-outside-clip.mov")
	thumbPath := filepath.Join(cacheDir, assetID, "thumbnail-software.jpg")
	if err := os.MkdirAll(filepath.Dir(thumbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumbPath, []byte("jpeg-data"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "cache-outside-input", 90); err != nil {
		t.Fatal(err)
	}
	leased, err := repo.LeaseNextJob(ctx, "worker-cache-outside", 30*time.Second, domain.LeaseFilter{})
	if err != nil || leased == nil {
		t.Fatalf("lease failed: err=%v job=%+v", err, leased)
	}
	if err := repo.SaveArtifact(ctx, domain.DerivedArtifact{
		ID:        "art-cache-outside-1",
		AssetID:   assetID,
		Type:      "thumbnail",
		LocalPath: thumbPath,
		SizeBytes: 9,
	}, leased.ID, "worker-cache-outside"); err != nil {
		t.Fatal(err)
	}

	service, err := app.NewService(repo, config.Config{
		DataDir:  dataDir,
		CacheDir: cacheDir,
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	req := lanRequest(http.MethodGet, "/api/v1/assets/"+assetID+"/thumbnail", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("artifact in CacheDir outside DataDir: got %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	if resp.Body.Len() == 0 {
		t.Fatal("artifact in CacheDir: body should not be empty")
	}
}

// --- P2-6: collection duplicate name → 409 -------------------------------------------

// saveCollection: duplicate name returns 409 Conflict.
func TestSaveCollectionRejectsDuplicateName(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "collection-dup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{
		DataDir:  t.TempDir(),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	// Create first collection.
	body := `{"name":"my-collection","description":"first"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/collections", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+service.AdminToken())
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("first create: got %d, want 201; body=%s", resp.Code, resp.Body.String())
	}

	// Duplicate name must return 409.
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/collections", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+service.AdminToken())
	req2.Header.Set("Content-Type", "application/json")
	resp2 := httptest.NewRecorder()
	handler.ServeHTTP(resp2, req2)

	if resp2.Code != http.StatusConflict {
		t.Fatalf("duplicate name: got %d, want 409; body=%s", resp2.Code, resp2.Body.String())
	}
	if !strings.Contains(resp2.Body.String(), "already exists") {
		t.Fatalf("duplicate name: body should mention already exists; body=%s", resp2.Body.String())
	}
}
