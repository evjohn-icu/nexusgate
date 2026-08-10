package e2e

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/providers"
	videoproviders "github.com/evjohn-icu/timingdex/internal/providers/video"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
	"github.com/evjohn-icu/timingdex/internal/search"
)

// scriptedVision is the in-process stand-in for a paid video-understanding
// provider. It is deliberately not an httptest wire: what these tests need is
// the pipeline's call boundary — the proxy path, the transcript and the
// metadata it hands the model — not the provider's HTTP protocol, which the
// provider packages already cover with fixtures. The scripted answer carries a
// known object ("car") so the search stage has a seeded claim to find.
//
// The shot range is derived from the metadata the pipeline actually passed, so
// the same scripted answer validates for every family whatever its probed
// duration is.
type scriptedVision struct {
	mu     sync.Mutex
	calls  int
	inputs []videoanalysis.Input
}

func (p *scriptedVision) Name() string { return "e2e-scripted-vision" }
func (p *scriptedVision) Model() string {
	return "e2e-scripted-v1"
}
func (p *scriptedVision) Capabilities() []videoproviders.Capability {
	return []videoproviders.Capability{videoproviders.CapabilityVideoAnalysis}
}

func (p *scriptedVision) Analyze(_ context.Context, input videoanalysis.Input) (videoanalysis.Result, string, error) {
	p.mu.Lock()
	p.calls++
	p.inputs = append(p.inputs, input)
	p.mu.Unlock()
	durationMS := input.Metadata.DurationMS
	if durationMS <= 0 {
		durationMS = 3000
	}
	return videoanalysis.Result{
		Summary: "e2e scripted analysis",
		Analysis: domain.StructuredAnalysis{
			Summary:      "e2e scripted analysis",
			AssetType:    "b_roll",
			ShotSize:     "wide",
			CameraMotion: "static",
			AudioType:    "unknown",
			Quality:      "usable",
			SceneTags:    []string{"test_pattern"},
			Subjects:     []string{"test_pattern"},
		},
		Shots: []videoanalysis.Shot{
			{
				StartMS:     0,
				EndMS:       durationMS,
				Description: "a red car on the test pattern",
				Objects:     []string{"car", "test_pattern"},
				Tags:        []string{"test_pattern"},
				Confidence:  0.99,
			},
		},
	}, `{"fixture":"e2e-scripted-vision"}`, nil
}

// scriptedASR stands in for a paid ASR provider so tone-bearing families can
// exercise the transcribe stage without a network. The returned transcript is
// scripted, not derived from the audio — the fixture's audio is a tone, and
// the chain's speech handling is what is under test.
type scriptedASR struct {
	mu    sync.Mutex
	calls int
}

func (a *scriptedASR) Name() string  { return "e2e-scripted-asr" }
func (a *scriptedASR) Model() string { return "e2e-asr-v1" }

func (a *scriptedASR) Transcribe(_ context.Context, _ providers.TranscribeRequest) (domain.Transcript, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	return domain.Transcript{
		Language: "zh",
		Text:     "e2e 测试音",
		Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 3000, Text: "e2e 测试音"}},
	}, nil
}

// chainEnv is one complete Hub under test: real SQLite, the real Service (for
// the root-add/scan boundary) and a pipeline wired with the scripted
// providers, sharing one cache directory and one footage root.
type chainEnv struct {
	repo     *sqlite.Repository
	service  *app.Service
	pipeline *app.Pipeline
	vision   *scriptedVision
	asr      *scriptedASR
	root     domain.LibraryRoot
	cacheDir string
}

// newChainEnv builds the environment. The Service is constructed the way the
// repo's other integration tests do (NewService over real SQLite with a
// software hardware plan); its own pipeline is never run — the test runs a
// separate pipeline wired with the scripted providers, exactly as
// multiframe_analysis_test.go does, because the mock boundary is at the
// provider interface, not at the HTTP layer.
func newChainEnv(t *testing.T) *chainEnv {
	t.Helper()
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: dir, CacheDir: cacheDir, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	footage := filepath.Join(dir, "footage")
	if err := os.MkdirAll(footage, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := service.AddLibraryRoot(ctx, footage)
	if err != nil {
		t.Fatal(err)
	}

	vision := &scriptedVision{}
	asr := &scriptedASR{}
	_, plan := media.DetectHardware(ctx, media.HardwareConfig{Mode: "software", AllowFallback: true})
	pipeline := app.NewPipeline(repo, cacheDir, asr, nil, vision, nil, nil, plan, nil, 0, 0)
	return &chainEnv{repo: repo, service: service, pipeline: pipeline, vision: vision, asr: asr, root: root, cacheDir: cacheDir}
}

// TestE2ECoreChain runs the full chain for every core fixture family, one
// subtest per family. Each subtest owns its own Hub, so a failure in one
// family cannot contaminate another. The subtest name is the fixture name,
// which is also the filename the clip lands at.
//
// The chain asserted here is the whole product path:
//
//	root add -> scan -> probe -> derive (thumbnail+proxy+audio) ->
//	speech gate -> [transcribe] -> analyze (scripted provider) ->
//	shots committed + search index -> v2 search -> preview-range validation
//
// Skips happen only in generateClip and name the missing binary or encoder
// (see fixture.go). A skip anywhere else — or a failed job in the summary —
// is a wiring failure, not an environment skip.
func TestE2ECoreChain(t *testing.T) {
	for _, family := range CoreFixtures {
		family := family
		t.Run(family.Name, func(t *testing.T) {
			runCoreChain(t, family)
		})
	}
}

func runCoreChain(t *testing.T, family Fixture) {
	t.Helper()
	ctx := context.Background()
	env := newChainEnv(t)

	clipPath := generateClip(t, env.root.Path, family.Name+".mp4", family.Opts)
	filename := filepath.Base(clipPath)

	// --- scan ---------------------------------------------------------------
	scan, err := env.service.ScanLibraryRoot(ctx, env.root.ID)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scan.Discovered != 1 {
		t.Fatalf("scan discovered %d assets, want 1: %+v", scan.Discovered, scan)
	}
	if len(scan.Errors) != 0 {
		t.Fatalf("scan errors: %v", scan.Errors)
	}
	assets, err := env.repo.ListAssets(ctx, 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets = %+v, err = %v; want exactly the scanned clip", assets, err)
	}
	assetID := assets[0].ID

	// --- pipeline -----------------------------------------------------------
	executed, err := env.pipeline.RunUntilIdle(ctx)
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if executed == 0 {
		t.Fatal("pipeline ran no jobs; the scan did not enqueue the probe stage")
	}
	summary, err := env.repo.JobSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantJobs := family.expectedJobs()
	if summary.Succeeded != wantJobs || summary.Failed != 0 || summary.Terminal != 0 || summary.Pending != 0 || summary.Running != 0 {
		jobs, _ := env.repo.ListJobs(ctx, 20)
		t.Fatalf("job summary = %+v, want %d succeeded and nothing else; jobs: %+v", summary, wantJobs, jobs)
	}

	// --- probe results ------------------------------------------------------
	meta, err := env.repo.GetMediaMetadata(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil {
		t.Fatal("probe stage committed no media metadata")
	}
	if meta.DurationMS <= 0 || meta.DurationMS > 4000 {
		t.Fatalf("probed duration = %dms, want ~%gs", meta.DurationMS, family.Opts.DurationS)
	}
	if meta.Width != family.Opts.Width || meta.Height != family.Opts.Height {
		t.Fatalf("probed size = %dx%d, want %dx%d", meta.Width, meta.Height, family.Opts.Width, family.Opts.Height)
	}
	if meta.HasAudio != family.hasAudio() {
		t.Fatalf("probed has_audio = %v, want %v", meta.HasAudio, family.hasAudio())
	}
	switch family.Name {
	case "clip-portrait":
		if meta.Orientation != "portrait" {
			t.Errorf("orientation = %q, want portrait", meta.Orientation)
		}
		if meta.VideoCodec != "hevc" && meta.VideoCodec != "h264" {
			t.Errorf("video codec = %q, want hevc (or h264 when libx265 is absent)", meta.VideoCodec)
		}
	case "clip-10bit":
		if meta.PixelFormat != "yuv420p10le" {
			t.Errorf("pixel format = %q, want yuv420p10le", meta.PixelFormat)
		}
		if meta.BitDepth != 10 {
			t.Errorf("bit depth = %d, want 10", meta.BitDepth)
		}
	}

	// --- derive artifacts on disk -------------------------------------------
	for _, typ := range []string{"thumbnail", "proxy"} {
		art, err := env.repo.GetArtifact(ctx, assetID, typ)
		if err != nil {
			t.Fatal(err)
		}
		if art == nil {
			t.Fatalf("derive stage recorded no %s artifact", typ)
		}
		if _, err := os.Stat(art.LocalPath); err != nil {
			t.Fatalf("%s artifact %q missing on disk: %v", typ, art.LocalPath, err)
		}
	}
	if family.hasAudio() {
		audio, err := env.repo.GetArtifact(ctx, assetID, "audio")
		if err != nil {
			t.Fatal(err)
		}
		if audio == nil {
			t.Fatal("audio-bearing fixture produced no audio artifact")
		}
		if _, err := os.Stat(audio.LocalPath); err != nil {
			t.Fatalf("audio artifact %q missing on disk: %v", audio.LocalPath, err)
		}
	}

	// --- speech gate --------------------------------------------------------
	if family.hasAudio() {
		classification, err := env.repo.GetSpeechClassification(ctx, assetID)
		if err != nil {
			t.Fatal(err)
		}
		if classification == nil {
			t.Fatal("audio-bearing fixture ran no speech gate (no classification row)")
		}
		wantClass := "speech_candidate"
		if family.Opts.AudioSource == AudioSilence {
			wantClass = "mostly_silent"
		}
		if classification.Classification != wantClass {
			t.Fatalf("speech gate classified %q (p=%.2f), want %q", classification.Classification, classification.SpeechProbability, wantClass)
		}
	} else {
		classification, err := env.repo.GetSpeechClassification(ctx, assetID)
		if err != nil {
			t.Fatal(err)
		}
		if classification != nil {
			t.Fatalf("no-audio fixture must not run the speech gate, got %+v", classification)
		}
	}

	// --- transcribe ---------------------------------------------------------
	transcript, err := env.repo.GetTranscript(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if family.transcribes() {
		if transcript == nil {
			t.Fatal("transcribe stage committed no transcript for a speech-classified fixture")
		}
		if !strings.Contains(transcript.Text, "e2e") {
			t.Fatalf("transcript text = %q, want the scripted ASR text", transcript.Text)
		}
	} else if transcript != nil {
		t.Fatalf("chain transcribed a fixture that must not transcribe: %+v", transcript)
	}

	// --- analyze + shots + model run ---------------------------------------
	env.vision.mu.Lock()
	visionCalls := env.vision.calls
	lastInput := videoanalysis.Input{}
	if len(env.vision.inputs) > 0 {
		lastInput = env.vision.inputs[len(env.vision.inputs)-1]
	}
	env.vision.mu.Unlock()
	if visionCalls != 1 {
		t.Fatalf("scripted vision provider called %d times, want 1", visionCalls)
	}
	proxy, err := env.repo.GetArtifact(ctx, assetID, "proxy")
	if err != nil {
		t.Fatal(err)
	}
	if lastInput.VideoPath != proxy.LocalPath {
		t.Fatalf("analysis input video = %q, want the derived proxy %q", lastInput.VideoPath, proxy.LocalPath)
	}
	if family.transcribes() && lastInput.Transcript == nil {
		t.Fatal("analysis input carried no transcript although transcribe committed one")
	}
	if !family.transcribes() && lastInput.Transcript != nil {
		t.Fatal("analysis input carried a transcript although the chain must not transcribe")
	}

	shots, err := env.repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) == 0 {
		t.Fatal("analyze stage committed no shots")
	}
	carSeeded := false
	for _, shot := range shots {
		if shot.StartMS < 0 || shot.EndMS <= shot.StartMS || shot.EndMS > meta.DurationMS {
			t.Fatalf("shot range %d-%d outside the clip duration %dms: %+v", shot.StartMS, shot.EndMS, meta.DurationMS, shot)
		}
		for _, object := range shot.Objects {
			if object == "car" {
				carSeeded = true
			}
		}
	}
	if !carSeeded {
		t.Fatalf("no committed shot carries the seeded 'car' object: %+v", shots)
	}
	var runState string
	if err := env.repo.DB().QueryRowContext(ctx, `SELECT state FROM model_runs WHERE asset_id=? AND prompt_version='footage-analysis-v4'`, assetID).Scan(&runState); err != nil {
		t.Fatalf("model run row missing: %v", err)
	}
	if runState != "committed" {
		t.Fatalf("model run state = %q, want committed", runState)
	}

	// --- search: v2 engine + legacy FTS -------------------------------------
	engine := search.NewService(env.repo, search.DefaultOptions())
	resp, err := engine.Search(ctx, search.SearchRequest{Query: "car", Limit: 10, IncludeEvidence: true})
	if err != nil {
		t.Fatalf("v2 search: %v", err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("search returned no shots for the seeded 'car' query; the search index was not built")
	}
	top := resp.Results[0]
	if top.AssetID != assetID {
		t.Fatalf("search top result asset = %q, want %q", top.AssetID, assetID)
	}
	if top.Filename != filename {
		t.Fatalf("search top result filename = %q, want %q", top.Filename, filename)
	}
	// Preview-range validation: the shot's window must be inside the clip's
	// real probed duration.
	if top.StartMS < 0 || top.EndMS <= top.StartMS || top.EndMS > meta.DurationMS {
		t.Fatalf("search result range %d-%d outside clip duration %dms", top.StartMS, top.EndMS, meta.DurationMS)
	}
	carConfirmed := false
	for _, evidence := range top.Evidence {
		if evidence.Value == "car" && evidence.State == search.EvidenceConfirmed {
			carConfirmed = true
		}
	}
	if !carConfirmed {
		t.Fatalf("evidence gate did not confirm the seeded 'car' claim: %+v", top.Evidence)
	}
	hits, err := env.repo.SearchShots(ctx, "car", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("legacy FTS search returned nothing for the seeded 'car' object")
	}

	// --- the chain's job types, exactly ------------------------------------
	types := map[domain.JobType]int{}
	for _, j := range env.jobTypes(t) {
		types[j]++
	}
	want := map[domain.JobType]int{
		domain.JobProbe:      1,
		domain.JobDerive:     1,
		domain.JobSpeechGate: boolToInt(family.hasAudio()),
		domain.JobTranscribe: boolToInt(family.transcribes()),
		domain.JobAnalyze:    1,
		domain.JobIndex:      1,
	}
	// A missing map key reads as 0, so the first pass catches a chain that
	// skipped a required stage and the second catches a chain that ran an
	// extra one; together they are exact set equality without comparing
	// zero-valued entries.
	for typ, count := range want {
		if types[typ] != count {
			t.Fatalf("executed job types = %v, want %v", types, want)
		}
	}
	for typ, count := range types {
		if want[typ] != count {
			t.Fatalf("executed job types = %v, want %v", types, want)
		}
	}
}

// jobTypes lists the types of the jobs this asset's chain ran, in a stable
// order, for the exact-chain assertion.
func (env *chainEnv) jobTypes(t *testing.T) []domain.JobType {
	t.Helper()
	jobs, err := env.repo.ListJobs(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	types := make([]domain.JobType, 0, len(jobs))
	for _, j := range jobs {
		types = append(types, j.Type)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	return types
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
