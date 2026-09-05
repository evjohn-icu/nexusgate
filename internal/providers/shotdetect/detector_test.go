package shotdetect

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/testhelper"
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

func TestNormalizeMergesShortShotIntoPreviousWhenAdjacent(t *testing.T) {
	// The real ffmpeg scene-detector shape: boundaries are adjacent (next
	// shot starts exactly where this one ends), so there is no room to
	// stretch — the 150 ms shot must dissolve into its predecessor. This is
	// the case the old stretch implementation got wrong: with
	// next.StartMS == current.EndMS the stretch cap was zero and the flash
	// survived as a canonical shot.
	out, err := Normalize([]ShotBound{
		{StartMS: 0, EndMS: 1000},
		{StartMS: 1000, EndMS: 1150},
		{StartMS: 1150, EndMS: 3000},
	}, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 shots after merge, got %+v", out)
	}
	if out[0].StartMS != 0 || out[0].EndMS != 1150 || out[1].StartMS != 1150 || out[1].EndMS != 3000 {
		t.Fatalf("got %+v, want [{0 1150} {1150 3000}]", out)
	}
}

func TestNormalizeMergesShortShotIntoPreviousWithGap(t *testing.T) {
	// A detector that leaves a gap between shots still merges into the
	// previous one; the pre-existing gap is preserved, not widened.
	out, err := Normalize([]ShotBound{
		{StartMS: 0, EndMS: 1000},
		{StartMS: 1000, EndMS: 1150},
		{StartMS: 2000, EndMS: 3000},
	}, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 shots after merge, got %+v", out)
	}
	if out[0].EndMS != 1150 || out[1].StartMS != 2000 {
		t.Fatalf("got %+v, want [{0 1150} {2000 3000}]", out)
	}
}

func TestNormalizeMergesShortFirstShotIntoNext(t *testing.T) {
	out, err := Normalize([]ShotBound{
		{StartMS: 0, EndMS: 100},
		{StartMS: 100, EndMS: 2000},
	}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].StartMS != 0 || out[0].EndMS != 2000 {
		t.Fatalf("got %+v, want single [{0 2000}]", out)
	}
}

func TestNormalizeMergesShortFirstShotIntoNextWithGap(t *testing.T) {
	out, err := Normalize([]ShotBound{
		{StartMS: 0, EndMS: 100},
		{StartMS: 500, EndMS: 1000},
	}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].StartMS != 0 || out[0].EndMS != 1000 {
		t.Fatalf("got %+v, want single [{0 1000}]", out)
	}
}

func TestNormalizeMergesShortLastShotIntoPrevious(t *testing.T) {
	out, err := Normalize([]ShotBound{
		{StartMS: 0, EndMS: 1900},
		{StartMS: 1900, EndMS: 2000},
	}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].StartMS != 0 || out[0].EndMS != 2000 {
		t.Fatalf("got %+v, want single [{0 2000}]", out)
	}
}

func TestNormalizeMergesConsecutiveShortShots(t *testing.T) {
	// A run of sub-minimum shots must fully dissolve: no canonical shot below
	// 300 ms may survive, and the merged span keeps full coverage.
	out, err := Normalize([]ShotBound{
		{StartMS: 0, EndMS: 1000},
		{StartMS: 1000, EndMS: 1100},
		{StartMS: 1100, EndMS: 1200},
		{StartMS: 1200, EndMS: 3000},
	}, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 shots, got %+v", out)
	}
	for _, b := range out {
		if b.EndMS-b.StartMS < MinShotLengthMS {
			t.Fatalf("short canonical shot survived the merge: %+v", out)
		}
	}
	if out[0].EndMS != 1200 || out[1].StartMS != 1200 || out[1].EndMS != 3000 {
		t.Fatalf("got %+v, want [{0 1200} {1200 3000}]", out)
	}
}

func TestNormalizeConsecutiveShortShotsAtStartDissolveForward(t *testing.T) {
	out, err := Normalize([]ShotBound{
		{StartMS: 0, EndMS: 100},
		{StartMS: 100, EndMS: 200},
		{StartMS: 200, EndMS: 2000},
	}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].EndMS != 2000 {
		t.Fatalf("got %+v, want single [{0 2000}]", out)
	}
}

func TestNormalizeSingleShortShotExtendsToFullCoverage(t *testing.T) {
	out, err := Normalize([]ShotBound{{StartMS: 900, EndMS: 950}}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].StartMS != 0 || out[0].EndMS != 1000 {
		t.Fatalf("got %+v, want [{0 1000}]", out)
	}
}

func TestNormalizeSingleShortShotWithoutDurationStaysPut(t *testing.T) {
	out, err := Normalize([]ShotBound{{StartMS: 900, EndMS: 950}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].StartMS != 900 || out[0].EndMS != 950 {
		t.Fatalf("got %+v", out)
	}
}

func TestExternalCommandEmpty(t *testing.T) {
	d := &ExternalCommand{}
	if _, err := d.Detect(context.Background(), "/tmp/v.mp4", 5000); err == nil || !contains(err.Error(), "command is empty") {
		t.Fatalf("got %v", err)
	}
}

func TestExternalCommandSuccess(t *testing.T) {
	command := testhelper.InstallCommand(t, t.TempDir(), "shotdetect-success")
	d := &ExternalCommand{Command: command, Args: []string{"--flag"}}
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
	command := testhelper.InstallCommand(t, t.TempDir(), "shotdetect-invalid")
	d := &ExternalCommand{Command: command}
	if _, err := d.Detect(context.Background(), "/tmp/v.mp4", 1000); err == nil || !contains(err.Error(), "decode shot detector output") {
		t.Fatalf("got %v", err)
	}
}

func TestExternalCommandRejectsEmptyShots(t *testing.T) {
	command := testhelper.InstallCommand(t, t.TempDir(), "shotdetect-empty")
	d := &ExternalCommand{Command: command}
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

// TestFFmpegSceneIntegrationFlashMerge is the release smoke for the merge
// rule: scene A, a 150 ms flash (a transition frame), scene B. The detector
// must not leave a canonical shot below MinShotLengthMS — the flash dissolves
// into its neighbor instead of becoming an extra model call and a retrieval
// false positive. Skipped without ffmpeg, like the hard-cut test above.
func TestFFmpegSceneIntegrationFlashMerge(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "flash.mp4")
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=25",
		"-f", "lavfi", "-i", "testsrc2=duration=0.15:size=320x240:rate=25",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=25",
		"-filter_complex", "[0:v]trim=duration=1[v0];[1:v]trim=duration=0.15[v1];[2:v]trim=duration=1[v2];[v0][v1][v2]concat=n=3:v=1",
		"-an", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", src,
	}
	if raw, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("generate flash clip: %v: %s", err, raw)
	}
	raw, err := (&FFmpegScene{Threshold: 0.3}).Detect(ctx, src, 2150)
	if err != nil {
		t.Fatal(err)
	}
	shots, err := Normalize(raw, 2150)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range shots {
		if s.EndMS-s.StartMS < MinShotLengthMS {
			t.Fatalf("canonical shot below %dms survived the merge: %+v (raw %+v)", MinShotLengthMS, shots, raw)
		}
	}
	if shots[len(shots)-1].EndMS != 2150 {
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
