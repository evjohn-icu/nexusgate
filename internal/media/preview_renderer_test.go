package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/testhelper"
)

func TestPreviewRendererRejectsAppleLogWithoutLUT(t *testing.T) {
	renderer := NewPreviewRenderer("")
	_, err := renderer.RenderProxy(context.Background(), "apple-log.mov", filepath.Join(t.TempDir(), "proxy.mp4"), HardwarePlan{Mode: "software"}, PreviewRenderPlan{
		SourceColor: SourceColorLogApple,
		Mode:        PreviewRenderLUT,
		RequiresLUT: true,
	})
	if err == nil {
		t.Fatal("expected Apple Log render to require a LUT")
	}
	var renderErr *PreviewRenderError
	if !errors.As(err, &renderErr) || renderErr.Code != PreviewErrorLUTRequired || !renderErr.Recoverable {
		t.Fatalf("error=%v, want recoverable LUT-required error", err)
	}
}

func TestPreviewRendererRejectsRAWWithoutInvokingFFmpeg(t *testing.T) {
	renderer := NewPreviewRenderer("")
	dir := t.TempDir()
	dst := filepath.Join(dir, "thumb.jpg")
	_, err := renderer.RenderThumbnail(context.Background(), "frame.BRAW", dst, HardwarePlan{Mode: "software"}, PreviewRenderPlan{
		SourceColor: SourceColorRAW,
		Mode:        PreviewRenderUnavailable,
	})
	if err == nil {
		t.Fatal("expected RAW render to be rejected")
	}
	var renderErr *PreviewRenderError
	if !errors.As(err, &renderErr) || renderErr.Code != PreviewErrorRendererUnavailable || !renderErr.Recoverable {
		t.Fatalf("error=%v, want recoverable renderer-unavailable error", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("RAW render created an output or unexpected path state: %v", statErr)
	}
}

func TestPreviewRendererRefusesToOverwriteOriginal(t *testing.T) {
	binDir := t.TempDir()
	testhelper.InstallCommand(t, binDir, "ffmpeg")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	src := filepath.Join(t.TempDir(), "source.mp4")
	original := []byte("source bytes must remain unchanged")
	if err := os.WriteFile(src, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewPreviewRenderer("").RenderThumbnail(context.Background(), src, src, HardwarePlan{Mode: "software"}, PreviewRenderPlan{
		SourceColor: SourceColorSDR,
		Mode:        PreviewRenderDirect,
		Available:   true,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite original") {
		t.Fatalf("same source/output path error=%v, want immutable-source error", err)
	}
	got, readErr := os.ReadFile(src)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("source changed from %q to %q", original, got)
	}
}

func TestPreviewRendererBuildsSafeHDRToSDRRec709Filter(t *testing.T) {
	renderer := NewPreviewRenderer("")
	filter, err := renderer.VideoFilter(PreviewRenderPlan{
		SourceColor: SourceColorHDRHLG,
		Mode:        PreviewRenderToneMap,
		Filter:      "tonemap",
	}, "scale=720:-2:force_original_aspect_ratio=decrease")
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"zscale=t=linear", "tonemap=tonemap=hable", "zscale=p=bt709", "t=bt709", "m=bt709", "format=yuv420p"} {
		if !strings.Contains(filter, part) {
			t.Fatalf("HDR filter %q missing %q", filter, part)
		}
	}
}

func TestPreviewRendererUsesExplicitAppleLogLUT(t *testing.T) {
	lut := filepath.Join(t.TempDir(), "apple-log.cube")
	if err := os.WriteFile(lut, []byte("TITLE \"test\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	renderer := NewPreviewRenderer(lut)
	plan, err := renderer.ResolvePlan(PreviewRenderPlan{
		SourceColor: SourceColorLogApple,
		Mode:        PreviewRenderLUT,
		RequiresLUT: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Available || plan.RequiresLUT != true {
		t.Fatalf("resolved Apple Log plan=%+v", plan)
	}
	filter, err := renderer.VideoFilter(plan, "scale=720:-2:force_original_aspect_ratio=decrease")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filter, "lut3d") || !strings.Contains(filter, "apple-log.cube") {
		t.Fatalf("Apple Log filter=%q", filter)
	}
}

func TestGenerateProxyRejectsRAWByPathBeforeFFmpeg(t *testing.T) {
	_, err := GenerateProxy(context.Background(), "frame.braw", filepath.Join(t.TempDir(), "proxy.mp4"), HardwarePlan{Mode: "software"})
	if err == nil {
		t.Fatal("expected RAW proxy generation to be rejected")
	}
	var renderErr *PreviewRenderError
	if !errors.As(err, &renderErr) || renderErr.Code != PreviewErrorRendererUnavailable {
		t.Fatalf("error=%v, want RAW renderer-unavailable error", err)
	}
}

// escapeFilterValue must emit forward slashes regardless of the platform
// path separator, because FFmpeg's filter graph syntax accepts '/' on all
// platforms. On Windows filepath.Clean would turn '/' into '\', and the
// replacer would escape each backslash, producing '\\' in the LUT path —
// which the lut3d filter may or may not handle depending on the FFmpeg build.
//
// filepath.ToSlash replaces the OS-specific separator with '/'; on Unix that
// is a no-op (the separator is already '/'). The Windows-style path below is
// skipped on Unix because backslash is not a path separator there and
// filepath.ToSlash cannot convert it.
func TestEscapeFilterValueUsesForwardSlashes(t *testing.T) {
	tests := []struct {
		input string
	}{
		{`/home/user/apple-log.cube`},
		{`../luts/apple-log.cube`},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests, struct{ input string }{`C:\Users\user\apple-log.cube`})
	}
	for _, tt := range tests {
		got := escapeFilterValue(tt.input)
		if strings.Contains(got, "\\") {
			t.Fatalf("escapeFilterValue(%q) = %q contains backslash", tt.input, got)
		}
		if !strings.Contains(got, "apple-log.cube") {
			t.Fatalf("escapeFilterValue(%q) = %q missing expected filename", tt.input, got)
		}
	}
}

func TestGenerateProxyPassesClassifiedHDRFilterToFFmpeg(t *testing.T) {
	binDir := t.TempDir()
	filterLog := filepath.Join(binDir, "filter.txt")
	testhelper.InstallCommand(t, binDir, "ffprobe")
	testhelper.InstallCommand(t, binDir, "ffmpeg")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NEXUSSLATE_FILTER_LOG", filterLog)
	t.Setenv("NEXUSSLATE_FAKE_FFPROBE_COLOR_TRANSFER", "arib-std-b67")

	if _, err := GenerateProxy(context.Background(), "hdr.mov", filepath.Join(t.TempDir(), "proxy.mp4"), HardwarePlan{Mode: "software"}); err != nil {
		t.Fatal(err)
	}
	filter, err := os.ReadFile(filterLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"zscale=t=linear", "tonemap=tonemap=hable", "zscale=p=bt709"} {
		if !strings.Contains(string(filter), part) {
			t.Fatalf("GenerateProxy filter=%q missing %q", filter, part)
		}
	}
}
