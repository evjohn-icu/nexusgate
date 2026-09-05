package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	videoanalysis "github.com/evjohn-icu/nexusgate/internal/domain/video_analysis"
	"github.com/evjohn-icu/nexusgate/internal/media"
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
