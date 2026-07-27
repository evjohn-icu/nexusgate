package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ev/timingdex/internal/app"
	"github.com/ev/timingdex/internal/config"
	"github.com/ev/timingdex/internal/remote"
	"github.com/ev/timingdex/internal/repository/sqlite"
)

func TestWorkerCredentialDeliveryAllowsLeasedTrustedWorkerWhenExplicitlyEnabled(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "credential-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	seedRemoteAnalyzeJob(t, ctx, repo)
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	worker, token, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: "credential-worker", Platform: "linux-amd64", Capabilities: remote.WorkerCapabilities{ProviderOperations: []string{"video_analysis"}}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, worker.ID, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("lease=%+v err=%v", job, err)
	}

	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), HubSecurity: config.HubSecurityConfig{AllowWorkerProviderCredentials: true}, Providers: config.ProvidersConfig{VisionPrimary: "volcengine_video", VolcVideo: config.ProviderConfig{Enabled: true, BaseURL: "https://vision.example", APIKey: "credential-secret", Model: "vision-v1"}}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+job.ID+"/credentials/video_analysis", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("credential status=%d body=%s", response.Code, response.Body.String())
	}
	var lease struct {
		JobID      string `json:"job_id"`
		WorkerID   string `json:"worker_id"`
		Operation  string `json:"operation"`
		Credential struct {
			APIKey string `json:"api_key"`
		} `json:"credential"`
	}
	if err := json.NewDecoder(response.Body).Decode(&lease); err != nil {
		t.Fatal(err)
	}
	if lease.JobID != job.ID || lease.WorkerID != worker.ID || lease.Operation != "video_analysis" || lease.Credential.APIKey != "credential-secret" {
		t.Fatalf("unexpected credential lease: %#v", lease)
	}

}

func TestWorkerCannotGetCredentialForAnotherWorkersJob(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "credential-ownership.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	seedRemoteAnalyzeJob(t, ctx, repo)
	pairing, _ := repo.CreateWorkerPairing(ctx, time.Minute)
	owner, _, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: "owner", Platform: "linux", Capabilities: remote.WorkerCapabilities{ProviderOperations: []string{"video_analysis"}}})
	if err != nil {
		t.Fatal(err)
	}
	pairing, _ = repo.CreateWorkerPairing(ctx, time.Minute)
	_, otherToken, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: "other", Platform: "linux", Capabilities: remote.WorkerCapabilities{ProviderOperations: []string{"video_analysis"}}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, owner.ID, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("lease=%+v err=%v", job, err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), HubSecurity: config.HubSecurityConfig{AllowWorkerProviderCredentials: true}, Providers: config.ProvidersConfig{VisionPrimary: "volcengine_video", VolcVideo: config.ProviderConfig{Enabled: true, BaseURL: "https://vision.example", APIKey: "secret", Model: "vision-v1"}}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+job.ID+"/credentials/video_analysis", nil)
	req.Header.Set("Authorization", "Bearer "+otherToken)
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, req)
	if response.Code != http.StatusConflict {
		t.Fatalf("other worker status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestWorkerCredentialDeliveryIsDisabledByDefault(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "credential-disabled.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	seedRemoteAnalyzeJob(t, ctx, repo)
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	worker, token, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: "credential-worker", Platform: "linux-amd64", Capabilities: remote.WorkerCapabilities{ProviderOperations: []string{"video_analysis"}}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, worker.ID, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("lease=%+v err=%v", job, err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Providers: config.ProvidersConfig{VisionPrimary: "volcengine_video", VolcVideo: config.ProviderConfig{Enabled: true, BaseURL: "https://vision.example", APIKey: "credential-secret", Model: "vision-v1"}}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/worker/jobs/"+job.ID+"/credentials/video_analysis", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("default credential delivery status=%d body=%s", response.Code, response.Body.String())
	}
}

func seedRemoteAnalyzeJob(t *testing.T, ctx context.Context, repo *sqlite.Repository) {
	t.Helper()
	rootPath := t.TempDir()
	videoPath := filepath.Join(rootPath, "credential.mp4")
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
	if _, err := repo.UpsertScannedFile(ctx, root, "credential.mp4", videoPath, info, "credential-fp"); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 1, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	if err := repo.EnqueueJob(ctx, assets[0].ID, "analyze", "analyze-input", 50); err != nil {
		t.Fatal(err)
	}
}
