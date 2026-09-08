package worker

import (
	"os"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/media"
)

// TestFFmpegDeriverWithPreviewLUT pins the chained-setter contract: the LUT
// path given to WithPreviewLUT must land in the deriver's unexported field and
// the setter must return the same deriver so call sites can chain it onto
// NewFFmpegDeriver. A Worker that never calls the setter must still be usable,
// which is why the zero value stays the empty string.
func TestFFmpegDeriverWithPreviewLUT(t *testing.T) {
	d := NewFFmpegDeriver(media.HardwarePlan{Mode: "software"})
	if d.previewLUTPath != "" {
		t.Fatalf("a freshly constructed FFmpegDeriver must start with an empty previewLUTPath, got %q", d.previewLUTPath)
	}

	chained := d.WithPreviewLUT("/srv/luts/x.cube")
	if chained != d {
		t.Fatalf("WithPreviewLUT must return the same deriver so chaining onto NewFFmpegDeriver works")
	}
	if d.previewLUTPath != "/srv/luts/x.cube" {
		t.Fatalf("WithPreviewLUT must store the configured path in previewLUTPath, got %q", d.previewLUTPath)
	}
}

// TestDerivePreviewRendererReceivesConfiguredLUT is a source guard, not a unit
// test: the LUT wiring in Derive has no output a test can observe without real
// ffmpeg, and tampering with it compiles cleanly while every other test stays
// green. So we read runtime.go and pin the exact call shape instead.
func TestDerivePreviewRendererReceivesConfiguredLUT(t *testing.T) {
	src, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatalf("cannot read runtime.go: %v", err)
	}

	if !strings.Contains(string(src), "media.NewPreviewRenderer(d.previewLUTPath)") {
		t.Fatalf("Worker Derive must hand its configured LUT path to media.NewPreviewRenderer; without this the Worker renders every Log clip ungraded even when worker.json names a LUT")
	}
	if strings.Contains(string(src), `media.NewPreviewRenderer("")`) {
		t.Fatalf("Worker Derive must not call media.NewPreviewRenderer with an empty literal; passing empty string makes the Worker permanently unable to grade Log footage, and this tampering compiles while all other tests stay green")
	}
}
