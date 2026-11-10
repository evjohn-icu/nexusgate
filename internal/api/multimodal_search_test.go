package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestPendingReferenceSearchReturns202 pins the plan's explicit-channel
// contract: when a query image is already queued but not yet encoded, the read
// path answers 202 with the job ids instead of pretending the channel is
// missing.
func TestPendingReferenceSearchReturns202(t *testing.T) {
	handler, repo := newMultimodalTestStack(t)
	// A 1x1 PNG is enough to create the reference; the encoder never runs.
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	upload := httptest.NewRequest(http.MethodPost, "/api/v1/admin/search-references", bytes.NewReader(encoded.Bytes()))
	upload.RemoteAddr = "10.0.0.5:1234"
	upload.Header.Set("Content-Type", "image/png")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, upload)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var ref struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &ref); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	jobID := "job-pending-ref"
	if _, err := repo.DB().ExecContext(ctx, `
		INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at)
		VALUES(?,NULL,'inference','running',30,1,3,?,?,?,?)`,
		jobID, now, "task-key-pending", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `
		INSERT INTO inference_tasks(job_id,task_key,operation,request_json,query_reference_id,config_hash,model_revision,created_at,updated_at)
		VALUES(?,?, 'visual_embedding','{}',?,'cfg','rev',?,?)`,
		jobID, "task-key-pending", ref.ID, now, now); err != nil {
		t.Fatal(err)
	}

	recorder = postSearch(t, handler, `{"query":"","modalities":["visual"],"reference_id":"`+ref.ID+`"}`)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var pending struct {
		Status string   `json:"status"`
		JobIDs []string `json:"job_ids"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" || len(pending.JobIDs) != 1 || pending.JobIDs[0] != jobID {
		t.Fatalf("unexpected pending body: %+v", pending)
	}
}
