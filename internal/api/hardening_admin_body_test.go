package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

func newHardeningTestService(t *testing.T) *app.Service {
	t.Helper()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "hardening-body-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// Oversize body for MaxBytesReader(1<<20).
func oversizedBody() *strings.Reader {
	return strings.NewReader(strings.Repeat("x", 2<<20))
}

func TestHardeningAdminCreateRootRejectsOversizedBody(t *testing.T) {
	service := newHardeningTestService(t)
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots", oversizedBody()))
	if response.Code == http.StatusOK || response.Code == http.StatusCreated {
		t.Fatalf("expected rejection for oversized body, got status=%d", response.Code)
	}
	if response.Code != http.StatusBadRequest && response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 400 or 413 for oversized body, got status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestHardeningAdminReviewTagProposalRejectsOversizedBody(t *testing.T) {
	service := newHardeningTestService(t)
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/tags/proposals/any-id/review", oversizedBody()))
	if response.Code == http.StatusOK {
		t.Fatalf("expected rejection for oversized body, got status=%d", response.Code)
	}
	if response.Code != http.StatusBadRequest && response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 400 or 413 for oversized body, got status=%d body=%s", response.Code, response.Body.String())
	}
}
