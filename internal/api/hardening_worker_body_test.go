package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/app"
	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/media"
	"github.com/evjohn-icu/nexusslate/internal/remote"
	"github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
)

func TestEnrollWorkerRejectsOversizedBody(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "enroll-maxbytes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	// The enrollment payload limit is 32 KiB; send a body that exceeds it.
	body := `{"pairing_token":"` + strings.Repeat("x", 33<<10) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/worker/enroll", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("enroll oversized body: expected 413, got %d body=%s", response.Code, response.Body.String())
	}
}

func TestEnrollWorkerAcceptsNormalSizedBody(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "enroll-normal-maxbytes.db"))
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
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	// A valid, small enrollment payload must be accepted.
	body := `{"pairing_token":"` + pairing.Token + `","name":"ok-worker","platform":"linux-amd64"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/worker/enroll", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("enroll normal body: expected 201, got %d body=%s", response.Code, response.Body.String())
	}
}

func TestWorkerHeartbeatRejectsOversizedBody(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "heartbeat-maxbytes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Enroll a worker so we have a valid token.
	pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, workerToken, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: "hb-worker", Platform: "linux-amd64", Capabilities: remote.WorkerCapabilities{Proxy: true}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	// The heartbeat payload limit is 16 KiB; send a body that exceeds it.
	body := `{"capabilities":{"` + strings.Repeat("x", 17<<10) + `":"y"}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/worker/heartbeat", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+workerToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("heartbeat oversized body: expected 413, got %d body=%s", response.Code, response.Body.String())
	}
}

func TestWorkerLeaseRejectsOversizedBody(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "lease-maxbytes.db"))
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
	_, workerToken, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: "lease-worker", Platform: "linux-amd64", Capabilities: remote.WorkerCapabilities{Proxy: true}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	// The lease endpoint does not use the body, but the limit of 16 KiB is
	// enforced via io.Copy(io.Discard, ...). Send a body that exceeds it.
	body := strings.Repeat("x", 17<<10)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/worker/lease", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+workerToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("lease oversized body: expected 413, got %d body=%s", response.Code, response.Body.String())
	}
}
