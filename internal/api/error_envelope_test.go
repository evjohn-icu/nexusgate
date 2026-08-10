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
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/remote"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// errorEnvelopeBody is the wire shape every API failure answers with: the
// error object is deliberately nested so a future endpoint can carry other
// fields next to it.
type errorEnvelopeBody struct {
	Error APIError `json:"error"`
}

func decodeErrorEnvelope(t *testing.T, response *httptest.ResponseRecorder) APIError {
	t.Helper()
	var envelope errorEnvelopeBody
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("error body is not the JSON envelope: %v body=%s", err, response.Body.String())
	}
	return envelope.Error
}

func newErrorEnvelopeTestService(t *testing.T, name string) *app.Service {
	t.Helper()
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), name+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// TestErrorEnvelopeMissingSearchQuery pins that a validation refusal arrives
// as the structured envelope with a stable code, not as prose a caller would
// have to read.
func TestErrorEnvelopeMissingSearchQuery(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "envelope-search")
	handler := NewServer("", service).Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/search", nil))

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	apiErr := decodeErrorEnvelope(t, response)
	if apiErr.Code != "invalid_request" {
		t.Fatalf("code=%q, want invalid_request", apiErr.Code)
	}
	if apiErr.Message != "missing q" {
		t.Fatalf("message=%q, want %q", apiErr.Message, "missing q")
	}
}

// TestErrorEnvelopeUnknownAPIRoute pins that an unmatched /api/v1/* path
// answers with the JSON 404 envelope rather than the mux's plain-text
// "404 page not found", so a JSON client can tell "empty result" from
// "not found" without sniffing Content-Type.
func TestErrorEnvelopeUnknownAPIRoute(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "envelope-route")
	handler := NewServer("", service).Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil))

	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	apiErr := decodeErrorEnvelope(t, response)
	if apiErr.Code != "not_found" {
		t.Fatalf("code=%q, want not_found", apiErr.Code)
	}
	if apiErr.Message != "not found" {
		t.Fatalf("message=%q, want %q", apiErr.Message, "not found")
	}
}

// TestErrorEnvelopeAdminAuthRequired pins the 401 an administrative route
// answers with when no (or a wrong) Hub admin token is presented: the code
// tells the caller exactly which credential is missing.
func TestErrorEnvelopeAdminAuthRequired(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "envelope-admin-auth")
	handler := NewServer("", service).Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/roots", strings.NewReader(`{"path":"/tmp"}`)))

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	apiErr := decodeErrorEnvelope(t, response)
	if apiErr.Code != "admin_authentication_required" {
		t.Fatalf("code=%q, want admin_authentication_required", apiErr.Code)
	}
	if apiErr.Action != "enter_the_admin_token" {
		t.Fatalf("action=%q, want enter_the_admin_token", apiErr.Action)
	}
}

// TestErrorEnvelopeRequestBodyTooLarge pins the 413 envelope from a
// worker-endpoint body that exceeds its MaxBytesReader limit.
func TestErrorEnvelopeRequestBodyTooLarge(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "envelope-413.db"))
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
	_, workerToken, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{Name: "envelope-worker", Platform: "linux-amd64", Capabilities: remote.WorkerCapabilities{Proxy: true}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	body := strings.Repeat("x", 17<<10)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/worker/lease", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+workerToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	apiErr := decodeErrorEnvelope(t, response)
	if apiErr.Code != "request_body_too_large" {
		t.Fatalf("code=%q, want request_body_too_large", apiErr.Code)
	}
	if apiErr.Message != "request body too large" {
		t.Fatalf("message=%q, want %q", apiErr.Message, "request body too large")
	}
}
