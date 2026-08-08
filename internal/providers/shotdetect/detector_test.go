package shotdetect

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNormalizeRejectsStructuralGarbage(t *testing.T) {
	tests := []struct {
		name     string
		bounds   []ShotBound
		duration int64
		wantErr  string
	}{
		{"negative start", []ShotBound{{StartMS: -5, EndMS: 100}}, 1000, "invalid shot time range"},
		{"inverted", []ShotBound{{StartMS: 200, EndMS: 100}}, 1000, "invalid shot time range"},
		{"zero length", []ShotBound{{StartMS: 200, EndMS: 200}}, 1000, "invalid shot time range"},
		{"overlap", []ShotBound{{StartMS: 0, EndMS: 500}, {StartMS: 400, EndMS: 900}}, 1000, "overlaps its predecessor"},
		{"beyond duration", []ShotBound{{StartMS: 0, EndMS: 1500}}, 1000, "ends after asset duration"},
		{"too many shots", makeManyShots(MaxShots + 1), 100_000_000, "exceeds limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Normalize(tc.bounds, tc.duration); err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			} else if !contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func makeManyShots(n int) []ShotBound {
	shots := make([]ShotBound, 0, n)
	for i := 0; i < n; i++ {
		start := int64(i) * 1000
		shots = append(shots, ShotBound{StartMS: start, EndMS: start + 900})
	}
	return shots
}

func TestNormalizeAcceptsValidAndAdjacent(t *testing.T) {
	bounds := []ShotBound{{StartMS: 0, EndMS: 500}, {StartMS: 500, EndMS: 900}}
	out, err := Normalize(bounds, 900)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].EndMS != 500 || out[1].StartMS != 500 {
		t.Fatalf("got %+v", out)
	}
}

func TestNormalizeMergesShortShotsIntoNeighbor(t *testing.T) {
	// 150 ms shot between two healthy ones is stretched to 300 ms, capped by
	// the next shot's start.
	out, err := Normalize([]ShotBound{
		{StartMS: 0, EndMS: 1000},
		{StartMS: 1000, EndMS: 1150},
		{StartMS: 2000, EndMS: 3000},
	}, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 shots, got %+v", out)
	}
	if out[1].EndMS != 1300 {
		t.Errorf("short shot stretched to %dms, want 1300ms", out[1].EndMS)
	}
	if out[2].StartMS != 2000 {
		t.Errorf("neighbor start moved to %dms, want 2000ms", out[2].StartMS)
	}
}

func TestNormalizeMergesShortFirstShot(t *testing.T) {
	out, err := Normalize([]ShotBound{
		{StartMS: 0, EndMS: 100},
		{StartMS: 500, EndMS: 1000},
	}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].EndMS != 300 {
		t.Fatalf("got %+v, want first shot stretched to 300ms", out)
	}
}

func TestNormalizeShortShotCappedByDuration(t *testing.T) {
	out, err := Normalize([]ShotBound{{StartMS: 900, EndMS: 950}}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].EndMS != 1000 {
		t.Fatalf("got %+v, want end capped at duration", out)
	}
}

func helperScript(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "detector.sh")
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	return path
}

func TestExternalCommandEmpty(t *testing.T) {
	d := &ExternalCommand{}
	if _, err := d.Detect(context.Background(), "/tmp/v.mp4", 5000); err == nil || !contains(err.Error(), "command is empty") {
		t.Fatalf("got %v", err)
	}
}

func TestExternalCommandSuccess(t *testing.T) {
	script := helperScript(t, `#!/bin/sh
cat >/dev/null
echo '{"shots":[{"start_ms":0,"end_ms":4300},{"start_ms":4300,"end_ms":9000}]}'
`)
	d := &ExternalCommand{Command: script, Args: []string{"--flag"}}
	if d.Name() == "" {
		t.Fatal("Name() is empty")
	}
	shots, err := d.Detect(context.Background(), "/tmp/v.mp4", 9000)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 2 || shots[1].StartMS != 4300 || shots[1].EndMS != 9000 {
		t.Fatalf("got %+v", shots)
	}
}

func TestExternalCommandNameTracksCommandLine(t *testing.T) {
	a := (&ExternalCommand{Command: "/bin/det", Args: []string{"-t", "0.3"}}).Name()
	b := (&ExternalCommand{Command: "/bin/det", Args: []string{"-t", "0.5"}}).Name()
	if a == b {
		t.Fatalf("different argument sets share identity %q", a)
	}
}

func TestExternalCommandRejectsBadOutput(t *testing.T) {
	script := helperScript(t, `#!/bin/sh
echo 'not json'
`)
	d := &ExternalCommand{Command: script}
	if _, err := d.Detect(context.Background(), "/tmp/v.mp4", 1000); err == nil || !contains(err.Error(), "decode shot detector output") {
		t.Fatalf("got %v", err)
	}
}

func TestExternalCommandRejectsEmptyShots(t *testing.T) {
	script := helperScript(t, `#!/bin/sh
echo '{"shots":[]}'
`)
	d := &ExternalCommand{Command: script}
	if _, err := d.Detect(context.Background(), "/tmp/v.mp4", 1000); err == nil || !contains(err.Error(), "no shots") {
		t.Fatalf("got %v", err)
	}
}

func TestExternalCommandFailure(t *testing.T) {
	d := &ExternalCommand{Command: "/nonexistent/shotdetect_cmd"}
	if _, err := d.Detect(context.Background(), "/tmp/v.mp4", 1000); err == nil || !contains(err.Error(), "shot detector failed") {
		t.Fatalf("got %v", err)
	}
}

func TestFFmpegSceneNameIncludesThreshold(t *testing.T) {
	a := (&FFmpegScene{Threshold: 0.3}).Name()
	b := (&FFmpegScene{Threshold: 0.5}).Name()
	if a == b {
		t.Fatalf("thresholds collapse into one identity: %q %q", a, b)
	}
	// An unset threshold resolves to the default 0.3, so the empty struct
	// shares the explicit 0.3 identity — that is the point: the identity is
	// the effective configuration, not the struct fields.
	if c := (&FFmpegScene{}).Name(); c != a {
		t.Fatalf("default threshold identity %q differs from explicit 0.3 identity %q", c, a)
	}
}

// TestFFmpegSceneIntegration detects a hard cut in a generated clip; skipped
// without ffmpeg, mirroring the media integration tests.
func TestFFmpegSceneIntegration(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "cut.mp4")
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=25",
		"-f", "lavfi", "-i", "testsrc2=duration=2:size=320x240:rate=25",
		"-filter_complex", "[0:v]trim=duration=2[v0];[1:v]trim=duration=2[v1];[v0][v1]concat=n=2:v=1",
		"-an", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", src,
	}
	if raw, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("generate cut clip: %v: %s", err, raw)
	}
	raw, err := (&FFmpegScene{Threshold: 0.3}).Detect(ctx, src, 4000)
	if err != nil {
		t.Fatal(err)
	}
	shots, err := Normalize(raw, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) < 2 {
		t.Fatalf("expected at least 2 shots across a hard cut, got %+v", shots)
	}
	foundCut := false
	for _, s := range shots {
		if s.StartMS >= 1800 && s.StartMS <= 2200 {
			foundCut = true
		}
	}
	if !foundCut {
		t.Fatalf("no shot boundary near the 2 s cut: %+v", shots)
	}
	if shots[len(shots)-1].EndMS != 4000 {
		t.Fatalf("last shot must end at the asset duration, got %+v", shots)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
