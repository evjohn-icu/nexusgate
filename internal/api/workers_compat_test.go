package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// workerItem mirrors the handler's response shape: every worker field plus
// the Hub-computed compat verdict.
type workerItem struct {
	remote.Worker
	Compat domain.WorkerCompat `json:"compat"`
}

// TestWorkersListCarriesCompatVerdict pins the P1b API contract: each item
// of GET /api/v1/hub/workers carries the Hub's compatibility verdict beside
// the stored version, so the workers page never re-derives semver logic in
// JavaScript.
func TestWorkersListCarriesCompatVerdict(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "workers-compat.db"))
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
	enroll := func(name, version string) {
		t.Helper()
		pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: name, Platform: "linux", Version: version}); err != nil {
			t.Fatal(err)
		}
	}
	enroll("old-worker", "v0.24.0")
	enroll("current-worker", "v0.26.0")
	enroll("dev-worker", "dev")

	request := httptest.NewRequest(http.MethodGet, "/api/v1/hub/workers", nil)
	request.Header.Set("Authorization", "Bearer "+service.AdminToken())
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var items []workerItem
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode workers list: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("workers list length=%d want 3", len(items))
	}
	byName := map[string]workerItem{}
	for _, item := range items {
		byName[item.Name] = item
	}
	assertVerdict := func(name, verdict string, messageExpected bool) {
		t.Helper()
		item, ok := byName[name]
		if !ok {
			t.Fatalf("worker %q missing from list: %v", name, items)
		}
		if item.Compat.Verdict != verdict {
			t.Fatalf("worker %q (version %q) verdict=%q want %q", name, item.Version, item.Compat.Verdict, verdict)
		}
		if item.Compat.MinVersion != app.MinWorkerVersion {
			t.Fatalf("worker %q min_version=%q want %q", name, item.Compat.MinVersion, app.MinWorkerVersion)
		}
		if item.Compat.WorkerVersion != item.Version {
			t.Fatalf("worker %q compat.worker_version=%q want %q", name, item.Compat.WorkerVersion, item.Version)
		}
		hasMessage := item.Compat.Message != ""
		if hasMessage != messageExpected {
			t.Fatalf("worker %q message=%q want message=%v", name, item.Compat.Message, messageExpected)
		}
	}
	assertVerdict("old-worker", domain.WorkerVerdictIncompatible, true)
	assertVerdict("current-worker", domain.WorkerVerdictCompatible, false)
	assertVerdict("dev-worker", domain.WorkerVerdictUpgradeRecommended, false)
}

// TestWorkersPageRendersCompatSurface guards the page copy: the version
// line, the three verdict pills and the 不派发任务 note are all part of the
// served page, so a stray edit that silently breaks the node renderer (the
// page has no template, just one JS string) is caught here.
func TestWorkersPageRendersCompatSurface(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "workers-page-compat.db"))
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
	for _, marker := range []string{"版本：", "兼容", "建议升级", "不兼容", "不派发任务"} {
		if !strings.Contains(response.Body.String(), marker) {
			t.Fatalf("workers page missing %q", marker)
		}
	}
}
