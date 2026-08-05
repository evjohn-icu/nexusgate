package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// AnalysisWindow is one slice of a proxy small enough to hand to a video model
// in a single request. Times are relative to the start of the proxy, which is
// also the start of the asset, so a shot the model reports inside a window is
// placed on the asset timeline by adding StartMS.
type AnalysisWindow struct {
	Index   int
	StartMS int64
	EndMS   int64
}

// Duration returns the window length in milliseconds.
func (w AnalysisWindow) Duration() int64 { return w.EndMS - w.StartMS }

// WindowPlan describes how a proxy will be presented to a video model.
type WindowPlan struct {
	// Windows is always at least one entry. A proxy that already fits is
	// planned as a single window spanning the whole asset, so callers have one
	// code path rather than two.
	Windows []AnalysisWindow
	// Split reports whether the proxy had to be cut. It exists so a caller can
	// avoid re-encoding anything in the common case.
	Split bool
}

// Analysis window bounds. The minimum keeps a pathologically high bitrate from
// planning windows too short to contain a usable observation; the maximum
// exists because request size is not the only ceiling — a video model also
// spends context per second of footage, and a window can exceed that budget
// long before it exceeds a byte limit.
const (
	MinAnalysisWindowMS      = 30_000
	DefaultMaxWindowMS       = 8 * 60_000
	DefaultWindowOverlapMS   = 5_000
	DefaultInlineBudgetBytes = 24 << 20
)

// PlanAnalysisWindows decides how to cut a proxy so each piece fits budgetBytes.
//
// The window length is derived from the proxy's own measured bytes-per-second
// rather than from a fixed duration, because the same duration is a different
// number of bytes at every bitrate — and the bitrate is not knowable here: it
// depends on the encoder, the profile, and the scene. A fixed duration would be
// wrong for most inputs in one direction or the other.
//
// Consecutive windows overlap so an event sitting on a cut is present whole in
// at least one of them; the caller is expected to de-duplicate the overlap.
func PlanAnalysisWindows(durationMS, proxyBytes, budgetBytes, maxWindowMS, overlapMS int64) WindowPlan {
	whole := WindowPlan{Windows: []AnalysisWindow{{Index: 0, StartMS: 0, EndMS: durationMS}}}
	if durationMS <= 0 || proxyBytes <= 0 || budgetBytes <= 0 {
		return whole
	}
	if maxWindowMS <= 0 {
		maxWindowMS = DefaultMaxWindowMS
	}
	if overlapMS < 0 {
		overlapMS = 0
	}
	if proxyBytes <= budgetBytes && durationMS <= maxWindowMS {
		return whole
	}

	// Integer arithmetic throughout: durationMS*budgetBytes can be large, but
	// both are bounded by real media (hours, tens of megabytes) and int64 holds
	// the product with room to spare.
	windowMS := durationMS * budgetBytes / proxyBytes
	if windowMS > maxWindowMS {
		windowMS = maxWindowMS
	}
	if windowMS < MinAnalysisWindowMS {
		windowMS = MinAnalysisWindowMS
	}
	if windowMS >= durationMS {
		return whole
	}
	// The step must make progress even if a caller passes an overlap as long as
	// the window itself.
	step := windowMS - overlapMS
	if step <= 0 {
		step = windowMS
	}

	plan := WindowPlan{Split: true}
	for start, i := int64(0), 0; start < durationMS; start, i = start+step, i+1 {
		end := start + windowMS
		if end > durationMS {
			end = durationMS
		}
		plan.Windows = append(plan.Windows, AnalysisWindow{Index: i, StartMS: start, EndMS: end})
		if end >= durationMS {
			break
		}
	}
	return plan
}

// ExtractAnalysisWindow copies one window out of src without re-encoding. The
// proxy has already paid for an encode; a second one would cost more than the
// analysis it feeds and would degrade the picture the model sees.
//
// Stream copy can only cut on a keyframe, so the piece may begin slightly
// before StartMS. That is why the caller keeps its own window boundaries for
// timeline arithmetic instead of trusting the extracted file's duration.
func ExtractAnalysisWindow(ctx context.Context, src, dst string, window AnalysisWindow) error {
	if err := rejectSourceOverwrite(src, dst); err != nil {
		return err
	}
	if window.Duration() <= 0 {
		return fmt.Errorf("analysis window %d has no duration", window.Index)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return atomicFFmpegOutput(dst, func(out string) error {
		args := []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-ss", msToTimestamp(window.StartMS),
			"-t", msToTimestamp(window.Duration()),
			"-i", src,
			"-c", "copy", "-movflags", "+faststart",
			out,
		}
		_, err := ffmpegOutput(ctx, nil, args...)
		return err
	})
}

func msToTimestamp(ms int64) string {
	return fmt.Sprintf("%d.%03d", ms/1000, ms%1000)
}
