// Package timecode turns a millisecond offset and a frame rate into an SMPTE
// timecode label for EDL and FCPXML export.
//
// The character between the seconds and frames fields is load-bearing: an NLE
// reading a drop-frame timeline whose labels use a colon instead of a
// semicolon silently misplaces every cut in the project, because the two
// formats count frames differently. Exporting the label as a single string
// keeps the separator from drifting out of agreement with the arithmetic.
package timecode

import (
	"fmt"
	"math"
)

// DropFrame reports whether fps is an NTSC drop-frame rate (nominal 30 or 60).
//
// fps arrives already divided: media/process.go's parseRate collapses
// "30000/1001" into 29.97002997002997, so it must never be compared with ==
// against 29.97 or 30000.0/1001.0. A tolerance classifies the exact rational
// and its rounded decimal the same way.
func DropFrame(fps float64) bool {
	return math.Abs(fps-30000.0/1001.0) < 0.01 ||
		math.Abs(fps-60000.0/1001.0) < 0.01
}

// Format renders offsetMS as an SMPTE timecode label at rate fps.
//
// Drop-frame skips only label numbers, never video frames: two labels (or four
// at nominal 60) are omitted at the start of every minute except every tenth
// one, which makes the label track wall-clock time instead of running 108
// frames behind the clock every hour. Non-drop timecode just counts every
// frame.
//
// Hours deliberately exceed 23 instead of wrapping to 00:00:00. A wrapped
// label is a plausible-looking lie in an exported edit list; an out-of-range
// hour at least reads as wrong and gets caught before it reaches an editor.
func Format(offsetMS int64, fps float64) (string, error) {
	total, err := Frames(offsetMS, fps)
	if err != nil {
		return "", err
	}
	return FormatFrames(total, fps)
}

// Frames converts a millisecond offset to a frame count at fps.
//
// It is exported because an edit list is a frame-domain document and has to
// stay in that domain to be correct. A caller that keeps positions in
// milliseconds and converts each one independently gets its rounding decided
// by each offset's own sub-frame phase: the same duration can then render as
// two frames of source and one frame of record, which an NLE imports as a gap
// or an overlap. Converting once at the edges and doing all arithmetic on the
// result removes that whole class of error.
func Frames(offsetMS int64, fps float64) (int64, error) {
	if fps <= 0 {
		return 0, fmt.Errorf("timecode: frame rate %v is not positive", fps)
	}
	if offsetMS < 0 {
		return 0, fmt.Errorf("timecode: negative offset %d has no valid label", offsetMS)
	}
	return int64(math.Round(float64(offsetMS) / 1000.0 * fps)), nil
}

// FormatFrames renders a frame count as an SMPTE timecode label at rate fps.
func FormatFrames(total int64, fps float64) (string, error) {
	if fps <= 0 {
		return "", fmt.Errorf("timecode: frame rate %v is not positive", fps)
	}
	if total < 0 {
		return "", fmt.Errorf("timecode: negative frame count %d has no valid label", total)
	}

	nominal := int64(math.Round(fps))
	if DropFrame(fps) {
		return label(total+skipCount(total, nominal), nominal, ';'), nil
	}
	return label(total, nominal, ':'), nil
}

// skipCount returns how many drop-frame label numbers were omitted before
// total, by frame number. It leans on one fact: a ten-minute block at the
// nominal rate holds exactly as many labels as ten minutes of NTSC holds real
// frames, which is the realignment the whole scheme is built on.
func skipCount(total, nominal int64) int64 {
	dropPerMinute := nominal / 15 // 2 at nominal 30, 4 at nominal 60
	framesPerMinute := nominal*60 - dropPerMinute
	// Nine of the ten minutes in a block skip labels; the first does not. So
	// the block holds a full ten minutes of labels minus nine skips — not ten
	// short minutes. Deriving it as framesPerMinute*10 undercounts by one
	// skip per block (17962 instead of 17982 at nominal 30), which stays
	// invisible at 0s, 60s, 10min and 1h and then drifts a further two frames
	// every ten minutes: ~3 frames off by the third hour of a long recording.
	framesPerTenMinutes := nominal*600 - dropPerMinute*9

	blocks := total / framesPerTenMinutes
	within := total % framesPerTenMinutes

	skipped := dropPerMinute * 9 * blocks
	if within > dropPerMinute {
		skipped += dropPerMinute * ((within - dropPerMinute) / framesPerMinute)
	}
	return skipped
}

// label formats a non-drop frame count at the nominal rate. Breaking the count
// down with the nominal integer rate is what keeps the label aligned with the
// NLE's own frame counter.
func label(total, nominal int64, sep byte) string {
	hours := total / (nominal * 3600)
	minutes := (total / (nominal * 60)) % 60
	seconds := (total / nominal) % 60
	frames := total % nominal
	return fmt.Sprintf("%02d:%02d:%02d%c%02d", hours, minutes, seconds, sep, frames)
}
