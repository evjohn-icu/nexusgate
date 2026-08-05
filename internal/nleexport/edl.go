// Package nleexport serializes a fully prepared timeline into a CMX3600 EDL an
// editor can import into an NLE.
//
// This package deliberately knows nothing about how a timeline came to be: it
// performs no I/O, reads no database, and has no notion of repurpose plans,
// shots, or any other model of the footage it describes. It takes the clips it
// is handed and writes text. The boundary exists so the serializer can never
// quietly re-decide an edit — no relinking, no reordering, no
// reinterpretation — because a subtle change to an edit list costs a human
// editor real time before it is ever discovered. Assembling the timeline stays
// with whoever owns the source material.
package nleexport

import (
	"fmt"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/timecode"
)

// placeholderReel is the stable label used when a clip's reel sanitizes to
// nothing. An EDL must name a source, and the label has to be the same every
// time so an unnamed source does not oscillate between names across exports.
const placeholderReel = "REEL"

// Clip is one entry on the timeline.
type Clip struct {
	Reel        string  // short source identifier, see reel naming rules
	SourceInMS  int64   // in point within the source file
	SourceOutMS int64   // out point within the source file, exclusive
	FPS         float64 // frame rate of the source
}

// Timeline is the ordered set of clips to be written as a single EDL.
type Timeline struct {
	Title string
	Clips []Clip
}

// EDL renders the timeline as a CMX3600 edit decision list.
func EDL(t Timeline) (string, error) {
	if len(t.Clips) == 0 {
		return "", fmt.Errorf("%w: timeline has no clips", ErrInvalidTimeline)
	}

	fps := t.Clips[0].FPS
	for i := range t.Clips {
		c := &t.Clips[i]
		if c.FPS <= 0 {
			return "", fmt.Errorf("%w: clip %d has frame rate %v, which is not positive", ErrInvalidTimeline, i+1, c.FPS)
		}
		// CMX3600 carries one FCM line for the whole list, so every clip must
		// agree on the rate. Drop-frame is a pure function of the rate, so
		// agreeing on fps also rules out mixing drop-frame with non-drop; no
		// second check is needed.
		if c.FPS != fps {
			return "", fmt.Errorf("%w: clips disagree on frame rate: clip 1 is %v fps, clip %d is %v fps", ErrInvalidTimeline, fps, i+1, c.FPS)
		}
		if c.SourceInMS < 0 {
			return "", fmt.Errorf("%w: clip %d has negative source in point %d", ErrInvalidTimeline, i+1, c.SourceInMS)
		}
		// An out point at or before the in point has no duration. Writing it
		// anyway yields a plausible-looking list with a zero-length shot, which
		// is harder to notice than the error.
		if c.SourceOutMS <= c.SourceInMS {
			return "", fmt.Errorf("%w: clip %d source out point %d is not after its source in point %d", ErrInvalidTimeline, i+1, c.SourceOutMS, c.SourceInMS)
		}
	}

	reels := assignReels(t.Clips)

	fcm := "FCM: NON-DROP FRAME"
	if timecode.DropFrame(fps) {
		fcm = "FCM: DROP FRAME"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "TITLE: %s\r\n", t.Title)
	fmt.Fprintf(&b, "%s\r\n", fcm)

	// Everything below is frame arithmetic, not millisecond arithmetic, and the
	// difference is not stylistic. For a straight cut the record duration must
	// equal the source duration exactly; an NLE reads any disagreement as a gap
	// or an overlap. Converting each millisecond position independently lets
	// each one round by its own sub-frame phase, so a 50 ms clip can render as
	// two frames of source starting at 0 and one frame of record starting at
	// 25 ms. Converting at the edges and deriving the record position by adding
	// frame counts makes the two sides equal by construction.
	var recFrames int64
	for i := range t.Clips {
		c := &t.Clips[i]
		srcInFrames, err := timecode.Frames(c.SourceInMS, fps)
		if err != nil {
			return "", fmt.Errorf("%w: clip %d source in: %w", ErrInvalidTimeline, i+1, err)
		}
		srcOutFrames, err := timecode.Frames(c.SourceOutMS, fps)
		if err != nil {
			return "", fmt.Errorf("%w: clip %d source out: %w", ErrInvalidTimeline, i+1, err)
		}
		// A range shorter than one frame survives the millisecond check above
		// but has nothing to cut. Refusing beats emitting a zero-length event.
		if srcOutFrames == srcInFrames {
			return "", fmt.Errorf("%w: clip %d spans %dms, which is less than one frame at %v fps", ErrInvalidTimeline, i+1, c.SourceOutMS-c.SourceInMS, fps)
		}
		labels := make([]string, 0, 4)
		for _, frames := range []int64{srcInFrames, srcOutFrames, recFrames, recFrames + (srcOutFrames - srcInFrames)} {
			rendered, err := timecode.FormatFrames(frames, fps)
			if err != nil {
				return "", fmt.Errorf("%w: clip %d: %w", ErrInvalidTimeline, i+1, err)
			}
			labels = append(labels, rendered)
		}
		recFrames += srcOutFrames - srcInFrames
		fmt.Fprintf(&b, "%03d  %-8s  V     C        %s  %s  %s  %s\r\n", i+1, reels[c.Reel], labels[0], labels[1], labels[2], labels[3])
	}
	return b.String(), nil
}

// assignReels gives every distinct source reel a CMX3600 label in first-use
// order. Two different originals that sanitize to the same base must not share
// a label: an EDL that relinks a shot to the wrong file is silent corruption.
// Collisions take a deterministic numeric suffix rather than being resolved in
// any other way, and first-use ordering keeps one source's label constant for
// the whole list and stable across re-exports of the same timeline.
func assignReels(clips []Clip) map[string]string {
	labels := make(map[string]string)
	used := make(map[string]struct{})
	for i := range clips {
		orig := clips[i].Reel
		if _, seen := labels[orig]; seen {
			continue
		}
		base := sanitizeReel(orig)
		if base == "" {
			base = placeholderReel
		}
		name := base
		for k := 2; ; k++ {
			if _, taken := used[name]; !taken {
				break
			}
			suffix := fmt.Sprintf("_%d", k)
			room := 8 - len(suffix)
			if room < 1 {
				room = 1
			}
			if len(base) >= room {
				name = base[:room] + suffix
			} else {
				name = base + suffix
			}
		}
		used[name] = struct{}{}
		labels[orig] = name
	}
	return labels
}

// sanitizeReel maps a source reel to the CMX3600 safe alphabet: uppercase A-Z,
// 0-9 and underscore, capped at 8 characters. The cap is a hard interop limit
// — NLEs truncate longer names, often mid-name — and a truncated name can
// silently collide with another source, which is why disambiguation is a
// separate, later step.
func sanitizeReel(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteByte(byte(r) &^ 0x20)
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteByte(byte(r))
		default:
			b.WriteByte('_')
		}
		if b.Len() == 8 {
			break
		}
	}
	return b.String()
}
