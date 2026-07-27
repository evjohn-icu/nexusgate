package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	if err := GenerateProxy(context.Background(), src, dst, plan); err != nil {
		t.Fatalf("proxy generation with fallback: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil || info.Size() == 0 {
		t.Fatalf("proxy output missing or empty: info=%v err=%v", info, err)
	}
}
