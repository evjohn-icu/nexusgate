package media

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
)

// SamplePoint is one frame position on the asset timeline. NexusSlate decides
// these positions deterministically; the VLM is never asked to pick them.
type SamplePoint struct {
	TimestampMS int64
}

// Frame sampling modes. boundary-aware prefers the edges of a shot, where a
// new subject or camera move usually announces itself; uniform spreads the
// frames evenly for shots with no obvious structure. The mode is part of the
// analysis provenance (recorded in the model run's request JSON), so a mode
// change re-keys the analysis rather than silently reusing sampled output.
const (
	FrameSamplingBoundaryAware = "boundary-aware"
	FrameSamplingUniform       = "uniform"
)

// Frame budget per shot: the majority of shots are between 2 s and 30 s and
// get four frames — enough to see a subject enter, move, and exit. Shorter
// shots get two, so the model is not shown near-duplicate frames; longer
// shots get six, because a 40 s pan holds more distinct information than the
// middle two thirds of a 4 s shot.
const (
	ShortShotFramesMS = 2_000
	LongShotFramesMS  = 30_000
	ShortShotCount    = 2
	DefaultShotCount  = 4
	LongShotCount     = 6
	MultiframeFrameW  = 768
	MultiframeFrameQ  = "4"
)

// PlanShotFrames returns the frame positions for one shot under the given
// sampling mode. Positions are clamped into [startMS, endMS], rounded to
// whole milliseconds, and deduplicated — a very short shot must still yield
// at least one frame even when its whole length collapses to a single
// millisecond.
func PlanShotFrames(startMS, endMS int64, mode string) []SamplePoint {
	length := endMS - startMS
	if length <= 0 {
		return nil
	}
	var count int
	switch {
	case length < ShortShotFramesMS:
		count = ShortShotCount
	case length > LongShotFramesMS:
		count = LongShotCount
	default:
		count = DefaultShotCount
	}
	var ratios []float64
	switch mode {
	case FrameSamplingUniform:
		ratios = make([]float64, count)
		for i := 0; i < count; i++ {
			ratios[i] = (float64(i) + 0.5) / float64(count)
		}
	default: // boundary-aware
		switch count {
		case ShortShotCount:
			ratios = []float64{0.2, 0.7}
		case LongShotCount:
			ratios = []float64{0.05, 0.20, 0.40, 0.60, 0.80, 0.95}
		default:
			ratios = []float64{0.10, 0.35, 0.65, 0.90}
		}
	}
	points := make([]SamplePoint, 0, len(ratios))
	seen := make(map[int64]struct{}, len(ratios))
	for _, ratio := range ratios {
		ts := startMS + int64(math.Round(float64(length)*ratio))
		if ts < startMS {
			ts = startMS
		}
		if ts > endMS {
			ts = endMS
		}
		if _, ok := seen[ts]; ok {
			continue
		}
		seen[ts] = struct{}{}
		points = append(points, SamplePoint{TimestampMS: ts})
	}
	if len(points) == 0 {
		points = append(points, SamplePoint{TimestampMS: startMS})
	}
	return points
}

// ExtractFrames pulls one JPEG still per SamplePoint out of src into dstDir
// and returns the written paths in point order. The caller's proxy has already
// paid for a decode-friendly encode, so frames are extracted from the proxy
// rather than the source; the stills are scratch, not artifacts, and are
// removed with their directory by the caller.
//
// Each frame is one ffmpeg invocation, seeking with -ss before -i — the same
// keyframe-level approximation the thumbnail renderer accepts. Seeking a
// handful of frames per shot costs far less than a second decode of the whole
// proxy; batch extraction with a select filter is a throughput optimization
// for a later phase.
func ExtractFrames(ctx context.Context, src, dstDir string, points []SamplePoint, width int) ([]string, error) {
	if err := rejectSourceOverwrite(src, dstDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return nil, err
	}
	if width <= 0 {
		width = MultiframeFrameW
	}
	paths := make([]string, 0, len(points))
	for i, point := range points {
		path := filepath.Join(dstDir, fmt.Sprintf("frame-%03d-%d.jpg", i, point.TimestampMS))
		// ffmpeg picks the muxer from the output extension, so the temporary
		// name must keep .jpg or the run fails with "unable to choose an
		// output format" (same trap atomicFFmpegOutput guards against).
		temp, err := os.CreateTemp(dstDir, ".frame-*"+".jpg")
		if err != nil {
			return nil, err
		}
		tmp := temp.Name()
		if err := temp.Close(); err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
		args := []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-ss", msToTimestamp(point.TimestampMS),
			"-i", src,
			"-frames:v", "1",
			"-vf", fmt.Sprintf("scale=%d:-2", width),
			"-q:v", MultiframeFrameQ,
			tmp,
		}
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		if raw, err := cmd.CombinedOutput(); err != nil {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("extract frame at %dms: %w: %s", point.TimestampMS, err, truncateStderr(raw))
		}
		// A still is either complete or absent; a truncated JPEG would reach
		// the VLM as a corrupted image and poison a paid or local inference.
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}
