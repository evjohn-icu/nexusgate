// Package shotdetect turns deterministic shot-boundary detection into a
// NexusGate-owned service. The VLM must never be the one to decide where a
// shot begins and ends — it is asked to describe pixels, and NexusGate is the
// one that knows the timeline. Two detectors exist: an external command
// (PySceneDetect wrappers and the like) following the same stdin-JSON /
// stdout-JSON contract as the forced aligner, and a built-in FFmpeg scene
// detector that needs nothing beyond the ffmpeg binary the pipeline already
// depends on.
//
// Whatever the source, the boundaries pass through Normalize, where the
// NexusGate-owned rules are enforced: monotonic, non-overlapping, inside the
// asset, and not shorter than a minimum shot length. A detector that produces
// structural garbage is rejected, not worked around; a shot that is merely
// short is merged into its neighbor.
package shotdetect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// ShotBound is one detected shot on the asset timeline, in milliseconds.
type ShotBound struct {
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
}

// Detector finds shot boundaries in a proxy. Name() is the detector's
// identity for provenance: it is recorded in the model run's request JSON and
// therefore re-keys analysis when the detector (or its threshold, or its
// command line) changes.
type Detector interface {
	Name() string
	Detect(ctx context.Context, videoPath string, durationMS int64) ([]ShotBound, error)
}

const (
	// MinShotLengthMS is the smallest shot worth a model call. A shot below
	// this length is merged into its neighbor rather than analysed on its
	// own: a hundred-millisecond flash carries no information a VLM call can
	// pay for.
	MinShotLengthMS int64 = 300
	// MaxShots bounds one detection the same way maxAnalysisShots bounds one
	// analysis: a detector stuck in a loop must not be able to produce an
	// unbounded shot list.
	MaxShots = 2000
)

// Normalize enforces the NexusGate-owned shot rules on raw detector output
// and returns the shots that may be analysed. The rules below are
// deterministic verdicts on bytes NexusGate already holds, so a violation is
// permanent at the call site; the caller marks it so.
//
// A shot shorter than MinShotLengthMS carries no information a model call can
// pay for — a transition flash becomes an extra canonical shot, an extra VLM
// inference, a caption/object pollution vector and a retrieval false positive
// — so short shots are merged, not stretched. Stretching does not even work
// for adjacent boundaries (a 150 ms shot between 1000 and 1150 ms has no
// stretch room). The merge rule is deterministic and documented:
//
//   - the only shot of an asset: extends to cover the whole asset [0, duration];
//   - the first shot: merges into the next (the next shot's start becomes the
//     short shot's start, so coverage is preserved);
//   - any other shot: merges into the previous (the previous shot's end
//     becomes the short shot's end).
//
// Each merge removes one shot and inherits only already-validated boundaries,
// so monotonicity, non-overlap, start >= 0, end <= duration and full coverage
// are preserved by construction, no new gaps are introduced, and the shot
// count never grows. The loop terminates: every iteration deletes a shot.
func Normalize(bounds []ShotBound, durationMS int64) ([]ShotBound, error) {
	if len(bounds) > MaxShots {
		return nil, fmt.Errorf("shot count %d exceeds limit %d", len(bounds), MaxShots)
	}
	for i, b := range bounds {
		if b.StartMS < 0 || b.EndMS <= b.StartMS {
			return nil, fmt.Errorf("invalid shot time range at ordinal %d: %d-%d", i, b.StartMS, b.EndMS)
		}
		if i > 0 && b.StartMS < bounds[i-1].EndMS {
			return nil, fmt.Errorf("shot at ordinal %d overlaps its predecessor: %d < %d", i, b.StartMS, bounds[i-1].EndMS)
		}
		if durationMS > 0 && b.EndMS > durationMS {
			return nil, fmt.Errorf("shot at ordinal %d ends after asset duration: %d > %d", i, b.EndMS, durationMS)
		}
	}
	out := make([]ShotBound, len(bounds))
	copy(out, bounds)
	for {
		short := -1
		for i, b := range out {
			if b.EndMS-b.StartMS < MinShotLengthMS {
				short = i
				break
			}
		}
		if short < 0 {
			return out, nil
		}
		if len(out) == 1 {
			// A single short shot is the whole asset's observation: extend
			// it to full coverage rather than keep a canonical shot that
			// does not represent the asset. Without a known duration there
			// is nothing to extend to, so the shot is kept as-is.
			if durationMS > 0 {
				out[0] = ShotBound{StartMS: 0, EndMS: durationMS}
			}
			return out, nil
		}
		if short == 0 {
			out[1].StartMS = out[0].StartMS
		} else {
			out[short-1].EndMS = out[short].EndMS
		}
		out = append(out[:short], out[short+1:]...)
	}
}

// ExternalCommand delegates shot detection to an external binary via the
// aligner-style contract: JSON on stdin, JSON on stdout, failure on a
// non-zero exit. The command line is the detector's identity, so swapping the
// wrapper (or its arguments) re-keys the analysis it feeds.
type ExternalCommand struct {
	Command string
	Args    []string
}

func (d *ExternalCommand) Name() string {
	h := sha256.Sum256([]byte(d.Command + "\x00" + strings.Join(d.Args, "\x00")))
	return "shot-detect-" + hex.EncodeToString(h[:8])
}

func (d *ExternalCommand) Detect(ctx context.Context, videoPath string, durationMS int64) ([]ShotBound, error) {
	if d.Command == "" {
		return nil, fmt.Errorf("shot detection command is empty")
	}
	payload, err := json.Marshal(map[string]any{
		"video_path":  videoPath,
		"duration_ms": durationMS,
	})
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, d.Command, d.Args...)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("shot detector failed: %w: %s", err, stderr.String())
	}
	var out struct {
		Shots []ShotBound `json:"shots"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("decode shot detector output: %w", err)
	}
	if len(out.Shots) == 0 {
		return nil, fmt.Errorf("shot detector returned no shots")
	}
	return out.Shots, nil
}

// FFmpegScene detects shot boundaries with ffmpeg's built-in scene filter,
// needing nothing beyond the binary the pipeline already requires. A frame
// whose scene score exceeds the threshold starts a new shot. The threshold is
// part of the detector's identity, so tuning it re-keys the analysis.
type FFmpegScene struct {
	Threshold float64
}

const defaultSceneThreshold = 0.3

func (d *FFmpegScene) Name() string {
	return fmt.Sprintf("ffmpeg-scene-%g", d.threshold())
}

func (d *FFmpegScene) threshold() float64 {
	if d.Threshold <= 0 {
		return defaultSceneThreshold
	}
	return d.Threshold
}

func (d *FFmpegScene) Detect(ctx context.Context, videoPath string, durationMS int64) ([]ShotBound, error) {
	args := []string{
		"-hide_banner", "-loglevel", "info",
		"-i", videoPath,
		"-vf", fmt.Sprintf("select='gt(scene,%g)',metadata=print", d.threshold()),
		"-an", "-f", "null", "-",
	}
	output, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg scene detection: %w: %s", err, truncateOutput(output))
	}
	return buildShots(sceneTimestamps(output, durationMS), durationMS), nil
}

// sceneTimestamps extracts scene-change points from ffmpeg's metadata=print
// output. Only lines from the metadata filter count; the input dump at
// loglevel info carries no pts_time of its own. The first boundary of the
// stream (frame 0, t≈0) is dropped: a shot starting at 0 is the implicit
// first shot, and anything within the minimum shot length is Normalize's
// business, not a boundary candidate.
func sceneTimestamps(output []byte, durationMS int64) []int64 {
	var ms []int64
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if !strings.Contains(line, "[Parsed_metadata_") {
			continue
		}
		idx := strings.LastIndex(line, "pts_time:")
		if idx < 0 {
			continue
		}
		value := strings.TrimSpace(line[idx+len("pts_time:"):])
		seconds, err := strconv.ParseFloat(value, 64)
		if err != nil || seconds <= 0 {
			continue
		}
		ts := int64(seconds * 1000)
		if durationMS > 0 && ts >= durationMS {
			continue
		}
		ms = append(ms, ts)
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
	deduped := ms[:0]
	for _, ts := range ms {
		if len(deduped) == 0 || deduped[len(deduped)-1] != ts {
			deduped = append(deduped, ts)
		}
	}
	return deduped
}

// buildShots turns boundary points into half-open shots covering the asset,
// ending at durationMS. Every clip has at least one shot.
func buildShots(boundaries []int64, durationMS int64) []ShotBound {
	shots := make([]ShotBound, 0, len(boundaries)+1)
	start := int64(0)
	for _, boundary := range boundaries {
		if boundary <= start {
			continue
		}
		shots = append(shots, ShotBound{StartMS: start, EndMS: boundary})
		start = boundary
	}
	if len(shots) == 0 || shots[len(shots)-1].EndMS != durationMS {
		end := durationMS
		if end <= 0 {
			end = start + MinShotLengthMS
		}
		if end > start {
			shots = append(shots, ShotBound{StartMS: start, EndMS: end})
		}
	}
	return shots
}

func truncateOutput(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 2048 {
		return text[:2048] + "…(truncated)"
	}
	return text
}
