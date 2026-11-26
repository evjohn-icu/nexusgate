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

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/remote"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
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

	artifactDir := filepath.Join(service.DataDir(), "derived", "worker", assetID)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(artifactDir, "thumbnail-thumb-v1.jpg")
	if err := os.WriteFile(orphan, []byte("jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeAdoption, err := os.Stat(orphan)
	if err != nil {
		t.Fatal(err)
	}

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
	if adopted, err := os.Stat(existingArtifact.LocalPath); err != nil || !os.SameFile(beforeAdoption, adopted) {
		t.Fatalf("identical thumbnail orphan was replaced: %v", err)
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

	makeOrphan := filepath.Join(artifactDir, "proxy-proxy-v1.mp4")
	if err := os.WriteFile(makeOrphan, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
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
	content, err := os.ReadFile(makeOrphan)
	if err != nil || string(content) != "mp4" {
		t.Fatalf("proxy orphan contains %q after replacement, err=%v", content, err)
	}
	var count int
	if err := repo.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM derived_artifacts WHERE asset_id=?`, assetID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("artifact rows=%d err=%v", count, err)
	}

	// A derive that ran locally commits its row under the Hub cache. A later
	// Worker attempt for the same artifact key must reuse that row instead of
	// rejecting it as outside Worker-managed storage.
	localDir := filepath.Join(service.CacheDir(), assetID)
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		t.Fatal(err)
	}
	localAudio := filepath.Join(localDir, "audio-16k-v1.wav")
	if err := os.WriteFile(localAudio, []byte("local-audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) VALUES(?,?,?,?,?,?,?)`,
		"hub-local-audio", assetID, "audio", "audio-16k-v1", localAudio, len("local-audio"), time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")); err != nil {
		t.Fatal(err)
	}
	localReuse := httptest.NewRecorder()
	localRequest := httptest.NewRequest(http.MethodPut, "/api/v1/worker/jobs/"+job.JobID+"/artifacts/audio?profile=audio-16k-v1", bytes.NewReader([]byte("worker-audio")))
	localRequest.Header.Set("Authorization", "Bearer "+workerToken)
	localRequest.Header.Set("Content-Type", "audio/wav")
	handler.ServeHTTP(localReuse, localRequest)
	if localReuse.Code != http.StatusAccepted || !strings.Contains(localReuse.Body.String(), `"reused":true`) {
		t.Fatalf("committed local artifact was not reused: status=%d body=%s", localReuse.Code, localReuse.Body.String())
	}
	committed, err := repo.GetArtifact(ctx, assetID, "audio")
	if err != nil || committed == nil || committed.LocalPath != localAudio {
		t.Fatalf("committed local artifact=%+v err=%v, want path %s", committed, err, localAudio)
	}
	if content, err := os.ReadFile(localAudio); err != nil || string(content) != "local-audio" {
		t.Fatalf("committed local artifact was rewritten: %q err=%v", content, err)
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
