package media

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This verifies the important v0.6 safety property with the real FFmpeg
// binary: an unavailable CUDA path is allowed to fall back to software and
// still produces a usable derived proxy. It does not require a GPU.
func TestGenerateProxyFallsBackFromUnavailableHardware(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required for this integration test")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "source.mp4")
	if output, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=24", "-t", "0.2", "-c:v", "libx264", "-pix_fmt", "yuv420p", src).CombinedOutput(); err != nil {
		t.Skipf("test fixture cannot be encoded by local ffmpeg: %v: %s", err, output)
	}
	dst := filepath.Join(dir, "proxy.mp4")
	plan := planFor("cuda", "", true, 500)
	if _, err := GenerateProxy(context.Background(), src, dst, plan); err != nil {
		t.Fatalf("proxy generation with fallback: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil || info.Size() == 0 {
		t.Fatalf("proxy output missing or empty: info=%v err=%v", info, err)
	}
}

// The proxy scale filter has to leave both dimensions even, because libx264
// refuses an odd one under yuv420p. The sizes here are the ones that actually
// broke: every 16:9 source produced an odd height and failed to encode, which
// the 4:3 fixture above happened to miss.
func TestGenerateProxyKeepsDimensionsEvenAcrossAspectRatios(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required for this integration test")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is required for this integration test")
	}
	for _, size := range []string{"1920x1080", "3840x2160", "640x360", "1080x1920", "1920x800"} {
		t.Run(size, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "source.mp4")
			if output, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size="+size+":rate=24", "-t", "0.2", "-c:v", "libx264", "-pix_fmt", "yuv420p", src).CombinedOutput(); err != nil {
				t.Skipf("test fixture cannot be encoded by local ffmpeg: %v: %s", err, output)
			}
			dst := filepath.Join(dir, "proxy.mp4")
			if _, err := GenerateProxy(context.Background(), src, dst, planFor("", "", true, 0)); err != nil {
				t.Fatalf("proxy generation for %s: %v", size, err)
			}
			probe, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "csv=p=0", dst).Output()
			if err != nil {
				t.Fatalf("probe proxy for %s: %v", size, err)
			}
			var width, height int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(probe)), "%d,%d", &width, &height); err != nil {
				t.Fatalf("parse proxy dimensions %q: %v", probe, err)
			}
			if width%2 != 0 || height%2 != 0 {
				t.Fatalf("%s produced an odd proxy dimension %dx%d", size, width, height)
			}
			if height > 720 {
				t.Fatalf("%s produced a proxy taller than the 720 cap: %dx%d", size, width, height)
			}
		})
	}
}
