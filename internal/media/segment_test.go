package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPlanAnalysisWindowsLeavesAFittingProxyWhole(t *testing.T) {
	plan := PlanAnalysisWindows(5*60_000, 10<<20, 24<<20, DefaultMaxWindowMS, DefaultWindowOverlapMS)
	if plan.Split {
		t.Fatal("a proxy inside the budget must not be split")
	}
	if len(plan.Windows) != 1 || plan.Windows[0].StartMS != 0 || plan.Windows[0].EndMS != 5*60_000 {
		t.Fatalf("expected one window spanning the asset, got %+v", plan.Windows)
	}
}

// The 591 MB / 44 min proxy that returned HTTP 413 from the live provider.
func TestPlanAnalysisWindowsSplitsAnOversizedProxy(t *testing.T) {
	const durationMS = 44 * 60_000
	const proxyBytes = 591 << 20
	const budget = 24 << 20

	plan := PlanAnalysisWindows(durationMS, proxyBytes, budget, DefaultMaxWindowMS, DefaultWindowOverlapMS)
	if !plan.Split {
		t.Fatal("an oversized proxy must be split")
	}
	if len(plan.Windows) < 2 {
		t.Fatalf("expected several windows, got %d", len(plan.Windows))
	}
	bytesPerMS := float64(proxyBytes) / float64(durationMS)
	for _, w := range plan.Windows {
		if w.Duration() <= 0 {
			t.Fatalf("window %d has no duration: %+v", w.Index, w)
		}
		if estimated := int64(float64(w.Duration()) * bytesPerMS); estimated > budget {
			t.Fatalf("window %d is still over budget: ~%d bytes > %d", w.Index, estimated, budget)
		}
	}
	if first, last := plan.Windows[0], plan.Windows[len(plan.Windows)-1]; first.StartMS != 0 || last.EndMS != durationMS {
		t.Fatalf("windows must cover the whole asset, got %d..%d", first.StartMS, last.EndMS)
	}
}

// Every instant of the asset has to appear in some window, or analysis silently
// loses footage rather than failing.
func TestPlanAnalysisWindowsCoverTheTimelineWithoutGaps(t *testing.T) {
	plan := PlanAnalysisWindows(30*60_000, 300<<20, 24<<20, DefaultMaxWindowMS, DefaultWindowOverlapMS)
	for i := 1; i < len(plan.Windows); i++ {
		if plan.Windows[i].StartMS > plan.Windows[i-1].EndMS {
			t.Fatalf("gap between window %d (ends %d) and %d (starts %d)",
				i-1, plan.Windows[i-1].EndMS, i, plan.Windows[i].StartMS)
		}
	}
}

// A long asset needs splitting for context reasons even when its bytes fit.
func TestPlanAnalysisWindowsSplitsOnDurationEvenWhenBytesFit(t *testing.T) {
	plan := PlanAnalysisWindows(40*60_000, 1<<20, 24<<20, DefaultMaxWindowMS, DefaultWindowOverlapMS)
	if !plan.Split {
		t.Fatal("an asset longer than the window cap must be split even if it is small")
	}
	for _, w := range plan.Windows {
		if w.Duration() > DefaultMaxWindowMS {
			t.Fatalf("window %d is %dms, over the %dms cap", w.Index, w.Duration(), DefaultMaxWindowMS)
		}
	}
}

// Degenerate inputs must not produce an empty plan or a zero-length loop; the
// caller always expects at least one window it can analyse.
func TestPlanAnalysisWindowsHandlesDegenerateInput(t *testing.T) {
	for _, tc := range []struct {
		name                                            string
		durationMS, proxyBytes, budget, maxWin, overlap int64
	}{
		{"zero duration", 0, 100 << 20, 24 << 20, DefaultMaxWindowMS, DefaultWindowOverlapMS},
		{"zero bytes", 60_000, 0, 24 << 20, DefaultMaxWindowMS, DefaultWindowOverlapMS},
		{"zero budget", 60_000, 100 << 20, 0, DefaultMaxWindowMS, DefaultWindowOverlapMS},
		{"overlap longer than window", 20 * 60_000, 300 << 20, 24 << 20, 60_000, 120_000},
		{"negative overlap", 20 * 60_000, 300 << 20, 24 << 20, DefaultMaxWindowMS, -5_000},
		{"absurd bitrate", 10 * 60_000, 8 << 30, 1 << 20, DefaultMaxWindowMS, DefaultWindowOverlapMS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := PlanAnalysisWindows(tc.durationMS, tc.proxyBytes, tc.budget, tc.maxWin, tc.overlap)
			if len(plan.Windows) == 0 {
				t.Fatal("plan must always contain at least one window")
			}
			if len(plan.Windows) > 10_000 {
				t.Fatalf("plan exploded to %d windows", len(plan.Windows))
			}
			for _, w := range plan.Windows {
				if w.EndMS < w.StartMS {
					t.Fatalf("window %d is inverted: %+v", w.Index, w)
				}
			}
		})
	}
}

func TestExtractAnalysisWindowCopiesWithoutReencoding(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required for this integration test")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is required for this integration test")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "proxy.mp4")
	if output, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-t", "12",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "25", src).CombinedOutput(); err != nil {
		t.Skipf("test fixture cannot be encoded by local ffmpeg: %v: %s", err, output)
	}

	dst := filepath.Join(dir, "window.mp4")
	if err := ExtractAnalysisWindow(context.Background(), src, dst, AnalysisWindow{Index: 1, StartMS: 4_000, EndMS: 8_000}); err != nil {
		t.Fatalf("extract window: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil || info.Size() == 0 {
		t.Fatalf("window output missing or empty: info=%v err=%v", info, err)
	}
	codec, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", dst).Output()
	if err != nil {
		t.Fatalf("probe window: %v", err)
	}
	if got := string(codec); len(got) < 4 || got[:4] != "h264" {
		t.Fatalf("window was re-encoded or is unreadable: codec=%q", got)
	}
}

func TestExtractAnalysisWindowRefusesToOverwriteItsSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "proxy.mp4")
	if err := os.WriteFile(src, []byte("not really a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ExtractAnalysisWindow(context.Background(), src, src, AnalysisWindow{StartMS: 0, EndMS: 1000}); err == nil {
		t.Fatal("extracting onto the source must be refused")
	}
}
