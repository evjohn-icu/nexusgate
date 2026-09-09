package app

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	videoanalysis "github.com/evjohn-icu/nexusgate/internal/domain/video_analysis"
	"github.com/evjohn-icu/nexusgate/internal/normalize"
)

// analyzeVideo cuts an oversized proxy into windows and rebuilds a request for
// each one. Language has to survive that rebuild: a library whose first window
// is described in Chinese and whose second is described in English is worse
// than either, and nothing fails when it happens.
func TestAnalysisLanguageSurvivesWindowSplitting(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to build and cut the fixture")
	}
	proxy, durationMS := buildFixtureProxy(t, 60)
	info, err := os.Stat(proxy)
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingVideoProvider{inlineLimit: info.Size() / 3}
	pipeline := newTestPipelineWithVideo(provider).WithAnalysisLanguage("zh-CN")

	if _, _, err := pipeline.analyzeVideo(context.Background(), provider, "asset-1",
		videoanalysis.Input{VideoPath: proxy, Language: pipeline.analysisLanguage}, durationMS); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(provider.calls) < 2 {
		t.Fatalf("expected the proxy to be split into windows, got %d call(s)", len(provider.calls))
	}
	for i, call := range provider.calls {
		if call.Language != "zh-CN" {
			t.Fatalf("window %d carried Language=%q, want zh-CN: the rebuilt request dropped the language", i, call.Language)
		}
	}
}

// WithAnalysisLanguage is chained, so it must return the pipeline it mutated
// rather than a copy — the setter's whole reason to exist is being appended to
// a NewPipeline expression.
func TestWithAnalysisLanguageIsChainable(t *testing.T) {
	p := (&Pipeline{}).WithAnalysisLanguage("ja-JP")
	if p == nil {
		t.Fatal("WithAnalysisLanguage returned nil; the chained call site would panic")
	}
	if p.analysisLanguage != "ja-JP" {
		t.Fatalf("analysisLanguage=%q, want ja-JP", p.analysisLanguage)
	}
}

// The clause has to move the prose without moving the vocabulary. A model that
// translates asset_type does not produce a nicer answer: ValidateAndNormalize
// matches those fields against English value sets, rejects permanently, and the
// paid call is spent for nothing. So the prompt must name the prose fields and
// say the constrained ones stay English — assert both halves, not just that
// some language text appeared.
func TestOutputLanguagePromptScopesToProse(t *testing.T) {
	clause := normalize.OutputLanguagePrompt("zh-CN")
	if clause == "" {
		t.Fatal("zh-CN produced no clause")
	}
	for _, prose := range []string{"summary", "editorial_reason", "description"} {
		if !strings.Contains(clause, prose) {
			t.Errorf("clause does not name the free-text field %q, so the model is not told to translate it: %q", prose, clause)
		}
	}
	for _, constrained := range []string{"asset_type", "camera_motion", "shot_size", "audio_type", "quality", "usable_as"} {
		if !strings.Contains(clause, constrained) {
			t.Errorf("clause does not name %q as staying English; a translated value there is a permanent failure: %q", constrained, clause)
		}
	}
	if !strings.Contains(clause, "简体中文") {
		t.Errorf("clause does not name the language the way a model recognises it: %q", clause)
	}
}

// An unconfigured or unrecognised tag must leave the prompt untouched. Falling
// back to English would be a silent choice of language for a deployment that
// never made one.
func TestOutputLanguagePromptIsEmptyWhenUnset(t *testing.T) {
	for _, tag := range []string{"", "   ", "kl-GL", "not-a-tag"} {
		if got := normalize.OutputLanguagePrompt(tag); got != "" {
			t.Errorf("OutputLanguagePrompt(%q) = %q, want empty: an unknown tag must not pick a language", tag, got)
		}
	}
}

// A cached model run is keyed on the prompt's identity. The language is part of
// the prompt, so it has to be part of that identity — otherwise switching the
// language hands back the answer written in the old one, for every asset already
// analysed, and the setting appears to do nothing at all.
func TestPromptVersionSeparatesLanguages(t *testing.T) {
	base := "footage-analysis-v4"
	unset := (&Pipeline{}).promptVersion(base)
	if unset != base {
		t.Fatalf("promptVersion(%q) = %q with no language set; an unset language must not invalidate an existing cache", base, unset)
	}
	zh := (&Pipeline{}).WithAnalysisLanguage("zh-CN").promptVersion(base)
	ja := (&Pipeline{}).WithAnalysisLanguage("ja-JP").promptVersion(base)
	if zh == base || ja == base {
		t.Fatalf("a configured language did not change the prompt identity (zh=%q ja=%q base=%q): the cached answer in the old language would be reused", zh, ja, base)
	}
	if zh == ja {
		t.Fatalf("zh-CN and ja-JP share the prompt identity %q: one library's analyses would satisfy the other's", zh)
	}
}
