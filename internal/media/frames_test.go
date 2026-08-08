package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPlanShotFramesBoundaryAwareCounts(t *testing.T) {
	tests := []struct {
		name        string
		start, end  int64
		wantCount   int
		wantFirstMS int64
		wantLastMS  int64
	}{
		{"short shot", 0, 1_000, ShortShotCount, 200, 700},
		{"default shot", 0, 4_000, DefaultShotCount, 400, 3600},
		{"long shot", 0, 60_000, LongShotCount, 3000, 57000},
		{"at the short threshold", 0, ShortShotFramesMS, DefaultShotCount, 200, 1800},
		{"just over the short threshold", 0, ShortShotFramesMS + 1, DefaultShotCount, 200, 1801},
		{"at the long threshold", 0, LongShotFramesMS, DefaultShotCount, 3000, 27000},
		{"just over the long threshold", 0, LongShotFramesMS + 1, LongShotCount, 1500, 28501},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			points := PlanShotFrames(tc.start, tc.end, FrameSamplingBoundaryAware)
			if len(points) != tc.wantCount {
				t.Fatalf("got %d frames, want %d: %+v", len(points), tc.wantCount, points)
			}
			if points[0].TimestampMS != tc.wantFirstMS {
				t.Errorf("first frame at %dms, want %dms", points[0].TimestampMS, tc.wantFirstMS)
			}
			if points[len(points)-1].TimestampMS != tc.wantLastMS {
				t.Errorf("last frame at %dms, want %dms", points[len(points)-1].TimestampMS, tc.wantLastMS)
			}
			// Every frame must sit strictly inside the shot.
			for _, p := range points {
				if p.TimestampMS < tc.start || p.TimestampMS > tc.end {
					t.Errorf("frame %dms outside shot [%d,%d]", p.TimestampMS, tc.start, tc.end)
				}
			}
		})
	}
}

func TestPlanShotFramesUniform(t *testing.T) {
	points := PlanShotFrames(0, 8_000, FrameSamplingUniform)
	if len(points) != DefaultShotCount {
		t.Fatalf("uniform mode frame count = %d, want %d", len(points), DefaultShotCount)
	}
	// 4 uniform points over 8 s: 1000/3000/5000/7000.
	want := []int64{1_000, 3_000, 5_000, 7_000}
	for i, p := range points {
		if p.TimestampMS != want[i] {
			t.Errorf("uniform frame %d at %dms, want %dms", i, p.TimestampMS, want[i])
		}
	}
}

func TestPlanShotFramesClampsToShotBounds(t *testing.T) {
	points := PlanShotFrames(10_000, 11_000, FrameSamplingBoundaryAware)
	for _, p := range points {
		if p.TimestampMS < 10_000 || p.TimestampMS > 11_000 {
			t.Fatalf("frame %dms escapes the shot bounds", p.TimestampMS)
		}
	}
}

func TestPlanShotFramesDedupesVeryShortShot(t *testing.T) {
	// A 1 ms shot rounds every ratio boundary to the same two milliseconds;
	// duplicates must be dropped and the remaining positions must stay inside
	// the shot.
	points := PlanShotFrames(500, 501, FrameSamplingBoundaryAware)
	if len(points) != 2 {
		t.Fatalf("got %+v, want exactly two distinct positions", points)
	}
	for _, p := range points {
		if p.TimestampMS != 500 && p.TimestampMS != 501 {
			t.Fatalf("frame %dms outside the 1 ms shot", p.TimestampMS)
		}
	}
}

func TestPlanShotFramesRejectsInvertedShot(t *testing.T) {
	if points := PlanShotFrames(5_000, 5_000, FrameSamplingBoundaryAware); points != nil {
		t.Fatalf("zero-length shot produced frames: %+v", points)
	}
	if points := PlanShotFrames(6_000, 5_000, FrameSamplingBoundaryAware); points != nil {
		t.Fatalf("inverted shot produced frames: %+v", points)
	}
}

// TestExtractFramesIntegration extracts frames from a generated clip. It is
// skipped without ffmpeg on PATH, mirroring process_integration_test.go.
func TestExtractFramesIntegration(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "clip.mp4")
	genArgs := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=4:size=640x360:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", src,
	}
	if raw, err := exec.CommandContext(ctx, "ffmpeg", genArgs...).CombinedOutput(); err != nil {
		t.Fatalf("generate clip: %v: %s", err, raw)
	}
	points := PlanShotFrames(0, 4_000, FrameSamplingBoundaryAware)
	dir := t.TempDir()
	paths, err := ExtractFrames(ctx, src, dir, points, 384)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != len(points) {
		t.Fatalf("got %d frames, want %d", len(paths), len(points))
	}
	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("frame %d missing: %v", i, err)
		}
		if info.Size() == 0 {
			t.Fatalf("frame %d is empty", i)
		}
	}
}
