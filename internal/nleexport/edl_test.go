package nleexport

import (
	"fmt"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/timecode"
)

// reelOf pulls the 8-column reel field out of an event line. Event lines place
// the reel at fixed columns: three event digits, two spaces, then the reel.
func reelOf(line string) string {
	return strings.TrimSpace(line[5:13])
}

// For a straight cut the record duration must equal the source duration frame
// for frame, and each event's record in must be the previous event's record
// out. An NLE reads any disagreement as a gap or an overlap — silently, since
// the list still parses. This is a property rather than a fixed expectation
// because the failure it guards against depends on the sub-frame phase of each
// offset: converting positions to frames independently in the millisecond
// domain made a 50ms clip span two frames of source and one of record, and no
// round-numbered example shows it.
func TestRecordTimesChainAndMatchSourceDurations(t *testing.T) {
	// The first entry is the case that actually caught the millisecond-domain
	// bug: at 30fps a 25ms clip followed by a 50ms clip put the second event's
	// record start on a half-frame boundary, so its source spanned two frames
	// and its record one. Kept explicitly because generated offsets happen to
	// miss that phase.
	known := []Clip{
		{Reel: "A", SourceInMS: 0, SourceOutMS: 25, FPS: 30},
		{Reel: "B", SourceInMS: 0, SourceOutMS: 50, FPS: 30},
	}
	if out, err := EDL(Timeline{Title: "known", Clips: known}); err != nil {
		t.Fatal(err)
	} else {
		checkChain(t, out, 30)
	}

	for _, fps := range []float64{24, 25, 30, 30000.0 / 1001.0, 60000.0 / 1001.0} {
		clips := make([]Clip, 0, 12)
		for i := int64(0); i < 12; i++ {
			// Deliberately off-grid start and end points, so the two sides
			// round from different phases.
			start := i*997 + i*7
			clips = append(clips, Clip{Reel: fmt.Sprintf("R%d", i), SourceInMS: start, SourceOutMS: start + 25 + i*13, FPS: fps})
		}
		out, err := EDL(Timeline{Title: "chain", Clips: clips})
		if err != nil {
			t.Fatalf("fps %v: %v", fps, err)
		}
		checkChain(t, out, fps)
	}
}

func checkChain(t *testing.T, out string, fps float64) {
	t.Helper()
	var previousOut int64
	for _, line := range strings.Split(out, "\r\n") {
		fields := strings.Fields(line)
		if len(fields) != 8 {
			continue
		}
		srcIn, srcOut := decodeLabel(t, fields[4], fps), decodeLabel(t, fields[5], fps)
		recIn, recOut := decodeLabel(t, fields[6], fps), decodeLabel(t, fields[7], fps)
		if srcOut-srcIn != recOut-recIn {
			t.Errorf("fps %v event %s: source spans %d frames, record spans %d", fps, fields[0], srcOut-srcIn, recOut-recIn)
		}
		if recIn != previousOut {
			t.Errorf("fps %v event %s: record in %d does not continue from previous record out %d", fps, fields[0], recIn, previousOut)
		}
		previousOut = recOut
	}
}

// decodeLabel converts a rendered label back to a frame count, undoing the
// drop-frame label skips so the result is comparable across events.
func decodeLabel(t *testing.T, value string, fps float64) int64 {
	t.Helper()
	var hh, mm, ss, ff int64
	separator := ':'
	if timecode.DropFrame(fps) {
		separator = ';'
	}
	if _, err := fmt.Sscanf(strings.Replace(value, string(separator), ":", 1), "%d:%d:%d:%d", &hh, &mm, &ss, &ff); err != nil {
		t.Fatalf("unparsable label %q: %v", value, err)
	}
	nominal := int64(fps + 0.5)
	labelled := ((hh*60+mm)*60+ss)*nominal + ff
	if !timecode.DropFrame(fps) {
		return labelled
	}
	// Undo the skipped label numbers: every minute except each tenth drops
	// nominal/15 of them.
	minutes := hh*60 + mm
	return labelled - (nominal/15)*(minutes-minutes/10)
}

func TestEDLSingleClip(t *testing.T) {
	got, err := EDL(Timeline{
		Title: "My Plan",
		Clips: []Clip{{Reel: "AX001", SourceInMS: 0, SourceOutMS: 10000, FPS: 25}},
	})
	if err != nil {
		t.Fatalf("EDL returned error: %v", err)
	}
	want := "TITLE: My Plan\r\n" +
		"FCM: NON-DROP FRAME\r\n" +
		"001  AX001     V     C        00:00:00:00  00:00:10:00  00:00:00:00  00:00:10:00\r\n"
	if got != want {
		t.Fatalf("EDL mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestEDLThreeClipsChain(t *testing.T) {
	got, err := EDL(Timeline{
		Title: "Chain",
		Clips: []Clip{
			{Reel: "A", SourceInMS: 0, SourceOutMS: 2000, FPS: 25},
			{Reel: "B", SourceInMS: 5000, SourceOutMS: 7000, FPS: 25},
			{Reel: "C", SourceInMS: 0, SourceOutMS: 4000, FPS: 25},
		},
	})
	if err != nil {
		t.Fatalf("EDL returned error: %v", err)
	}
	want := "TITLE: Chain\r\n" +
		"FCM: NON-DROP FRAME\r\n" +
		"001  A         V     C        00:00:00:00  00:00:02:00  00:00:00:00  00:00:02:00\r\n" +
		"002  B         V     C        00:00:05:00  00:00:07:00  00:00:02:00  00:00:04:00\r\n" +
		"003  C         V     C        00:00:00:00  00:00:04:00  00:00:04:00  00:00:08:00\r\n"
	if got != want {
		t.Fatalf("EDL mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestEDLReelCollision(t *testing.T) {
	got, err := EDL(Timeline{
		Title: "Collide",
		Clips: []Clip{
			{Reel: "foo/bar", SourceInMS: 0, SourceOutMS: 1000, FPS: 25},
			{Reel: "foo.bar", SourceInMS: 0, SourceOutMS: 1000, FPS: 25},
		},
	})
	if err != nil {
		t.Fatalf("EDL returned error: %v", err)
	}
	lines := strings.Split(got, "\r\n")
	if len(lines) != 5 {
		t.Fatalf("unexpected line count %d:\n%s", len(lines), got)
	}
	first, second := reelOf(lines[2]), reelOf(lines[3])
	if first == second {
		t.Fatalf("distinct sources collapsed to the same reel %q", first)
	}
	if first != "FOO_BAR" {
		t.Fatalf("first colliding reel = %q, want FOO_BAR", first)
	}
	if second != "FOO_BA_2" {
		t.Fatalf("second colliding reel = %q, want FOO_BA_2", second)
	}
}

func TestEDLReelSanitize(t *testing.T) {
	got, err := EDL(Timeline{
		Title: "Sanitize",
		Clips: []Clip{
			{Reel: "拍摄 现场 Footage", SourceInMS: 0, SourceOutMS: 1000, FPS: 25},
			{Reel: "my reel", SourceInMS: 0, SourceOutMS: 1000, FPS: 25},
		},
	})
	if err != nil {
		t.Fatalf("EDL returned error: %v", err)
	}
	lines := strings.Split(got, "\r\n")
	if len(lines) != 5 {
		t.Fatalf("unexpected line count %d:\n%s", len(lines), got)
	}
	if first := reelOf(lines[2]); first != "______FO" {
		t.Fatalf("CJK reel = %q, want ______FO", first)
	}
	if second := reelOf(lines[3]); second != "MY_REEL" {
		t.Fatalf("mixed-case reel = %q, want MY_REEL", second)
	}
}

func TestEDLReelTruncation(t *testing.T) {
	got, err := EDL(Timeline{
		Title: "Long",
		Clips: []Clip{{Reel: "MyLongReelName123", SourceInMS: 0, SourceOutMS: 1000, FPS: 25}},
	})
	if err != nil {
		t.Fatalf("EDL returned error: %v", err)
	}
	lines := strings.Split(got, "\r\n")
	if reel := reelOf(lines[2]); reel != "MYLONGRE" {
		t.Fatalf("long reel = %q, want MYLONGRE", reel)
	}
}

func TestEDLReelPlaceholder(t *testing.T) {
	got, err := EDL(Timeline{
		Title: "Empty",
		Clips: []Clip{{Reel: "", SourceInMS: 0, SourceOutMS: 1000, FPS: 25}},
	})
	if err != nil {
		t.Fatalf("EDL returned error: %v", err)
	}
	lines := strings.Split(got, "\r\n")
	if reel := reelOf(lines[2]); reel != "REEL" {
		t.Fatalf("empty reel = %q, want REEL", reel)
	}
}

func TestEDLDropFrame(t *testing.T) {
	fps := 30000.0 / 1001.0
	got, err := EDL(Timeline{
		Title: "Drop",
		Clips: []Clip{{Reel: "AX001", SourceInMS: 0, SourceOutMS: 2000, FPS: fps}},
	})
	if err != nil {
		t.Fatalf("EDL returned error: %v", err)
	}
	srcIn, _ := timecode.Format(0, fps)
	srcOut, _ := timecode.Format(2000, fps)
	recIn, _ := timecode.Format(0, fps)
	recOut, _ := timecode.Format(2000, fps)
	for _, tc := range []string{srcIn, srcOut, recIn, recOut} {
		if !strings.Contains(tc, ";") {
			t.Fatalf("drop-frame label %q lacks ';' separator", tc)
		}
	}
	want := "TITLE: Drop\r\n" +
		"FCM: DROP FRAME\r\n" +
		fmt.Sprintf("001  AX001     V     C        %s  %s  %s  %s\r\n", srcIn, srcOut, recIn, recOut)
	if got != want {
		t.Fatalf("EDL mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestEDLErrors(t *testing.T) {
	tests := []struct {
		name     string
		timeline Timeline
		wantErr  string
	}{
		{"no clips", Timeline{Title: "x"}, "no clips"},
		{"source out equals source in", Timeline{Clips: []Clip{{Reel: "A", SourceInMS: 5000, SourceOutMS: 5000, FPS: 25}}}, "source out point"},
		{"source out before source in", Timeline{Clips: []Clip{{Reel: "A", SourceInMS: 5000, SourceOutMS: 1000, FPS: 25}}}, "source out point"},
		{"negative source in", Timeline{Clips: []Clip{{Reel: "A", SourceInMS: -1, SourceOutMS: 0, FPS: 25}}}, "negative source in"},
		{"zero frame rate", Timeline{Clips: []Clip{{Reel: "A", SourceInMS: 0, SourceOutMS: 1000, FPS: 0}}}, "frame rate"},
		{"negative frame rate", Timeline{Clips: []Clip{{Reel: "A", SourceInMS: 0, SourceOutMS: 1000, FPS: -25}}}, "frame rate"},
		{"mixed frame rates", Timeline{Clips: []Clip{
			{Reel: "A", SourceInMS: 0, SourceOutMS: 1000, FPS: 25},
			{Reel: "B", SourceInMS: 0, SourceOutMS: 1000, FPS: 24},
		}}, "disagree"},
		{"mixed drop and non-drop", Timeline{Clips: []Clip{
			{Reel: "A", SourceInMS: 0, SourceOutMS: 1000, FPS: 30000.0 / 1001.0},
			{Reel: "B", SourceInMS: 0, SourceOutMS: 1000, FPS: 25},
		}}, "disagree"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EDL(tc.timeline)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
