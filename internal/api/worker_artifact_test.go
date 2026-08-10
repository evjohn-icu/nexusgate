package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
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

func TestWorkerArtifactUploadPersistsOnHubAndRejectsNonOwner(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "worker-artifact-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootPath := t.TempDir()
	videoPath := filepath.Join(rootPath, "clip.mp4")
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
	if _, err := repo.UpsertScannedFile(ctx, root, "clip.mp4", videoPath, info, "api-upload-fp"); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	assetID := assets[0].ID
	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "api-upload-input", 90); err != nil {
		t.Fatal(err)
	}

	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	worker, workerToken, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: "api-upload-owner", Platform: "linux-amd64", Capabilities: remote.WorkerCapabilities{Proxy: true, Thumbnail: true, LibraryRoots: []string{root.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	otherPairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, otherToken, err := repo.EnrollWorker(ctx, otherPairing.Token, remote.WorkerRegistration{Name: "api-upload-other", Platform: "linux-amd64", Capabilities: remote.WorkerCapabilities{Proxy: true, LibraryRoots: []string{root.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextWorkerDerive(ctx, worker, time.Minute, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease job=%+v err=%v", job, err)
	}

	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), CacheDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	first := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/v1/worker/jobs/"+job.JobID+"/artifacts/thumbnail?profile=thumb-v1", bytes.NewReader([]byte("jpeg")))
	request.Header.Set("Authorization", "Bearer "+workerToken)
	request.Header.Set("Content-Type", "image/jpeg")
	handler.ServeHTTP(first, request)
	if first.Code != http.StatusCreated {
		t.Fatalf("first upload status=%d body=%s", first.Code, first.Body.String())
	}
	var firstResponse struct {
		Artifact struct {
			AssetID string `json:"asset_id"`
		} `json:"artifact"`
		Reused bool `json:"reused"`
	}
	if err := json.NewDecoder(first.Body).Decode(&firstResponse); err != nil {
		t.Fatal(err)
	}
	if firstResponse.Reused || firstResponse.Artifact.AssetID != assetID {
		t.Fatalf("unexpected first response=%+v", firstResponse)
	}

	duplicate := httptest.NewRecorder()
	duplicateRequest := httptest.NewRequest(http.MethodPut, "/api/v1/worker/jobs/"+job.JobID+"/artifacts/thumbnail?profile=thumb-v1", bytes.NewReader([]byte("different")))
	duplicateRequest.Header.Set("Authorization", "Bearer "+workerToken)
	duplicateRequest.Header.Set("Content-Type", "image/jpeg")
	handler.ServeHTTP(duplicate, duplicateRequest)
	if duplicate.Code != http.StatusAccepted {
		t.Fatalf("duplicate upload status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	if !strings.Contains(duplicate.Body.String(), `"reused":true`) {
		t.Fatalf("duplicate upload was not reported as reused: %s", duplicate.Body.String())
	}
	existingArtifact, err := repo.GetArtifact(ctx, assetID, "thumbnail")
	if err != nil || existingArtifact == nil {
		t.Fatalf("stored thumbnail=%+v err=%v", existingArtifact, err)
	}
	if err := os.Remove(existingArtifact.LocalPath); err != nil {
		t.Fatal(err)
	}
	repair := httptest.NewRecorder()
	repairRequest := httptest.NewRequest(http.MethodPut, "/api/v1/worker/jobs/"+job.JobID+"/artifacts/thumbnail?profile=thumb-v1", bytes.NewReader([]byte("repaired-jpeg")))
	repairRequest.Header.Set("Authorization", "Bearer "+workerToken)
	repairRequest.Header.Set("Content-Type", "image/jpeg")
	handler.ServeHTTP(repair, repairRequest)
	if repair.Code != http.StatusCreated {
		t.Fatalf("repair upload status=%d body=%s", repair.Code, repair.Body.String())
	}

	var multipartBody bytes.Buffer
	multipartWriter := multipart.NewWriter(&multipartBody)
	if err := multipartWriter.WriteField("type", "proxy"); err != nil {
		t.Fatal(err)
	}
	if err := multipartWriter.WriteField("profile_hash", "proxy-v1"); err != nil {
		t.Fatal(err)
	}
	part, err := multipartWriter.CreateFormFile("artifact", "clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("mp4")); err != nil {
		t.Fatal(err)
	}
	if err := multipartWriter.Close(); err != nil {
		t.Fatal(err)
	}
	multipartUpload := httptest.NewRecorder()
	multipartRequest := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+job.JobID+"/artifacts", &multipartBody)
	multipartRequest.Header.Set("Authorization", "Bearer "+workerToken)
	multipartRequest.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	handler.ServeHTTP(multipartUpload, multipartRequest)
	if multipartUpload.Code != http.StatusCreated {
		t.Fatalf("multipart upload status=%d body=%s", multipartUpload.Code, multipartUpload.Body.String())
	}

	unauthorized := httptest.NewRecorder()
	unauthorizedRequest := httptest.NewRequest(http.MethodPut, "/api/v1/worker/jobs/"+job.JobID+"/artifacts/proxy?profile=proxy-v1", bytes.NewReader([]byte("mp4")))
	unauthorizedRequest.Header.Set("Authorization", "Bearer "+otherToken)
	unauthorizedRequest.Header.Set("Content-Type", "video/mp4")
	handler.ServeHTTP(unauthorized, unauthorizedRequest)
	if unauthorized.Code != http.StatusConflict {
		t.Fatalf("non-owner upload status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
}
