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

func newMultimodalTestStack(t *testing.T) (http.Handler, *sqlite.Repository) {
	t.Helper()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "multimodal-search.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(context.Background()); err != nil {
		repo.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	cfg := config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}}
	cfg.HubSecurity.AdminAuth = "off"
	service, err := app.NewService(repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer("", service).Handler(), repo
}

func postSearch(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/search/shots", strings.NewReader(body))
	request.RemoteAddr = "10.0.0.5:1234"
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestReadPathNeverEnqueuesInference pins the authorization rule: a trusted
// read request that asks for a modality this Hub cannot serve reports the
// channel as unavailable and creates no work. Repeating a unique query must not
// accumulate jobs either — the read path never spends device time.
func TestReadPathNeverEnqueuesInference(t *testing.T) {
	handler, repo := newMultimodalTestStack(t)
	for _, query := range []string{"unique rain query one", "unique rain query two"} {
		recorder := postSearch(t, handler, `{"query":"`+query+`","modalities":["visual"]}`)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "modality_unavailable") {
			t.Fatalf("expected a typed modality_unavailable code: %s", recorder.Body.String())
		}
	}
	var tasks int
	if err := repo.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM inference_tasks`).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 0 {
		t.Fatalf("read path created %d inference task(s)", tasks)
	}

	// A legacy request is untouched and still answers 200.
	legacy := postSearch(t, handler, `{"query":"rain"}`)
	if legacy.Code != http.StatusOK {
		t.Fatalf("legacy search status=%d body=%s", legacy.Code, legacy.Body.String())
	}
}

// TestSearchRejectsInvalidTemporalBeforeRetrieval pins request validation: an
// unknown relation is a 400, not an empty result set.
func TestSearchRejectsInvalidTemporalBeforeRetrieval(t *testing.T) {
	handler, _ := newMultimodalTestStack(t)
	recorder := postSearch(t, handler, `{"query":"x","temporal":{"conditions":[{"id":"a","modality":"sound_event","query":"rain"},{"id":"b","modality":"sound_event","query":"horn"}],"predicates":[{"id":"p","relation":"near","left_id":"a","right_id":"b"}]}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
