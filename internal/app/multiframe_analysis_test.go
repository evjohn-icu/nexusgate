package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/providers/shotdetect"
	videoproviders "github.com/evjohn-icu/timingdex/internal/providers/video"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// testMultiframeRouter stands in for the providers/video Router: it exposes
// the two member selectors the pipeline's multiframe dispatch relies on.
type testMultiframeRouter struct {
	analyzer videoproviders.MultiframeShotAnalyzer
	video    videoproviders.VideoUnderstandingProvider
}

func (r *testMultiframeRouter) Name() string {
	if r.analyzer != nil {
		return r.analyzer.Name()
	}
	return "test-router"
}
func (r *testMultiframeRouter) Model() string { return "test-model" }
func (r *testMultiframeRouter) Capabilities() []videoproviders.Capability {
	return []videoproviders.Capability{videoproviders.CapabilityVideoAnalysis}
}
func (r *testMultiframeRouter) Analyze(_ context.Context, _ videoanalysis.Input) (videoanalysis.Result, string, error) {
	return videoanalysis.Result{}, "", nil
}
func (r *testMultiframeRouter) MultiframeAnalyzer() videoproviders.MultiframeShotAnalyzer {
	return r.analyzer
}
func (r *testMultiframeRouter) VideoAnalyzer() videoproviders.VideoUnderstandingProvider {
	return r.video
}

// fakeMultiframeAnalyzer records its per-shot requests and answers from a
// per-shot map, so a test can verify which boundaries and transcript slices
// reached the model.
type fakeMultiframeAnalyzer struct {
	summary      videoanalysis.Result
	byShotStart  map[int64]videoproviders.ShotMetadata
	requests     []videoproviders.ShotAnalysisRequest
	failShot     int64 // shot start that fails permanently
	failOn       map[int64]error
	summaryCalls int
}

func (f *fakeMultiframeAnalyzer) Name() string  { return "fake-multiframe" }
func (f *fakeMultiframeAnalyzer) Model() string { return "fake-mf-v1" }
func (f *fakeMultiframeAnalyzer) Capabilities() []videoproviders.Capability {
	return []videoproviders.Capability{videoproviders.CapabilityVideoAnalysis}
}
func (f *fakeMultiframeAnalyzer) Analyze(_ context.Context, input videoanalysis.Input) (videoanalysis.Result, string, error) {
	f.summaryCalls++
	return f.summary, `{"summary_call":true}`, nil
}
func (f *fakeMultiframeAnalyzer) AnalyzeShot(_ context.Context, req videoproviders.ShotAnalysisRequest) (videoproviders.ShotMetadata, string, error) {
	f.requests = append(f.requests, req)
	if f.failShot != 0 && req.ShotStartMS == f.failShot {
		return videoproviders.ShotMetadata{}, "", domain.Permanent(permanentErr("refinement blew up"))
	}
	if err := f.failOn[req.ShotStartMS]; err != nil {
		return videoproviders.ShotMetadata{}, "", err
	}
	meta, ok := f.byShotStart[req.ShotStartMS]
	if !ok {
		meta = videoproviders.ShotMetadata{Description: "default shot"}
	}
	return meta, `{"shot":true}`, nil
}

type permanentErr string

func (e permanentErr) Error() string { return string(e) }

// fakeShotDetector returns fixed boundaries (the caller's fixture) and
// records how often it ran.
type fakeShotDetector struct {
	name   string
	bounds []shotdetect.ShotBound
	err    error
	calls  int
}

func (d *fakeShotDetector) Name() string {
	if d.name == "" {
		return "fake-detector-v1"
	}
	return d.name
}
func (d *fakeShotDetector) Detect(_ context.Context, _ string, _ int64) ([]shotdetect.ShotBound, error) {
	d.calls++
	return d.bounds, d.err
}

// twoShotVideoProvider stands in for a video-capable member: it produces two
// pass-1 shots, which the two-pass flow then refines.
type twoShotVideoProvider struct{}

func (f *twoShotVideoProvider) Name() string  { return "fixture-video" }
func (f *twoShotVideoProvider) Model() string { return "fixture-v" }
func (f *twoShotVideoProvider) Capabilities() []videoproviders.Capability {
	return []videoproviders.Capability{videoproviders.CapabilityVideoAnalysis}
}
func (f *twoShotVideoProvider) Analyze(_ context.Context, _ videoanalysis.Input) (videoanalysis.Result, string, error) {
	return videoanalysis.Result{
		Summary:  "pass one summary",
		Analysis: domain.StructuredAnalysis{Summary: "pass one summary", ShotSize: "medium"},
		Shots: []videoanalysis.Shot{
			{StartMS: 0, EndMS: 1500, Description: "pass1 opening"},
			{StartMS: 1500, EndMS: 2000, Description: "pass1 ending"},
		},
	}, `{"fixture":true}`, nil
}

// seedMultiframeAsset builds an asset whose proxy is a real (tiny) encoded
// clip, because the multiframe path extracts frames from the proxy with
// ffmpeg. Skipped without ffmpeg on PATH, like the other media tests.
func seedMultiframeAsset(t *testing.T, repo *sqlite.Repository, root domain.LibraryRoot, id string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build the multiframe fixture")
	}
	ctx := context.Background()
	proxyPath := filepath.Join(root.Path, id+".mp4")
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=25",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", proxyPath,
	}
	if raw, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("test fixture cannot be encoded by local ffmpeg: %v: %s", err, raw)
	}
	info, err := os.Stat(proxyPath)
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := repo.UpsertScannedFile(ctx, root, id+".mp4", proxyPath, info, "fp-"+id)
	if err != nil {
		t.Fatal(err)
	}
	assetID := scanned.AssetID
	if err := repo.SaveMediaMetadata(ctx, assetID, domain.MediaMetadata{DurationMS: 2000}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "seed-"+id, 50); err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, "seed", nil, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease: job=%+v err=%v", job, err)
	}
	if err := repo.SaveArtifact(ctx, domain.DerivedArtifact{ID: "art-" + id, AssetID: assetID, Type: "proxy", ProfileHash: "p1", LocalPath: proxyPath, SizeBytes: info.Size()}, job.ID, "seed"); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteJob(ctx, job.ID, "seed", domain.JobSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTranscript(ctx, assetID, "qwen", "qwen3-asr-flash", "thash-"+id, domain.Transcript{Language: "zh", Text: "text " + id, Segments: []domain.TranscriptSegment{{StartMS: 500, EndMS: 1500, Text: "text " + id}}}); err != nil {
		t.Fatal(err)
	}
	return assetID
}

func openMultiframeRepo(t *testing.T) (*sqlite.Repository, domain.LibraryRoot, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dir, "multiframe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	footage := filepath.Join(dir, "footage")
	if err := os.MkdirAll(footage, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, footage)
	if err != nil {
		t.Fatal(err)
	}
	return repo, root, dir
}

// Detector mode: the detector owns the boundaries, the summary call owns the
// asset-level analysis, and every shot's metadata comes from the multiframe
// model. The canonical commit must look exactly like a normal analysis — the
// detector boundaries with the model's per-shot truth.
func TestMultiframeDetectorModeEndToEnd(t *testing.T) {
	repo, root, dir := openMultiframeRepo(t)
	ctx := context.Background()
	assetID := seedMultiframeAsset(t, repo, root, "clip-a")

	analyzer := &fakeMultiframeAnalyzer{
		summary: videoanalysis.Result{Summary: "city night", Analysis: domain.StructuredAnalysis{Summary: "city night", ShotSize: "wide"}},
		byShotStart: map[int64]videoproviders.ShotMetadata{
			0:    {Description: "street opening", Tags: []string{"street"}, Objects: []string{"car"}, ShotSize: "close_up", CameraMotion: "pan_left", Quality: "excellent", UsableAs: []string{"establishing"}},
			1000: {Description: "wet road reflection", Tags: []string{"rain"}, Objects: []string{"car"}, ShotSize: "close_up", CameraMotion: "static", Quality: "usable", UsableAs: []string{"ending"}},
		},
	}
	detector := &fakeShotDetector{bounds: []shotdetect.ShotBound{{StartMS: 0, EndMS: 1000}, {StartMS: 1000, EndMS: 2000}}}
	pipeline := NewPipeline(repo, dir, nil, nil, &testMultiframeRouter{analyzer: analyzer}, nil, detector, media.HardwarePlan{}, nil, providerRouteDeferral, 0)
	if err := repo.EnqueueJob(ctx, assetID, domain.JobAnalyze, hashStrings("mf", "detector"), 30); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}

	shots, err := repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 2 {
		t.Fatalf("canonical shots = %d, want 2: %+v", len(shots), shots)
	}
	if shots[0].StartMS != 0 || shots[0].EndMS != 1000 || shots[1].StartMS != 1000 || shots[1].EndMS != 2000 {
		t.Fatalf("boundaries are not the detector's: %+v", shots)
	}
	if shots[0].Description != "street opening" || len(shots[0].Objects) != 1 || len(shots[0].Tags) != 1 {
		t.Fatalf("shot metadata not from the multiframe model: %+v", shots[0])
	}
	runIDs := map[string]bool{}
	for _, s := range shots {
		runIDs[s.SourceRunID] = true
	}
	if len(runIDs) != 1 {
		t.Fatalf("all shots must come from one run, got %+v", shots)
	}

	// Asset-level analysis: summary from the summary call, shot_size folded
	// (dominant close_up), quality folded (worst = usable).
	detail, err := repo.GetAssetDetail(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Analysis == nil || detail.Analysis.Summary != "city night" {
		t.Fatalf("summary not committed: %+v", detail.Analysis)
	}
	if detail.Analysis.ShotSize != "close_up" {
		t.Errorf("shot_size = %q, want folded close_up", detail.Analysis.ShotSize)
	}
	if detail.Analysis.Quality != "usable" {
		t.Errorf("quality = %q, want folded worst (usable)", detail.Analysis.Quality)
	}

	// Provenance: one multiframe run, detector identity in the request JSON.
	var promptVersion, requestJSON string
	if err := repo.DB().QueryRowContext(ctx, `SELECT prompt_version, request_json FROM model_runs WHERE asset_id=?`, assetID).Scan(&promptVersion, &requestJSON); err != nil {
		t.Fatal(err)
	}
	if promptVersion != "multiframe-analysis-v1" {
		t.Errorf("prompt_version = %q, want multiframe-analysis-v1", promptVersion)
	}
	if !strings.Contains(requestJSON, "fake-detector-v1") || !strings.Contains(requestJSON, "boundary-aware") {
		t.Errorf("request_json lacks detector/sampler provenance: %s", requestJSON)
	}

	// The per-shot model saw frames and a transcript slice per shot.
	if len(analyzer.requests) != 2 || analyzer.summaryCalls != 1 {
		t.Fatalf("summary calls = %d, shot calls = %d, want 1 and 2", analyzer.summaryCalls, len(analyzer.requests))
	}
	assertSingleIndexJob(t, repo, assetID, hashStrings("mf", "detector"))
	if len(analyzer.requests[0].Frames) == 0 {
		t.Fatal("shot call carried no frames")
	}
	if !strings.Contains(analyzer.requests[0].Transcript.Text, "text clip-a") {
		t.Fatalf("shot call transcript not sliced to the shot: %q", analyzer.requests[0].Transcript.Text)
	}

	// FTS was rebuilt against the refined shot text.
	hits, err := repo.SearchShots(ctx, "rain", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("search rebuilt against refined shots: %+v", hits)
	}
}

// Two-pass mode: without a detector the video member's window analysis
// provides the boundaries (and the asset-level analysis), and the multiframe
// analyzer refines every shot afterwards. The refinement run must own the
// canonical shots while the video run keeps the analysis.
func TestMultiframeTwoPassModeEndToEnd(t *testing.T) {
	repo, root, dir := openMultiframeRepo(t)
	ctx := context.Background()
	assetID := seedMultiframeAsset(t, repo, root, "clip-b")

	video := &twoShotVideoProvider{}
	analyzer := &fakeMultiframeAnalyzer{
		byShotStart: map[int64]videoproviders.ShotMetadata{
			0:    {Description: "refined opening"},
			1500: {Description: "refined ending"},
		},
	}
	pipeline := NewPipeline(repo, dir, nil, nil, &testMultiframeRouter{analyzer: analyzer, video: video}, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)
	if err := repo.EnqueueJob(ctx, assetID, domain.JobAnalyze, hashStrings("mf", "twopass"), 30); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}

	shots, err := repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 2 {
		t.Fatalf("canonical shots = %d, want the video member's 2: %+v", len(shots), shots)
	}
	if shots[0].Description != "refined opening" || shots[1].Description != "refined ending" {
		t.Fatalf("shots were not refined: %+v", shots)
	}
	// Pass-1 boundaries survived refinement.
	if shots[0].StartMS != 0 || shots[1].StartMS != 1500 {
		t.Fatalf("pass-1 boundaries not preserved: %+v", shots)
	}
	if shots[0].SourceRunID != shots[1].SourceRunID {
		t.Fatal("both shots must share the refinement run")
	}
	if len(analyzer.requests) != 2 {
		t.Fatalf("refinement calls = %d, want 2", len(analyzer.requests))
	}

	// Two runs, distinct provenance: the video member's footage-analysis-v4
	// run and the refinement run.
	var runCount int
	if err := repo.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM model_runs WHERE asset_id=?`, assetID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 2 {
		t.Fatalf("model runs = %d, want 2 (video + refinement)", runCount)
	}
	var refinedRun string
	if err := repo.DB().QueryRowContext(ctx, `SELECT id FROM model_runs WHERE asset_id=? AND prompt_version='multiframe-refinement-v1'`, assetID).Scan(&refinedRun); err != nil {
		t.Fatalf("refinement run missing: %v", err)
	}
	if shots[0].SourceRunID != refinedRun {
		t.Fatalf("canonical shots must point at the refinement run, got %q", shots[0].SourceRunID)
	}
	var refinedState string
	if err := repo.DB().QueryRowContext(ctx, `SELECT state FROM model_runs WHERE id=?`, refinedRun).Scan(&refinedState); err != nil {
		t.Fatal(err)
	}
	if refinedState != "committed" {
		t.Fatalf("refinement run state = %q, want committed (its shots are canonical)", refinedState)
	}
	assertSingleIndexJob(t, repo, assetID, hashStrings("mf", "twopass"))
}

// A multiframe-only chain (no detector, no video-capable member) is a
// deployment error, not a transient one: the job must fail terminally with a
// message that names the remedy.
func TestMultiframeOnlyChainFailsTerminally(t *testing.T) {
	repo, root, dir := openMultiframeRepo(t)
	ctx := context.Background()
	assetID := seedMultiframeAsset(t, repo, root, "clip-c")

	pipeline := NewPipeline(repo, dir, nil, nil, &testMultiframeRouter{analyzer: &fakeMultiframeAnalyzer{}}, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)
	if err := repo.EnqueueJob(ctx, assetID, domain.JobAnalyze, hashStrings("mf", "only"), 30); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}
	summary, err := repo.JobSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Terminal != 1 {
		t.Fatalf("terminal jobs = %d, want 1: %+v", summary.Terminal, summary)
	}
	jobs, err := repo.ListJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var analyzeJob *domain.Job
	for i := range jobs {
		if jobs[i].Type == domain.JobAnalyze {
			analyzeJob = &jobs[i]
		}
	}
	if analyzeJob == nil || !analyzeJob.Terminal || analyzeJob.State != domain.JobFailed {
		t.Fatalf("analyze job not terminal: %+v", jobs)
	}
	if !strings.Contains(analyzeJob.LastError, "shot_detection") || !strings.Contains(analyzeJob.LastError, "vision_fallback") {
		t.Fatalf("error does not name the remedy: %s", analyzeJob.LastError)
	}
}

// A refinement failure is permanent and must leave pass-1's canonical shots
// untouched — degradation, not loss.
func TestMultiframeRefinementFailureKeepsPassOneResults(t *testing.T) {
	repo, root, dir := openMultiframeRepo(t)
	ctx := context.Background()
	assetID := seedMultiframeAsset(t, repo, root, "clip-d")

	video := &twoShotVideoProvider{}
	analyzer := &fakeMultiframeAnalyzer{
		failShot: 1500,
		byShotStart: map[int64]videoproviders.ShotMetadata{
			0: {Description: "refined opening"},
		},
	}
	pipeline := NewPipeline(repo, dir, nil, nil, &testMultiframeRouter{analyzer: analyzer, video: video}, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)
	if err := repo.EnqueueJob(ctx, assetID, domain.JobAnalyze, hashStrings("mf", "fail"), 30); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}

	shots, err := repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	// The video member's pass-1 shots are the last good state.
	if len(shots) != 2 {
		t.Fatalf("canonical shots after failed refinement = %d, want pass-1's 2: %+v", len(shots), shots)
	}
	for _, s := range shots {
		if s.Description == "refined opening" {
			t.Fatalf("refinement leaked into canonical shots despite failure: %+v", shots)
		}
	}
	var state string
	if err := repo.DB().QueryRowContext(ctx, `SELECT state FROM model_runs WHERE asset_id=? AND prompt_version='multiframe-refinement-v1'`, assetID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("refinement run state = %q, want failed", state)
	}
	assertNoIndexJob(t, repo, assetID)
}

func assertSingleIndexJob(t *testing.T, repo *sqlite.Repository, assetID, analyzeHash string) {
	t.Helper()
	jobs, err := repo.ListJobs(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var indexes []domain.Job
	for _, job := range jobs {
		if job.AssetID == assetID && job.Type == domain.JobIndex {
			indexes = append(indexes, job)
		}
	}
	if len(indexes) != 1 {
		t.Fatalf("index jobs = %d, want exactly 1: %+v", len(indexes), indexes)
	}
	want := hashStrings(analyzeHash, "index-v1")
	if indexes[0].InputHash != want {
		t.Fatalf("index hash = %q, want %q", indexes[0].InputHash, want)
	}
}

func assertNoIndexJob(t *testing.T, repo *sqlite.Repository, assetID string) {
	t.Helper()
	jobs, err := repo.ListJobs(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.AssetID == assetID && job.Type == domain.JobIndex {
			t.Fatalf("failed analysis enqueued index job: %+v", job)
		}
	}
}

// The detector is a hard boundary source: garbage boundaries are permanent
// and never reach the model.
func TestMultiframeDetectorGarbageBoundariesFailTerminally(t *testing.T) {
	repo, root, dir := openMultiframeRepo(t)
	ctx := context.Background()
	assetID := seedMultiframeAsset(t, repo, root, "clip-e")

	analyzer := &fakeMultiframeAnalyzer{}
	detector := &fakeShotDetector{bounds: []shotdetect.ShotBound{{StartMS: 500, EndMS: 100}, {StartMS: 0, EndMS: 2000}}}
	pipeline := NewPipeline(repo, dir, nil, nil, &testMultiframeRouter{analyzer: analyzer}, nil, detector, media.HardwarePlan{}, nil, providerRouteDeferral, 0)
	if err := repo.EnqueueJob(ctx, assetID, domain.JobAnalyze, hashStrings("mf", "badbounds"), 30); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if analyzer.summaryCalls != 0 && len(analyzer.requests) != 0 {
		t.Fatalf("model must not be called with invalid boundaries (summary=%d shots=%d)", analyzer.summaryCalls, len(analyzer.requests))
	}
	jobs, err := repo.ListJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var analyzeJob *domain.Job
	for i := range jobs {
		if jobs[i].Type == domain.JobAnalyze {
			analyzeJob = &jobs[i]
		}
	}
	if analyzeJob == nil || !analyzeJob.Terminal || analyzeJob.State != domain.JobFailed {
		t.Fatalf("analyze job not terminal: %+v", jobs)
	}
	if !strings.Contains(analyzeJob.LastError, "invalid shot time range") {
		t.Fatalf("error does not name the invalid boundaries: %s", analyzeJob.LastError)
	}
}

func TestFoldShotMetadata(t *testing.T) {
	a := domain.StructuredAnalysis{ShotSize: "wide", Quality: "excellent"}
	foldShotMetadata(&a, []videoproviders.ShotMetadata{
		{ShotSize: "close_up", CameraMotion: "static", Quality: "usable", UsableAs: []string{"establishing"}},
		{ShotSize: "close_up", CameraMotion: "pan_left", Quality: "not_recommended", UsableAs: []string{"ending"}},
		{ShotSize: "wide", CameraMotion: "pan_left", Quality: "usable"},
	})
	if a.ShotSize != "close_up" {
		t.Errorf("ShotSize = %q, want folded dominant close_up", a.ShotSize)
	}
	if a.CameraMotion != "pan_left" {
		t.Errorf("CameraMotion = %q, want dominant pan_left", a.CameraMotion)
	}
	if a.Quality != "not_recommended" {
		t.Errorf("Quality = %q, want worst (not_recommended)", a.Quality)
	}
	if len(a.UsableAs) != 2 {
		t.Errorf("UsableAs = %v, want the union", a.UsableAs)
	}
}

// An unrecognized quality answer normalizes to unknown; it must never win
// the worst-quality fold — an absent verdict is not a bad one. The same
// abstention rule applies to the dominant shot_size/camera_motion.
func TestFoldShotMetadataUnknownNeverVotes(t *testing.T) {
	a := domain.StructuredAnalysis{}
	foldShotMetadata(&a, []videoproviders.ShotMetadata{
		{ShotSize: "wide", Quality: "not_recommended"},
		{ShotSize: "unknown", CameraMotion: "unknown", Quality: "unknown"},
		{ShotSize: "unknown", CameraMotion: "unknown", Quality: "unknown"},
	})
	if a.Quality != "not_recommended" {
		t.Errorf("Quality = %q, want not_recommended (unknown must not beat it)", a.Quality)
	}
	if a.ShotSize != "wide" {
		t.Errorf("ShotSize = %q, want wide (unknown must not tip the dominant)", a.ShotSize)
	}
	if a.CameraMotion != "" {
		t.Errorf("CameraMotion = %q, want empty when every shot abstains", a.CameraMotion)
	}
}

func TestMultiframeRequestJSONCarriesProvenance(t *testing.T) {
	json := multiframeRequestJSON("asset-1", "/cache/proxy.mp4", "ffmpeg-scene-0.4", "boundary-aware")
	for _, want := range []string{"asset-1", "ffmpeg-scene-0.4", "boundary-aware", "openai_multiframe"} {
		if !strings.Contains(json, want) {
			t.Errorf("request JSON %q lacks %q", json, want)
		}
	}
}

// The summary call must not overflow a small local model with a long-form
// asset's whole transcript: bounded head+tail excerpt, gap marked, and the
// segments dropped so no stale timing survives the truncation.
func TestBoundSummaryTranscript(t *testing.T) {
	short := &domain.Transcript{Text: "short"}
	if got := boundSummaryTranscript(short); got != short {
		t.Fatal("short transcript must pass through untouched")
	}
	long := &domain.Transcript{Text: strings.Repeat("a", 10_000), Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 100, Text: "x"}}}
	bounded := boundSummaryTranscript(long)
	if len([]rune(bounded.Text)) >= 10_000 {
		t.Fatalf("transcript not bounded: %d runes", len([]rune(bounded.Text)))
	}
	if !strings.Contains(bounded.Text, "truncated") {
		t.Errorf("gap marker missing: %.40s…", bounded.Text)
	}
	if !strings.HasPrefix(bounded.Text, "aaaa") || !strings.HasSuffix(bounded.Text, "aaaa") {
		t.Errorf("head/tail not preserved: %.20s…%.20s", bounded.Text[:20], bounded.Text[len(bounded.Text)-20:])
	}
	if bounded.Segments != nil {
		t.Fatal("truncated transcript must drop segments")
	}
	if got := boundSummaryTranscript(nil); got != nil {
		t.Fatal("nil transcript must stay nil")
	}
}

// The detector is part of the analysis identity. The real path: attempt 1
// fails (retryable analyzer error) under detector A; the operator tunes the
// detector; the job retries (same job hash) under detector B. Attempt 2 must
// create a NEW run — the detector-A run stays failed — and commit the
// detector-B boundaries. Without the detector in the run key, attempt 2
// reuses the failed run's id and the committed analysis lies about which
// detector produced it.
func TestMultiframeDetectorChangeRekeysAnalysis(t *testing.T) {
	repo, root, dir := openMultiframeRepo(t)
	ctx := context.Background()
	assetID := seedMultiframeAsset(t, repo, root, "clip-f")

	jobHash := hashStrings("mf", "rekey")
	if err := repo.EnqueueJob(ctx, assetID, domain.JobAnalyze, jobHash, 30); err != nil {
		t.Fatal(err)
	}
	// Attempt 1: detector A, analyzer fails retryably on the first shot.
	failAnalyzer := &fakeMultiframeAnalyzer{
		summary: videoanalysisResult("city night"),
	}
	failAnalyzer.failOn = map[int64]error{0: fmt.Errorf("endpoint warming up")}
	pipelineA := NewPipeline(repo, dir, nil, nil, &testMultiframeRouter{analyzer: failAnalyzer}, nil, &fakeShotDetector{name: "detector-a", bounds: []shotdetect.ShotBound{{StartMS: 0, EndMS: 1000}}}, media.HardwarePlan{}, nil, providerRouteDeferral, 0)
	if _, err := pipelineA.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}
	// Attempt 2: detector B, analyzer succeeds. The retry backoff at attempt
	// 0 is one second.
	time.Sleep(1100 * time.Millisecond)
	okAnalyzer := &fakeMultiframeAnalyzer{
		summary: videoanalysisResult("city night"),
		byShotStart: map[int64]videoproviders.ShotMetadata{
			0: {Description: "shot under B"},
		},
	}
	pipelineB := NewPipeline(repo, dir, nil, nil, &testMultiframeRouter{analyzer: okAnalyzer}, nil, &fakeShotDetector{name: "detector-b", bounds: []shotdetect.ShotBound{{StartMS: 0, EndMS: 1500}}}, media.HardwarePlan{}, nil, providerRouteDeferral, 0)
	if _, err := pipelineB.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}

	var runCount int
	if err := repo.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM model_runs WHERE asset_id=? AND prompt_version='multiframe-analysis-v1'`, assetID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 2 {
		t.Fatalf("model runs = %d, want 2 (detector A failed + detector B committed); detector identity is not in the dedup key", runCount)
	}
	shots, err := repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 1 || shots[0].StartMS != 0 || shots[0].EndMS != 1500 {
		t.Fatalf("committed shots are not detector B's boundaries: %+v", shots)
	}
}

// A transient detector failure must leave the run marked failed, not dangling
// in running forever.
func TestMultiframeDetectorFailureFailsTheRun(t *testing.T) {
	repo, root, dir := openMultiframeRepo(t)
	ctx := context.Background()
	assetID := seedMultiframeAsset(t, repo, root, "clip-g")

	analyzer := &fakeMultiframeAnalyzer{}
	detector := &fakeShotDetector{err: fmt.Errorf("detector exploded")}
	pipeline := NewPipeline(repo, dir, nil, nil, &testMultiframeRouter{analyzer: analyzer}, nil, detector, media.HardwarePlan{}, nil, providerRouteDeferral, 0)
	if err := repo.EnqueueJob(ctx, assetID, domain.JobAnalyze, hashStrings("mf", "detfail"), 30); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := repo.DB().QueryRowContext(ctx, `SELECT state FROM model_runs WHERE asset_id=? AND prompt_version='multiframe-analysis-v1'`, assetID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("detector failure left run in state %q, want failed", state)
	}
}

func videoanalysisResult(summary string) videoanalysis.Result {
	return videoanalysis.Result{Summary: summary, Analysis: domain.StructuredAnalysis{Summary: summary}}
}
