package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
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
	cfg := config.Config{
		DataDir:  secureTestDataDir(t),
		CacheDir: filepath.Join(t.TempDir(), "cache"),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	}
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
	// The query encoder reads the operator-selected profile, so visual must be
	// enabled for the pending-channel path to be reachable.
	settings := domain.DefaultMultimodalSettings()
	settings.Enabled[domain.OpVisualEmbedding] = true
	if err := repo.PutMultimodalSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
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

func TestTimelineCursorValidation(t *testing.T) {
	handler, _ := newMultimodalTestStack(t)
	for _, raw := range []string{
		`?cursor=not-json`,
		`?cursor={"bogus":"0000000000000|0000000000000|x"}`,
		`?cursor={"shots":"bad"}`,
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/assets/missing/timeline"+raw, nil)
		request.RemoteAddr = "10.0.0.5:1234"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("cursor %q status=%d body=%s", raw, recorder.Code, recorder.Body.String())
		}
	}
}

func TestMultimodalAdminReadAndValidationHandlers(t *testing.T) {
	handler, _ := newMultimodalTestStack(t)
	request := func(method, path string, body string) *httptest.ResponseRecorder {
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, reader)
		req.RemoteAddr = "10.0.0.5:1234"
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := request(http.MethodGet, "/api/v1/admin/inference/catalog", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "visual-siglip2-base") {
		t.Fatalf("catalog status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodGet, "/api/v1/admin/search-references/missing", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing reference status=%d", rec.Code)
	}
	if rec := request(http.MethodDelete, "/api/v1/admin/search-references/missing", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing reference status=%d", rec.Code)
	}
	if rec := request(http.MethodGet, "/api/v1/assets/missing/timeline", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing timeline status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodGet, "/api/v1/assets/missing/waveform", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing waveform status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodPost, "/api/v1/admin/assets/missing/modalities/visual_embedding/rebuild", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing rebuild status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodGet, "/api/v1/admin/inference/operations/missing", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing operation status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodPost, "/api/v1/admin/inference/models/visual-siglip2-base/install", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid install status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodGet, "/api/v1/admin/speaker-profiles", ""); rec.Code != http.StatusOK {
		t.Fatalf("speaker profiles status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodGet, "/api/v1/admin/speaker-profiles/missing/matches", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing speaker matches status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSpeakerAdminHandlersValidateInputs(t *testing.T) {
	handler, _ := newMultimodalTestStack(t)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "10.0.0.5:1234"
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	cases := []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPut, "/api/v1/admin/speaker-consent", `{`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/admin/speaker-profiles", `{`, http.StatusBadRequest},
		{http.MethodDelete, "/api/v1/admin/speaker-profiles/missing", "", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/admin/speaker-profiles/missing/matches", "", http.StatusNotFound},
		{http.MethodPatch, "/api/v1/admin/speaker-associations/missing", `{`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/admin/speaker-clusters/missing/split", `{`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/admin/inference/models/visual-siglip2-base/probe", `{}`, http.StatusAccepted},
		{http.MethodPost, "/api/v1/admin/inference/jobs/missing/cancel", "", http.StatusNotFound},
	}
	for _, tc := range cases {
		rec := request(tc.method, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s %s status=%d body=%s, want %d", tc.method, tc.path, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func TestTimelinePaginationRoundTrip(t *testing.T) {
	handler, repo := newMultimodalTestStack(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('timeline-asset','fp',100,'ready','` + now + `','` + now + `')`,
		`INSERT INTO media_metadata(asset_id,ffprobe_json,exiftool_json,normalized_json,probe_version,updated_at,duration_ms) VALUES('timeline-asset','{}','{}','{}','test','` + now + `',10000)`,
		`INSERT INTO asset_shots(id,asset_id,source_run_id,ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at) VALUES('shot-1','timeline-asset',NULL,0,0,5000,'one','[]','[]','[]','[]',0.9,'` + now + `')`,
		`INSERT INTO asset_shots(id,asset_id,source_run_id,ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at) VALUES('shot-2','timeline-asset',NULL,1,5000,10000,'two','[]','[]','[]','[]',0.9,'` + now + `')`,
	} {
		if _, err := repo.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/assets/timeline-asset/timeline?start_ms=0&end_ms=10000&limit=1", nil)
	request.RemoteAddr = "10.0.0.5:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusOK {
		t.Fatalf("timeline status=%d body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Timeline struct {
			Shots []struct {
				ID string `json:"id"`
			} `json:"shots"`
			NextCursors map[string]string `json:"next_cursors"`
		} `json:"timeline"`
		NextCursors map[string]string `json:"next_cursors"`
		Truncated   map[string]bool   `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	cursor := page.NextCursors["shots"]
	if len(page.Timeline.Shots) != 1 || !page.Truncated["shots"] || cursor == "" || page.Timeline.NextCursors["shots"] != cursor {
		t.Fatalf("first page = %+v", page)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/assets/timeline-asset/timeline?start_ms=0&end_ms=10000&limit=1&cursor="+url.QueryEscape(`{"shots":"`+cursor+`"}`), nil)
	request.RemoteAddr = "10.0.0.5:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusOK {
		t.Fatalf("second page status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Timeline.Shots) != 1 || page.Timeline.Shots[0].ID != "shot-2" {
		t.Fatalf("second page shots = %+v", page.Timeline.Shots)
	}
}
