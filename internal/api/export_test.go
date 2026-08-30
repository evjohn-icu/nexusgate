package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	sqlite "github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// exportFixture builds the smallest library that can produce a timeline: one
// original with known metadata, two shots on it, and a plan whose sections
// select those shots.
type exportFixture struct {
	handler   http.Handler
	service   *app.Service
	planID    string
	assetID   string
	videoPath string
}

func newExportFixture(t *testing.T, name string, fps float64, withMetadata bool) exportFixture {
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
	rootPath := t.TempDir()
	videoPath := filepath.Join(rootPath, "A001C002.mp4")
	if err := os.WriteFile(videoPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, rootPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertScannedFile(ctx, root, "A001C002.mp4", videoPath, info, name+"-fingerprint"); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 1, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	if withMetadata {
		if err := repo.SaveMediaMetadata(ctx, assets[0].ID, domain.MediaMetadata{DurationMS: 60000, Width: 3840, Height: 2160, FPS: fps, HasAudio: true, Reel: "A001"}, "test", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.ReplaceAssetShots(ctx, assets[0].ID, "", []domain.AssetShot{
		{ID: "opening-a", StartMS: 0, EndMS: 4000, Description: "城市夜景开场"},
		{ID: "closing-b", StartMS: 10000, EndMS: 14000, Description: "城市街道人流"},
	}, "", ""); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	create := httptest.NewRecorder()
	handler.ServeHTTP(create, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans", bytes.NewBufferString(`{"brief":"深圳城市宣传片"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var plan domain.RepurposePlan
	if err := json.NewDecoder(create.Body).Decode(&plan); err != nil {
		t.Fatal(err)
	}

	sections := `{"sections":[` +
		`{"role":"opening","query":"city","duration_ms":4000,"required":true,"selected_shot_id":"opening-a","candidates":[{"shot_id":"opening-a","asset_id":"` + assets[0].ID + `","start_ms":0,"end_ms":4000}]},` +
		`{"role":"closing","query":"street","duration_ms":4000,"required":true,"selected_shot_id":"closing-b","candidates":[{"shot_id":"closing-b","asset_id":"` + assets[0].ID + `","start_ms":10000,"end_ms":14000}]}]}`
	revise := httptest.NewRecorder()
	handler.ServeHTTP(revise, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/"+plan.ID+"/revisions", bytes.NewBufferString(sections)))
	if revise.Code != http.StatusCreated {
		t.Fatalf("revise status=%d body=%s", revise.Code, revise.Body.String())
	}

	return exportFixture{handler: handler, service: service, planID: plan.ID, assetID: assets[0].ID, videoPath: videoPath}
}

func (f exportFixture) approve(t *testing.T) {
	t.Helper()
	revisions := httptest.NewRecorder()
	f.handler.ServeHTTP(revisions, hubAdminRequest(f.service, http.MethodGet, "/api/v1/repurpose/plans/"+f.planID+"/revisions", nil))
	var list []domain.RepurposePlanRevision
	if err := json.NewDecoder(revisions.Body).Decode(&list); err != nil || len(list) == 0 {
		t.Fatalf("revisions=%+v err=%v", list, err)
	}
	approve := httptest.NewRecorder()
	f.handler.ServeHTTP(approve, hubAdminRequest(f.service, http.MethodPost, "/api/v1/repurpose/plans/"+f.planID+"/revisions/"+itoa(list[0].Revision)+"/approve", nil))
	if approve.Code != http.StatusOK {
		t.Fatalf("approve status=%d body=%s", approve.Code, approve.Body.String())
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// TestExportRefusesUnapprovedPlan is the point of the whole route pair: an
// export is the artifact somebody cuts with, so a plan an agent drafted but no
// human accepted must not be renderable into one.
func TestExportRefusesUnapprovedPlan(t *testing.T) {
	fixture := newExportFixture(t, "export-draft", 25, true)
	for _, kind := range []string{"edl", "fcpxml"} {
		recorder := httptest.NewRecorder()
		fixture.handler.ServeHTTP(recorder, hubAdminRequest(fixture.service, http.MethodGet, "/api/v1/repurpose/plans/"+fixture.planID+"/export."+kind, nil))
		if recorder.Code != http.StatusConflict {
			t.Fatalf("%s status=%d body=%s", kind, recorder.Code, recorder.Body.String())
		}
	}
}

// TestExportRequiresHubAdmin proves the agent token cannot reach an export even
// though it can read the same plan. The FCPXML names every original's absolute
// path, which is exactly what access_original_media_paths denies.
func TestExportRequiresHubAdmin(t *testing.T) {
	fixture := newExportFixture(t, "export-auth", 25, true)
	fixture.approve(t)
	for _, kind := range []string{"edl", "fcpxml"} {
		agent := httptest.NewRecorder()
		fixture.handler.ServeHTTP(agent, hubAgentRequest(fixture.service, http.MethodGet, "/api/v1/repurpose/plans/"+fixture.planID+"/export."+kind, nil))
		if agent.Code != http.StatusUnauthorized {
			t.Fatalf("%s with agent token status=%d body=%s", kind, agent.Code, agent.Body.String())
		}
		anonymous := httptest.NewRecorder()
		fixture.handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/api/v1/repurpose/plans/"+fixture.planID+"/export."+kind, nil))
		if anonymous.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated status=%d body=%s", kind, anonymous.Code, anonymous.Body.String())
		}
	}
}

func TestExportApprovedPlan(t *testing.T) {
	fixture := newExportFixture(t, "export-ok", 25, true)
	fixture.approve(t)

	edl := httptest.NewRecorder()
	fixture.handler.ServeHTTP(edl, hubAdminRequest(fixture.service, http.MethodGet, "/api/v1/repurpose/plans/"+fixture.planID+"/export.edl", nil))
	if edl.Code != http.StatusOK {
		t.Fatalf("edl status=%d body=%s", edl.Code, edl.Body.String())
	}
	body := edl.Body.String()
	// Two four-second events at 25 fps: source 0-100 then 250-350, record laid
	// end to end at 0-100 then 100-200. Checking the record times catches the
	// class of bug where each event restarts the record timeline at zero.
	for _, want := range []string{"FCM: NON-DROP FRAME", "A001", "00:00:00:00  00:00:04:00  00:00:00:00  00:00:04:00", "00:00:10:00  00:00:14:00  00:00:04:00  00:00:08:00"} {
		if !strings.Contains(body, want) {
			t.Fatalf("edl missing %q:\n%s", want, body)
		}
	}
	if got := edl.Header().Get("Content-Disposition"); !strings.Contains(got, fixture.planID+".edl") {
		t.Fatalf("content-disposition=%q", got)
	}

	fcpxml := httptest.NewRecorder()
	fixture.handler.ServeHTTP(fcpxml, hubAdminRequest(fixture.service, http.MethodGet, "/api/v1/repurpose/plans/"+fixture.planID+"/export.fcpxml", nil))
	if fcpxml.Code != http.StatusOK {
		t.Fatalf("fcpxml status=%d body=%s", fcpxml.Code, fcpxml.Body.String())
	}
	document := fcpxml.Body.String()
	for _, want := range []string{`<fcpxml version="1.9">`, "file://" + fixture.videoPath, `frameDuration="1/25s"`} {
		if !strings.Contains(document, want) {
			t.Fatalf("fcpxml missing %q:\n%s", want, document)
		}
	}
}

// TestExportReportsUnprobedAssetWithoutLeakingItsPath covers the failure an
// operator will actually hit — a selected shot on an asset the pipeline has not
// finished — and pins that the message identifies the file by name only. The
// route is admin-only, but the same text reaches logs, which are not.
func TestExportReportsUnprobedAssetWithoutLeakingItsPath(t *testing.T) {
	fixture := newExportFixture(t, "export-unprobed", 25, false)
	fixture.approve(t)

	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, hubAdminRequest(fixture.service, http.MethodGet, "/api/v1/repurpose/plans/"+fixture.planID+"/export.edl", nil))
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "A001C002.mp4") {
		t.Fatalf("error does not name the file: %s", body)
	}
	if strings.Contains(body, filepath.Dir(fixture.videoPath)) {
		t.Fatalf("error leaked the source directory: %s", body)
	}
}

// TestExportRefusesUnrenderableTimeline drives the real handler through the
// export route and pins that a timeline nleexport refuses arrives as 422. The
// revision below selects a shot range shorter than one frame: at 25 fps a 10ms
// shot is zero frames, which passes every app-layer check — the shot exists,
// the asset is probed, the plan is approved — and is refused only by the
// serializer, so this exercises the nleexport classification specifically,
// not app.ErrPlanNotExportable. The API used to recognize nleexport
// rejections by the message prefix "nleexport: ", so rewording an error
// message would have silently turned a refusal back into a 500; classification
// is structural now, and rewording the message cannot change the status code.
func TestExportRefusesUnrenderableTimeline(t *testing.T) {
	fixture := newExportFixture(t, "export-unrenderable", 25, true)

	sections := `{"sections":[` +
		`{"role":"opening","query":"city","duration_ms":4000,"required":true,"selected_shot_id":"opening-a","candidates":[{"shot_id":"opening-a","asset_id":"` + fixture.assetID + `","start_ms":0,"end_ms":10}]}]}`
	revise := httptest.NewRecorder()
	fixture.handler.ServeHTTP(revise, hubAdminRequest(fixture.service, http.MethodPost, "/api/v1/repurpose/plans/"+fixture.planID+"/revisions", bytes.NewBufferString(sections)))
	if revise.Code != http.StatusCreated {
		t.Fatalf("revise status=%d body=%s", revise.Code, revise.Body.String())
	}
	fixture.approve(t)

	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, hubAdminRequest(fixture.service, http.MethodGet, "/api/v1/repurpose/plans/"+fixture.planID+"/export.edl", nil))
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestExportRefusesMissingPlan pins that exporting a plan id that is not
// registered arrives as 404. GetRepurposePlan returns (nil, nil) for a missing
// plan, so sql.ErrNoRows never surfaces and the API had to recognize the
// refusal by the literal phrase "not found" in the message; rewording that
// message — or an unrelated lower-layer error that happened to contain the
// phrase — would have silently turned the 404 into a 500. Classification is
// structural now, via app.ErrPlanNotFound, so the status no longer depends on
// what the message happens to say.
func TestExportRefusesMissingPlan(t *testing.T) {
	fixture := newExportFixture(t, "export-missing", 25, true)
	for _, kind := range []string{"edl", "fcpxml"} {
		recorder := httptest.NewRecorder()
		fixture.handler.ServeHTTP(recorder, hubAdminRequest(fixture.service, http.MethodGet, "/api/v1/repurpose/plans/"+fixture.planID+"-missing/export."+kind, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d body=%s", kind, recorder.Code, recorder.Body.String())
		}
	}
}

// TestAgentCapabilitiesDenyExport keeps the declared contract and the routing
// table in agreement. skills/timingdex reads this endpoint to decide what it may
// attempt, so an export that is unreachable in code but unlisted here would
// still be attempted on every run.
func TestAgentCapabilitiesDenyExport(t *testing.T) {
	fixture := newExportFixture(t, "export-capabilities", 25, true)
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/agent/capabilities", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var capabilities struct {
		AllowedActions []string `json:"allowed_actions"`
		DeniedActions  []string `json:"denied_actions"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&capabilities); err != nil {
		t.Fatal(err)
	}
	for _, action := range capabilities.AllowedActions {
		if strings.Contains(action, "export") {
			t.Fatalf("export action %q is in the agent allowlist", action)
		}
	}
	found := false
	for _, action := range capabilities.DeniedActions {
		if action == "export_timeline" {
			found = true
		}
	}
	if !found {
		t.Fatalf("export_timeline is not declared denied: %+v", capabilities.DeniedActions)
	}
}
