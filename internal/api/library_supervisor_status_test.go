package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

func newSupervisorHandler(t *testing.T, supervisor config.LibrarySupervisorConfig) http.Handler {
	t.Helper()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "supervisor-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}, LibrarySupervisor: supervisor})
	if err != nil {
		t.Fatal(err)
	}
	return NewServer("", service).Handler()
}

// A background loop nobody can see is indistinguishable from one that has
// quietly died, so the schedule is ordinary status the progress page may poll
// without a token -- the same rule /api/v1/jobs follows.
func TestSupervisorStatusReportsTheScheduleToTheProgressPage(t *testing.T) {
	handler := newSupervisorHandler(t, config.LibrarySupervisorConfig{Enabled: true, ScanIntervalMinutes: 20})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/pipeline/supervisor", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var status struct {
		Enabled         bool `json:"enabled"`
		Running         bool `json:"running"`
		IntervalSeconds int  `json:"interval_seconds"`
		HasError        bool `json:"has_error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || status.IntervalSeconds != 1200 {
		t.Fatalf("status=%+v, want the configured 20 minute interval", status)
	}
	// Configured but not started: nothing in this process called
	// RunLibrarySupervisor, and the page must be able to say so rather than
	// imply a loop that is running.
	if status.Running || status.HasError {
		t.Fatalf("status=%+v, want a configured but unstarted supervisor", status)
	}
}

// The default is off, and the endpoint has to say so plainly: an operator
// wondering why nothing is being indexed should not have to guess.
func TestSupervisorStatusReportsDisabledByDefault(t *testing.T) {
	handler := newSupervisorHandler(t, config.LibrarySupervisorConfig{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/pipeline/supervisor", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if body := response.Body.String(); !strings.Contains(body, `"enabled":false`) || !strings.Contains(body, `"running":false`) {
		t.Fatalf("body=%s", body)
	}
	// The failure text is omitted entirely rather than sent empty, so nothing on
	// the page reads it out of an unauthenticated response by accident.
	if strings.Contains(response.Body.String(), `"last_error"`) {
		t.Fatalf("clean status must not carry a failure field: %s", response.Body.String())
	}
}

// Library reads are network-guarded; this one is no different.
func TestSupervisorStatusIsRefusedFromAnUntrustedNetwork(t *testing.T) {
	handler := newSupervisorHandler(t, config.LibrarySupervisorConfig{Enabled: true})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/supervisor", nil)
	request.RemoteAddr = "203.0.113.7:44321"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

// The progress page is the only place an operator looks; the status has to be
// rendered there, not merely be available over the API.
func TestProgressPageRendersTheSupervisorSchedule(t *testing.T) {
	handler := newSupervisorHandler(t, config.LibrarySupervisorConfig{Enabled: true})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodGet, "/progress", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	for _, marker := range []string{`id="supervisor"`, "/api/v1/pipeline/supervisor", "自动巡检", "held_off_peak", "next_pass_at"} {
		if !strings.Contains(response.Body.String(), marker) {
			t.Fatalf("progress page missing %q", marker)
		}
	}
}
