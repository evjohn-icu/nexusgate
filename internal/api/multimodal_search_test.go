package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/remote"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
	"github.com/evjohn-icu/nexusgate/internal/worker"
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

func TestWorkerEncodesReferenceAfterLeasedModelInstall(t *testing.T) {
	handler, repo := newMultimodalTestStack(t)
	// The query encoder reads the operator-selected profile, so visual must be
	// enabled for the pending-channel path to be reachable.
	settings := domain.DefaultMultimodalSettings()
	settings.Enabled[domain.OpVisualEmbedding] = true
	if err := repo.PutMultimodalSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	// Upload triggers encoding on a Hub whose only executor is the Worker.
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
	jobID, queued, err := repo.QueryReferenceJob(ctx, ref.ID)
	if err != nil || !queued {
		t.Fatalf("reference job=%q queued=%t err=%v", jobID, queued, err)
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

	hub := httptest.NewTLSServer(handler)
	defer hub.Close()
	request := func(path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := lanRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		if response.Code != want {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		return response
	}
	pairingResponse := request("/api/v1/hub/worker-pairings", `{"grants":{"inference_operations":["visual_embedding","visual_embedding"],"model_management_operations":["install"]}}`, http.StatusCreated)
	var pairing remote.PairingToken
	if err := json.Unmarshal(pairingResponse.Body.Bytes(), &pairing); err != nil {
		t.Fatal(err)
	}
	client := worker.NewClient(hub.URL, fmt.Sprintf("%x", sha256.Sum256(hub.Certificate().Raw)))
	registration := remote.WorkerRegistration{Name: "visual-worker", Platform: "linux"}
	enrollment, err := client.Enroll(ctx, pairing.Token, registration)
	if err != nil {
		t.Fatal(err)
	}
	registration.Capabilities = enrollment.Worker.Capabilities
	if len(registration.Capabilities.InferenceOperations) != 1 {
		t.Fatalf("enrollment grants=%v", registration.Capabilities.InferenceOperations)
	}
	model, ok := inferencecatalog.DefaultFor(domain.OpVisualEmbedding)
	if !ok {
		t.Fatal("visual model missing from catalog")
	}
	executor := &referenceWorkerFixture{model: model}
	runtime := worker.NewRuntime(client, worker.Config{Token: enrollment.Token, CacheDir: t.TempDir(), Registration: registration}, nil, registration.Capabilities).
		WithInferenceExecutor(executor).WithModelManager(executor)
	if worked, err := runtime.RunOnce(ctx); err != nil || worked {
		t.Fatalf("unattested executor worked=%t err=%v", worked, err)
	}
	request("/api/v1/admin/inference/models/"+model.CatalogID+"/install", `{"worker_id":"`+enrollment.Worker.ID+`","license_accepted":true}`, http.StatusAccepted)
	for _, operation := range []string{"install", "inference"} {
		if worked, err := runtime.RunOnce(ctx); err != nil || !worked {
			t.Fatalf("%s worked=%t err=%v", operation, worked, err)
		}
	}
	if _, err := os.Stat(executor.imagePath); !os.IsNotExist(err) {
		t.Fatalf("staged Worker image was retained: %v", err)
	}
	if _, err := client.FetchInferenceInput(ctx, enrollment.Token, jobID); err == nil {
		t.Fatal("completed lease still grants image access")
	}
	recorder = postSearch(t, handler, `{"query":"","modalities":["visual"],"reference_id":"`+ref.ID+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("ready status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

type referenceWorkerFixture struct {
	model     inferencecatalog.Model
	installed bool
	imagePath string
}

func (f *referenceWorkerFixture) Install(_ context.Context, task domain.ModelManagementTask) error {
	if task.CatalogID != f.model.CatalogID || task.ManifestDigest != f.model.ManifestDigest() || !task.LicenseAccepted {
		return fmt.Errorf("unexpected model installation task")
	}
	f.installed = true
	return nil
}

func (f *referenceWorkerFixture) Probe(context.Context, domain.ModelManagementTask) error {
	return fmt.Errorf("unexpected model probe")
}

func (f *referenceWorkerFixture) Capabilities(context.Context) ([]remote.InferenceCapability, error) {
	if !f.installed {
		return nil, nil
	}
	return []remote.InferenceCapability{{
		Operation: domain.OpVisualEmbedding, SchemaVersion: domain.InferenceSchemaVersion,
		ModelID: f.model.ModelID, ModelRevision: f.model.Revision, Backend: "cpu", Available: true,
	}}, nil
}

func (f *referenceWorkerFixture) Execute(ctx context.Context, request domain.InferenceRequest) (domain.InferenceResult, error) {
	if request.Input.Kind != domain.InputKindImage || request.Input.RelativePath != "" || request.Input.RootID != "" {
		return domain.InferenceResult{}, fmt.Errorf("unexpected image input locator")
	}
	path, err := worker.ResolveInferenceImage(ctx, request.Input.ReferenceID, request.Input.Digest)
	if err != nil {
		return domain.InferenceResult{}, err
	}
	f.imagePath = path
	file, err := os.Open(path)
	if err != nil {
		return domain.InferenceResult{}, err
	}
	defer file.Close()
	if _, err := jpeg.DecodeConfig(file); err != nil {
		return domain.InferenceResult{}, fmt.Errorf("Worker image was not normalized: %w", err)
	}
	vector := make([]float32, f.model.Dimension)
	vector[0] = 1
	return domain.InferenceResult{
		SchemaVersion: request.SchemaVersion, OperationID: request.OperationID, Operation: request.Operation,
		ModelID: request.ModelID, ModelRevision: request.ModelRevision, SpaceID: request.SpaceID,
		InputHash: request.InputHash, ConfigHash: request.ConfigHash, Backend: "cpu",
		Embeddings: []domain.EmbeddingRecord{{Vector: vector, SampleIndex: -1}},
	}, nil
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
