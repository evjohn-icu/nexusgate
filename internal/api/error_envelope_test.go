package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/app"
	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/media"
	"github.com/evjohn-icu/nexusslate/internal/remote"
	"github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
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

// TestErrorEnvelopeRootPathInvalid pins that a mistyped library-root path is
// the caller's 400, not the server's 500. Both failures — a path that does not
// exist and a path that is a file — previously fell through to the generic
// internal_error, so the operator was told the server broke and the real
// reason went only to the Hub log.
func TestErrorEnvelopeRootPathInvalid(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "envelope-root-path")
	handler := NewServer("", service).Handler()

	file := filepath.Join(t.TempDir(), "not-a-directory.mov")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "no-such-directory")

	for _, tc := range []struct{ name, path, wantIn string }{
		{"nonexistent path", missing, "no-such-directory"},
		{"file instead of directory", file, "not a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			body := `{"path":` + strconv.Quote(tc.path) + `}`
			handler.ServeHTTP(response, lanRequest(http.MethodPost, "/api/v1/roots", strings.NewReader(body)))

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400; body=%s", response.Code, response.Body.String())
			}
			apiErr := decodeErrorEnvelope(t, response)
			if apiErr.Code != "invalid_request" {
				t.Fatalf("code=%q, want invalid_request", apiErr.Code)
			}
			// The message must say what was wrong with the path the caller
			// sent; a 400 whose body explains nothing is only marginally
			// better than the 500 it replaced.
			if !strings.Contains(apiErr.Message, tc.wantIn) {
				t.Fatalf("message=%q, want it to mention %q", apiErr.Message, tc.wantIn)
			}
		})
	}
}

// TestErrorEnvelopeSimilarShotsInvalidAssetFilterDate pins audit U3-04's
// other half: GET /shots/{id}/similar now parses the same asset-context
// filter POST /search/shots' asset_filter carries, and an unparseable date
// must 400 rather than silently compile into a WHERE clause matching
// nothing — listAssetCards' silent-ignore of a bad date is deliberately not
// the pattern here, because a filter the caller asked for and did not get is
// exactly the defect this endpoint was fixed for. The shot id need not
// exist: invalid input is rejected before the repository is ever consulted.
func TestErrorEnvelopeSimilarShotsInvalidAssetFilterDate(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "envelope-similar-shots-date")
	handler := NewServer("", service).Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/shots/does-not-exist/similar?date_from=not-a-date", nil))

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	apiErr := decodeErrorEnvelope(t, response)
	if apiErr.Code != "invalid_request" {
		t.Fatalf("code=%q, want invalid_request", apiErr.Code)
	}
	if !strings.Contains(apiErr.Message, "date_from") {
		t.Fatalf("message=%q, want it to mention date_from", apiErr.Message)
	}
}

// TestErrorEnvelopeSimilarShotsInvalidAssetFilterStatus pins the status half
// of the same filter: an asset-context status outside the six
// domain.ProcessingStatus literals must 400 too, not silently compile into
// processingStatusSQL=? and match nothing.
func TestErrorEnvelopeSimilarShotsInvalidAssetFilterStatus(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "envelope-similar-shots-status")
	handler := NewServer("", service).Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/shots/does-not-exist/similar?status=not-a-status", nil))

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	apiErr := decodeErrorEnvelope(t, response)
	if apiErr.Code != "invalid_request" {
		t.Fatalf("code=%q, want invalid_request", apiErr.Code)
	}
}

// TestErrorEnvelopeWorkerJobStatusNotFound pins that an unknown job id is the
// caller's mistake (404), not a Hub fault (500). GetWorkerJobStatus used to
// return a bare errors.New, which apiErrorFromError could only classify as
// internal_error — telling an operator to go read Hub logs for something the
// Hub answered correctly (audit F7-03).
func TestErrorEnvelopeWorkerJobStatusNotFound(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "envelope-worker-job")
	handler := NewServer("", service).Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/admin/worker-jobs/job-does-not-exist", nil))

	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if apiErr := decodeErrorEnvelope(t, response); apiErr.Code != "not_found" {
		t.Fatalf("code=%q, want not_found", apiErr.Code)
	}
}

// TestErrorEnvelopeWorkerEnrollmentIncomplete pins that a valid pairing token
// with an incomplete registration is a 400 naming what is missing — not a 500,
// and not the 401 "enrollment rejected" that would send an operator off to
// regenerate a pairing token that was fine (audit F7-03).
func TestErrorEnvelopeWorkerEnrollmentIncomplete(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "envelope-worker-enroll")
	handler := NewServer("", service).Handler()

	pairing, err := service.CreateWorkerPairing(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"pairing_token":"` + pairing.Token + `","platform":"linux-amd64"}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodPost, "/api/v1/worker/enroll", strings.NewReader(body)))

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	apiErr := decodeErrorEnvelope(t, response)
	if apiErr.Code != "invalid_request" {
		t.Fatalf("code=%q, want invalid_request", apiErr.Code)
	}
	// The refusal names the missing fields; a caller cannot fix "internal
	// error".
	if !strings.Contains(apiErr.Message, "name") || !strings.Contains(apiErr.Message, "platform") {
		t.Fatalf("message=%q does not name what is missing", apiErr.Message)
	}
}

// TestErrorEnvelopeWorkerEnrollmentBadToken pins the other half of the same
// split: a bad pairing token still answers 401, and now does so through the
// sentinel rather than through strings.Contains on the message (audit F7-02).
func TestErrorEnvelopeWorkerEnrollmentBadToken(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "envelope-worker-enroll-token")
	handler := NewServer("", service).Handler()

	body := `{"pairing_token":"not-a-real-token","name":"w","platform":"linux-amd64"}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodPost, "/api/v1/worker/enroll", strings.NewReader(body)))

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if apiErr := decodeErrorEnvelope(t, response); apiErr.Code != "worker_enrollment_rejected" {
		t.Fatalf("code=%q, want worker_enrollment_rejected", apiErr.Code)
	}
}
