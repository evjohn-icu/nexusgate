package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	videoanalysis "github.com/evjohn-icu/nexusgate/internal/domain/video_analysis"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/providers/common"
	"github.com/evjohn-icu/nexusgate/internal/providers/openaivideo"
	videoproviders "github.com/evjohn-icu/nexusgate/internal/providers/video"
)

// recordingVideoProvider answers every call with a shot at a fixed offset
// inside whatever clip it was given, which is how a real model behaves: it
// reports times relative to the video in front of it and knows nothing about
// where that video sits in the asset.
type recordingVideoProvider struct {
	inlineLimit int64
	calls       []videoanalysis.Input
	sizes       []int64
	failAt      int
}

func (f *recordingVideoProvider) Name() string  { return "fake_video" }
func (f *recordingVideoProvider) Model() string { return "fake-model" }
func (f *recordingVideoProvider) Capabilities() []videoproviders.Capability {
	return []videoproviders.Capability{videoproviders.CapabilityVideoAnalysis}
}
func (f *recordingVideoProvider) MaxInlineVideoBytes() int64 { return f.inlineLimit }

func (f *recordingVideoProvider) Analyze(_ context.Context, input videoanalysis.Input) (videoanalysis.Result, string, error) {
	f.calls = append(f.calls, input)
	var size int64
	if info, err := os.Stat(input.VideoPath); err == nil {
		size = info.Size()
	}
	f.sizes = append(f.sizes, size)
	if f.failAt > 0 && len(f.calls) == f.failAt {
		return videoanalysis.Result{}, `{"error":"boom"}`, os.ErrDeadlineExceeded
	}
	return videoanalysis.Result{
		Summary: "window summary",
		Shots:   []videoanalysis.Shot{{StartMS: 1_000, EndMS: 3_000, Description: "a shot"}},
	}, `{"ok":true}`, nil
}

func newTestPipelineWithVideo(provider videoproviders.VideoUnderstandingProvider) *Pipeline {
	return &Pipeline{videoProvider: provider}
}

// A proxy longer than the provider's request has to be cut, and each piece has
// to arrive inside the declared budget — otherwise the endpoint answers 413 and
// the analysis is lost for good, since an oversized body stays oversized on
// retry.
func TestAnalyzeVideoSplitsAnOversizedProxyIntoWindowsThatFit(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build and cut the fixture")
	}
	proxy, durationMS := buildFixtureProxy(t, 60)
	info, err := os.Stat(proxy)
	if err != nil {
		t.Fatal(err)
	}
	// A budget of a third of the file forces several windows.
	provider := &recordingVideoProvider{inlineLimit: info.Size() / 3}
	pipeline := newTestPipelineWithVideo(provider)

	result, raw, err := pipeline.analyzeVideo(context.Background(), provider, "asset-1",
		videoanalysis.Input{VideoPath: proxy}, durationMS)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(provider.calls) < 2 {
		t.Fatalf("expected the proxy to be split, got %d call(s)", len(provider.calls))
	}
	for i, size := range provider.sizes {
		if size == 0 {
			t.Fatalf("window %d was not a real file", i)
		}
		if size > info.Size() {
			t.Fatalf("window %d (%d bytes) is larger than the whole proxy", i, size)
		}
	}
	// Every window reports its shot at 1s; only shifting puts them at distinct
	// points on the asset timeline.
	if len(result.Shots) < 2 {
		t.Fatalf("expected one shot per window on the asset timeline, got %+v", result.Shots)
	}
	seen := map[int64]bool{}
	for _, shot := range result.Shots {
		if seen[shot.StartMS] {
			t.Fatalf("two windows produced the same start time %d — shots were not shifted: %+v", shot.StartMS, result.Shots)
		}
		seen[shot.StartMS] = true
		if shot.EndMS > durationMS {
			t.Fatalf("shot runs past the asset duration: %+v", shot)
		}
	}
	// The run's raw record must hold every window's reply, not just the last.
	var replies []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &replies); err != nil {
		t.Fatalf("raw responses are not a JSON array: %v (%s)", err, raw)
	}
	if len(replies) != len(provider.calls) {
		t.Fatalf("raw holds %d replies for %d calls", len(replies), len(provider.calls))
	}
}

// The common case must stay a single request: splitting costs an extra ffmpeg
// pass per window and multiplies the provider spend.
func TestAnalyzeVideoSendsAFittingProxyWhole(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build the fixture")
	}
	proxy, durationMS := buildFixtureProxy(t, 5)
	provider := &recordingVideoProvider{inlineLimit: 64 << 20}
	pipeline := newTestPipelineWithVideo(provider)

	if _, _, err := pipeline.analyzeVideo(context.Background(), provider, "asset-1",
		videoanalysis.Input{VideoPath: proxy}, durationMS); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(provider.calls) != 1 {
		t.Fatalf("a proxy inside the budget must be sent in one call, got %d", len(provider.calls))
	}
	if provider.calls[0].VideoPath != proxy {
		t.Fatalf("the original proxy should have been sent, got %q", provider.calls[0].VideoPath)
	}
}

// A partial analysis committed as if it described the whole asset would be
// worse than none, so one failed window fails the job.
func TestAnalyzeVideoFailsTheAssetWhenAWindowFails(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build and cut the fixture")
	}
	proxy, durationMS := buildFixtureProxy(t, 60)
	info, err := os.Stat(proxy)
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingVideoProvider{inlineLimit: info.Size() / 3, failAt: 2}
	pipeline := newTestPipelineWithVideo(provider)

	_, raw, err := pipeline.analyzeVideo(context.Background(), provider, "asset-1",
		videoanalysis.Input{VideoPath: proxy}, durationMS)
	if err == nil {
		t.Fatal("a failed window must fail the asset's analysis")
	}
	if raw == "" {
		t.Fatal("the raw responses collected before the failure must be kept for the failed run")
	}
}

// Scratch windows are recreatable from the proxy; leaving them behind would
// grow the cache directory by the size of the library.
func TestAnalyzeVideoRemovesItsScratchWindows(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build and cut the fixture")
	}
	proxy, durationMS := buildFixtureProxy(t, 60)
	info, err := os.Stat(proxy)
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingVideoProvider{inlineLimit: info.Size() / 3}
	pipeline := newTestPipelineWithVideo(provider)

	if _, _, err := pipeline.analyzeVideo(context.Background(), provider, "asset-1",
		videoanalysis.Input{VideoPath: proxy}, durationMS); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(proxy), "analysis-windows")); !os.IsNotExist(err) {
		t.Fatalf("scratch window directory was left behind: %v", err)
	}
}

// countingSizeThresholdServer is a fixture endpoint that answers 413 above a
// byte threshold, measured against the request body actually written to the
// wire, and 200 with a valid unified-analysis reply below it. It is the
// shape the task calls for: a real HTTP server, not a mock of one, so the
// base64 expansion inputVideoURL performs is exercised for real rather than
// asserted about.
func countingSizeThresholdServer(t *testing.T, threshold int64) (*httptest.Server, func() (total, tooLarge int)) {
	t.Helper()
	var mu sync.Mutex
	var total, tooLarge int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		total++
		over := int64(len(body)) > threshold
		if over {
			tooLarge++
		}
		mu.Unlock()
		if over {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte("request too large"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"ok\",\"shots\":[{\"start_ms\":1000,\"end_ms\":2000,\"description\":\"a shot\"}]}"}}]}`))
	}))
	t.Cleanup(server.Close)
	return server, func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return total, tooLarge
	}
}

// buildHighBitrateFixtureProxy forces true CBR (nal-hrd=cbr) rather than
// trusting libx264's default rate control on a near-static synthetic
// pattern, which compresses far below any requested target bitrate. A
// predictable byte size is what lets this file's arithmetic tests choose a
// ceiling that is neither trivially satisfied nor dominated by
// media.MinAnalysisWindowMS's own floor.
func buildHighBitrateFixtureProxy(t *testing.T, seconds, bitrateKbps int) (string, int64) {
	t.Helper()
	dir := t.TempDir()
	proxy := filepath.Join(dir, "proxy.mp4")
	rate := strconv.Itoa(bitrateKbps) + "k"
	if output, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25",
		"-t", strconv.Itoa(seconds), "-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "25",
		"-b:v", rate, "-minrate", rate, "-maxrate", rate, "-bufsize", rate,
		"-x264-params", "nal-hrd=cbr:force-cfr=1", proxy).CombinedOutput(); err != nil {
		t.Skipf("test fixture cannot be encoded by local ffmpeg: %v: %s", err, output)
	}
	return proxy, int64(seconds) * 1000
}

// This is the arithmetic the bug report asked to be established: a window
// planned to fit a declared byte budget must not exceed that budget once
// base64-encoded on the wire. The endpoint here enforces its ceiling against
// the real request body length (not a mock's opinion of it), so this fails
// if MaxInlineVideoBytes ever again hands the splitter a raw-byte budget
// equal to (rather than 3/4 of) the endpoint's declared ceiling.
func TestAnalyzeVideoWindowsFitTheDeclaredCeilingOnceBase64Encoded(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build and cut the fixture")
	}
	// ~4Mbps CBR for 60s is ~30MB: large enough that the fixed prompt/JSON
	// overhead reserve is a small fraction of the budget, so the planned
	// window size is driven by the byte budget, not clamped up by
	// media.MinAnalysisWindowMS (which would confound this test with that
	// separate floor).
	proxy, durationMS := buildHighBitrateFixtureProxy(t, 60, 4000)
	info, err := os.Stat(proxy)
	if err != nil {
		t.Fatal(err)
	}
	// The endpoint's real, observed ceiling — exactly what "the channel
	// declared no explicit limit" resolves to once a limit is declared.
	// Comfortably under the proxy's own size, so several windows are forced.
	ceiling := int64(25 << 20)
	if ceiling >= info.Size() {
		t.Fatalf("fixture (%d bytes) is not larger than the test ceiling (%d) — adjust the bitrate", info.Size(), ceiling)
	}
	server, counts := countingSizeThresholdServer(t, ceiling)

	provider := &openaivideo.Provider{ProviderName: "wire_ceiling_test", Endpoint: common.Endpoint{BaseURL: server.URL}, MaxInlineBytes: ceiling}
	pipeline := newTestPipelineWithVideo(provider)

	if _, _, err := pipeline.analyzeVideo(context.Background(), provider, "asset-1",
		videoanalysis.Input{VideoPath: proxy}, durationMS); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	total, tooLarge := counts()
	if total < 2 {
		t.Fatalf("expected the proxy to be split into several windows against a %d-byte ceiling, got %d request(s)", ceiling, total)
	}
	if tooLarge != 0 {
		t.Fatalf("%d of %d requests exceeded the declared %d-byte ceiling — the splitter is still sizing windows against pre-encoding bytes", tooLarge, total, ceiling)
	}
}

// unknownLimitVideoProvider wraps a real provider but, deliberately, does not
// implement videoproviders.InlineVideoLimiter — exactly the shape of
// internal/app/provider_channel_runtime.go's channelVideo for a
// provider-channel video route, which answers "unknown" for
// MaxInlineVideoBytes rather than resolving a secret merely to size a
// request. This is the reported bug's actual path: "the channel in use
// declared no explicit limit, so the default applied."
type unknownLimitVideoProvider struct {
	inner *openaivideo.Provider
}

func (u *unknownLimitVideoProvider) Name() string  { return u.inner.Name() }
func (u *unknownLimitVideoProvider) Model() string { return u.inner.Model() }
func (u *unknownLimitVideoProvider) Capabilities() []videoproviders.Capability {
	return u.inner.Capabilities()
}
func (u *unknownLimitVideoProvider) Analyze(ctx context.Context, input videoanalysis.Input) (videoanalysis.Result, string, error) {
	return u.inner.Analyze(ctx, input)
}

var _ videoproviders.VideoUnderstandingProvider = (*unknownLimitVideoProvider)(nil)

// This is the channel-routed half of the same arithmetic bug:
// channelVideo.MaxInlineVideoBytes intentionally answers "unknown" rather
// than resolve a secret to size a request, which sends InlineVideoBudget to
// media.DefaultInlineBudgetBytes. That fallback needs the identical base64
// correction as openaivideo's own default, and this proves it against a real
// endpoint rather than the fallback constant's own arithmetic.
func TestAnalyzeVideoFallbackBudgetFitsTheDeclaredCeilingWhenTheProviderDeclinesToDeclareOne(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build and cut the fixture")
	}
	proxy, durationMS := buildHighBitrateFixtureProxy(t, 90, 4000)
	info, err := os.Stat(proxy)
	if err != nil {
		t.Fatal(err)
	}
	// The same round 24MiB figure the pre-fix defaultMaxInlineBytes and
	// DefaultInlineBudgetBytes both used unscaled — the value that produced
	// the reported 413 when a channel declared no explicit limit.
	const wireCeiling = 24 << 20
	if wireCeiling >= info.Size() {
		t.Fatalf("fixture (%d bytes) is not larger than the test ceiling — adjust the bitrate/duration", info.Size())
	}
	server, counts := countingSizeThresholdServer(t, wireCeiling)

	provider := &unknownLimitVideoProvider{inner: &openaivideo.Provider{ProviderName: "channel_routed_test", Endpoint: common.Endpoint{BaseURL: server.URL}}}
	pipeline := newTestPipelineWithVideo(provider)

	if _, _, err := pipeline.analyzeVideo(context.Background(), provider, "asset-1",
		videoanalysis.Input{VideoPath: proxy}, durationMS); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	total, tooLarge := counts()
	if total < 2 {
		t.Fatalf("expected the proxy to be split into several windows against the %d-byte fallback, got %d request(s)", wireCeiling, total)
	}
	if tooLarge != 0 {
		t.Fatalf("%d of %d requests exceeded the %d-byte ceiling — media.DefaultInlineBudgetBytes is still an unscaled pre-encoding figure", tooLarge, total, wireCeiling)
	}
}

// A declared budget is an estimate: bitrate variance or an endpoint whose
// real ceiling is stricter than assumed can still produce a 413. This
// endpoint's threshold is deliberately set so that neither the whole proxy
// nor one bisection is enough — only a window bisected *twice* fits — so a
// pass requires both recovery mechanisms in analyzeVideo: the one-time
// unsplit-path fallback (whole → two halves) and analyzeAnalysisWindow's own
// recursive bisect (a half that still 413s → two quarters). A 413 must not
// cost the asset its whole analysis when a smaller window would have worked.
func TestAnalyzeVideoRecoversFromA413ByBisectingTheWindowTwice(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build and cut the fixture")
	}
	// 120s leaves two floor-respecting halvings: 120s → 60s → 30s, the last
	// exactly at media.MinAnalysisWindowMS.
	proxy, durationMS := buildHighBitrateFixtureProxy(t, 120, 4000)
	info, err := os.Stat(proxy)
	if err != nil {
		t.Fatal(err)
	}
	// 32% of the whole proxy's encoded size: a half (~50% encoded) still
	// exceeds it, a quarter (~25% encoded) fits with margin for the JSON
	// envelope and keyframe-alignment overshoot.
	wireCeiling := int64(base64.StdEncoding.EncodedLen(int(info.Size()))) * 32 / 100
	server, counts := countingSizeThresholdServer(t, wireCeiling)

	// A generous declared ceiling: the provider believes (wrongly, per this
	// endpoint) that the whole proxy fits in one request, so analyzeVideo
	// takes the unsplit path first and must recover from its own 413.
	provider := &openaivideo.Provider{ProviderName: "shrink_recovery_test", Endpoint: common.Endpoint{BaseURL: server.URL}, MaxInlineBytes: info.Size() * 50}
	pipeline := newTestPipelineWithVideo(provider)

	result, _, err := pipeline.analyzeVideo(context.Background(), provider, "asset-1",
		videoanalysis.Input{VideoPath: proxy}, durationMS)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	total, tooLarge := counts()
	// One whole-proxy attempt plus two half-window attempts must have been
	// rejected before four quarter-windows succeeded.
	if tooLarge < 3 {
		t.Fatalf("expected at least 3 rejected attempts (whole + two halves) before recovery, got %d of %d requests", tooLarge, total)
	}
	if total <= tooLarge {
		t.Fatalf("every request (%d of %d) got a 413 — the asset never actually recovered by shrinking", tooLarge, total)
	}
	if len(result.Shots) == 0 {
		t.Fatalf("recovered analysis reported no shots: %+v", result)
	}
}

// Once bisection reaches media.MinAnalysisWindowMS there is nothing smaller
// left to try: an endpoint that refuses every request regardless of size is
// a genuine dead end, and the failure must surface as the 413 itself — not
// as an internal error from trying to extract a window with no duration —
// and must not retry forever.
func TestAnalyzeVideoFailsWhenNoWindowSizeIsSmallEnough(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build and cut the fixture")
	}
	proxy, durationMS := buildFixtureProxy(t, 60)
	var requests int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte("request too large"))
	}))
	t.Cleanup(server.Close)

	provider := &openaivideo.Provider{ProviderName: "always_413_test", Endpoint: common.Endpoint{BaseURL: server.URL}, MaxInlineBytes: 8 << 20}
	pipeline := newTestPipelineWithVideo(provider)

	_, _, err := pipeline.analyzeVideo(context.Background(), provider, "asset-1",
		videoanalysis.Input{VideoPath: proxy}, durationMS)
	if err == nil {
		t.Fatal("an endpoint that always answers 413 must not be reported as a successful analysis")
	}
	var status *common.StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected the 413 to surface as a *common.StatusError, got %v", err)
	}
	mu.Lock()
	got := requests
	mu.Unlock()
	t.Logf("requests = %d", got)
	// Tightly bounded, not merely finite: this 60s asset takes one
	// whole-proxy attempt, then the unsplit-path fallback bisects it into two
	// windows already at MinAnalysisWindowMS — the first of which fails and
	// (like any failed window) stops the loop before the second is ever
	// attempted, exactly as it would for an ordinary non-413 failure. Two
	// requests total, neither bisected further. A gate that forgets to stop
	// at the floor would keep halving well past it — ffmpeg tolerates a
	// sub-second -t extraction, so it degrades into dozens of extra requests
	// rather than an immediate, cheap extraction error — and this tight a
	// bound catches that even though the count never grows without limit.
	if got != 2 {
		t.Fatalf("request count = %d, want exactly 2 (whole + the first window at the MinAnalysisWindowMS floor, not bisected further)", got)
	}
}

// A whole asset shorter than 2*media.MinAnalysisWindowMS cannot be bisected
// at all without producing a window under the floor — the unsplit path's own
// dead end. This must fail on the single whole-proxy attempt, not spend a
// second request trying to split something already too short to split.
func TestAnalyzeVideoGivesUpWithoutSplittingAnAssetAlreadyTooShort(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build the fixture")
	}
	// 40s: half of it (20s) is under media.MinAnalysisWindowMS (30s), so
	// there is no smaller window this pipeline will ever plan.
	proxy, durationMS := buildFixtureProxy(t, 40)
	var requests int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte("request too large"))
	}))
	t.Cleanup(server.Close)

	provider := &openaivideo.Provider{ProviderName: "too_short_to_split_test", Endpoint: common.Endpoint{BaseURL: server.URL}, MaxInlineBytes: 64 << 20}
	pipeline := newTestPipelineWithVideo(provider)

	_, _, err := pipeline.analyzeVideo(context.Background(), provider, "asset-1",
		videoanalysis.Input{VideoPath: proxy}, durationMS)
	var status *common.StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected the 413 to surface as a *common.StatusError, got %v", err)
	}
	mu.Lock()
	got := requests
	mu.Unlock()
	if got != 1 {
		t.Fatalf("request count = %d, want exactly 1 — an asset already too short to split must not spend a second request trying", got)
	}
}

// The model is shown one window; giving it the whole asset's transcript would
// describe speech that is not in the clip it is looking at.
func TestSliceTranscriptNarrowsAndRebasesToTheWindow(t *testing.T) {
	transcript := &domain.Transcript{
		Language: "zh",
		Text:     "one two three",
		Segments: []domain.TranscriptSegment{
			{StartMS: 0, EndMS: 5_000, Text: "one"},
			{StartMS: 310_000, EndMS: 315_000, Text: "two"},
			{StartMS: 700_000, EndMS: 705_000, Text: "three"},
		},
	}
	sliced := sliceTranscript(transcript, media.AnalysisWindow{StartMS: 300_000, EndMS: 600_000})
	if len(sliced.Segments) != 1 {
		t.Fatalf("expected only the segment inside the window, got %+v", sliced.Segments)
	}
	if sliced.Segments[0].StartMS != 10_000 || sliced.Segments[0].EndMS != 15_000 {
		t.Fatalf("segment was not re-based to window-relative time: %+v", sliced.Segments[0])
	}
	if sliced.Text != "two" {
		t.Fatalf("text = %q, want only the speech inside the window", sliced.Text)
	}
	if sliceTranscript(nil, media.AnalysisWindow{StartMS: 0, EndMS: 1000}) != nil {
		t.Fatal("a missing transcript must stay missing")
	}
}

// Case A: an ASR provider (Qwen, Volcengine) returns the full text with a
// single 0-0 placeholder segment. That segment is not a timestamp — it must
// not make the transcript look timed, and the text must not be handed to any
// window, not even the first one, which would otherwise attribute speech from
// anywhere in the asset to the window's footage.
func TestSliceTranscriptWithholdsUntimedTranscriptFromEveryWindow(t *testing.T) {
	transcript := &domain.Transcript{
		Language: "zh",
		Text:     "整段视频的完整语音内容",
		Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 0, Text: "整段视频的完整语音内容"}},
	}
	for _, window := range []media.AnalysisWindow{
		{Index: 0, StartMS: 0, EndMS: 300_000},
		{Index: 1, StartMS: 295_000, EndMS: 600_000},
	} {
		if got := sliceTranscript(transcript, window); got != nil {
			t.Fatalf("window %d-%d received an untimed transcript: %+v", window.StartMS, window.EndMS, got)
		}
	}
}

// A transcript with a literally empty segment slice is untimed: with no
// segment spanning real media time, the full text has no place on the
// timeline and must not be copied into any window. This is the nil-segments
// shape the domain Timed() test covers; this is its window-slicing side.
func TestSliceTranscriptWithholdsEmptySegmentSlice(t *testing.T) {
	transcript := &domain.Transcript{
		Language: "zh",
		Text:     "整段视频的完整语音内容",
		Segments: []domain.TranscriptSegment{},
	}
	for _, window := range []media.AnalysisWindow{
		{Index: 0, StartMS: 0, EndMS: 300_000},
		{Index: 1, StartMS: 295_000, EndMS: 600_000},
	} {
		if got := sliceTranscript(transcript, window); got != nil {
			t.Fatalf("window %d-%d received a segmentless transcript: %+v", window.StartMS, window.EndMS, got)
		}
	}
}

// Case C: a window that ends before a segment begins must not receive its
// text, whether the transcript is timed by ASR segments or by alignment
// words. 310s of speech belongs to the 300-600s window, never to 0-300s.
func TestSliceTranscriptWindowBeforeSpeechGetsNothing(t *testing.T) {
	transcript := &domain.Transcript{
		Text: "hello",
		Segments: []domain.TranscriptSegment{
			{StartMS: 0, EndMS: 5_000, Text: "intro"},
			{StartMS: 310_000, EndMS: 315_000, Text: "hello"},
		},
	}
	early := sliceTranscript(transcript, media.AnalysisWindow{StartMS: 0, EndMS: 300_000})
	if early == nil {
		t.Fatal("timed transcript must be sliced even when nothing overlaps")
	}
	if len(early.Segments) != 1 || early.Segments[0].Text != "intro" {
		t.Fatalf("0-300s window received out-of-window speech: %+v", early.Segments)
	}
	if strings.Contains(early.Text, "hello") {
		t.Fatalf("0-300s window must not receive 310s text: %q", early.Text)
	}
}

// A mixed transcript — real segments plus one degenerate 0-0 placeholder —
// is timed overall, and the placeholder must never reach the model inside
// any window, not even the first.
func TestSliceTranscriptDropsDegenerateSegmentInsideTimedTranscript(t *testing.T) {
	transcript := &domain.Transcript{
		Text: "hello full",
		Segments: []domain.TranscriptSegment{
			{StartMS: 0, EndMS: 0, Text: "full text placeholder"},
			{StartMS: 0, EndMS: 5_000, Text: "hello"},
			{StartMS: 100, EndMS: 50, Text: "inverted"},
		},
	}
	for _, window := range []media.AnalysisWindow{
		{Index: 0, StartMS: 0, EndMS: 300_000},
		{Index: 1, StartMS: 295_000, EndMS: 600_000},
	} {
		sliced := sliceTranscript(transcript, window)
		if sliced == nil {
			t.Fatal("a timed transcript must be sliced, not withheld")
		}
		for _, segment := range sliced.Segments {
			if segment.EndMS <= segment.StartMS {
				t.Fatalf("degenerate segment %+v reached the model in window %d", segment, window.Index)
			}
		}
	}
}

// TranscriptFromAlignmentWords is the canonical timed-transcript constructor:
// word timestamps from a forced alignment place speech precisely, and the
// analysis windows must be fed from them rather than from an untimed ASR text.
func TestTranscriptFromAlignmentWords(t *testing.T) {
	aligned := domain.TranscriptFromAlignmentWords([]domain.AlignmentWord{
		{StartMS: 310_000, EndMS: 313_000, Text: "hel"},
		{StartMS: 313_000, EndMS: 315_000, Text: "lo"},
	})
	if aligned == nil || !aligned.Timed() {
		t.Fatal("valid alignment words must produce a timed transcript")
	}
	if len(aligned.Segments) != 2 || aligned.Segments[1].EndMS != 315_000 {
		t.Fatalf("segments not built from words: %+v", aligned.Segments)
	}
	if aligned.Text != "hel lo" {
		t.Fatalf("text = %q, want words joined", aligned.Text)
	}
	if domain.TranscriptFromAlignmentWords([]domain.AlignmentWord{{StartMS: 0, EndMS: 0, Text: "no timing"}}) != nil {
		t.Fatal("degenerate words must not produce a transcript")
	}
}

func buildFixtureProxy(t *testing.T, seconds int) (string, int64) {
	t.Helper()
	dir := t.TempDir()
	proxy := filepath.Join(dir, "proxy.mp4")
	if output, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25",
		"-t", strconv.Itoa(seconds), "-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "25", proxy).CombinedOutput(); err != nil {
		t.Skipf("test fixture cannot be encoded by local ffmpeg: %v: %s", err, output)
	}
	return proxy, int64(seconds) * 1000
}
