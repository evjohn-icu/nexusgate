package timecode

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestFormat(t *testing.T) {
	tests := []struct {
		name     string
		offsetMS int64
		fps      float64
		want     string
	}{
		{"zero at 25", 0, 25, "00:00:00:00"},
		{"one second at 25", 1000, 25, "00:00:01:00"},
		{"one hour at 25", 3600000, 25, "01:00:00:00"},
		{"hours exceed 23 rather than wrapping", 86401000, 25, "24:00:01:00"},
		{"zero at 29.97 still semicolon", 0, 30000.0 / 1001.0, "00:00:00;00"},
		{"one second at 59.94", 1000, 60000.0 / 1001.0, "00:00:01;00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Format(tt.offsetMS, tt.fps)
			if err != nil {
				t.Fatalf("Format(%d, %v) error: %v", tt.offsetMS, tt.fps, err)
			}
			if got != tt.want {
				t.Errorf("Format(%d, %v) = %q, want %q", tt.offsetMS, tt.fps, got, tt.want)
			}
		})
	}
}

func TestFormatDropFrame(t *testing.T) {
	ntsc30 := 30000.0 / 1001.0
	ntsc60 := 60000.0 / 1001.0

	tests := []struct {
		name     string
		offsetMS int64
		fps      float64
		want     string
	}{
		{"29.97 at exactly 60s has already skipped labels", 60000, ntsc30, "00:00:59;28"},
		{"59.94 at exactly 60s has already skipped labels", 60000, ntsc60, "00:00:59;56"},
		{"29.97 at exactly 10 minutes", 600000, ntsc30, "00:10:00;00"},
		{"59.94 at exactly 10 minutes", 600000, ntsc60, "00:10:00;00"},
		{"29.97 one hour stays nominal", 3600000, ntsc30, "01:00:00;00"},
		{"29.97 minute boundary skips to ;02", 60060, ntsc30, "00:01:00;02"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Format(tt.offsetMS, tt.fps)
			if err != nil {
				t.Fatalf("Format(%d, %v) error: %v", tt.offsetMS, tt.fps, err)
			}
			if got != tt.want {
				t.Errorf("Format(%d, %v) = %q, want %q", tt.offsetMS, tt.fps, got, tt.want)
			}
		})
	}
}

// TestDropFrameRealignsAtTenMinutes pins down the tenth-minute exception: at a
// ten-minute boundary the drop-frame and non-drop label numbers must be
// identical, differing only in the separator.
func TestDropFrameRealignsAtTenMinutes(t *testing.T) {
	df, err := Format(600000, 30000.0/1001.0)
	if err != nil {
		t.Fatal(err)
	}
	nd, err := Format(600000, 30)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(df, ";", ":") != nd {
		t.Errorf("at 10 minutes drop-frame %q must equal non-drop %q", df, nd)
	}
}

func TestDropFrame(t *testing.T) {
	tests := []struct {
		name string
		fps  float64
		want bool
	}{
		{"nominal 30 is not NTSC drop", 30, false},
		{"PAL 25 is not NTSC drop", 25, false},
		{"film 24 is not NTSC drop", 24, false},
		{"29.97 is drop", 30000.0 / 1001.0, true},
		{"59.94 is drop", 60000.0 / 1001.0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DropFrame(tt.fps); got != tt.want {
				t.Errorf("DropFrame(%v) = %v, want %v", tt.fps, got, tt.want)
			}
		})
	}
}

// A ten-minute drop-frame block holds a full ten minutes of labels minus the
// nine skips inside it — 17982 real frames at nominal 30, not ten
// short-by-two minutes (17962). The difference is invisible at 0s, 60s, 10min
// and 1h, which is exactly where a hand-picked test table looks, and only
// shows up between minute boundaries. These offsets sit at the first
// divergence and just past it; each expectation is the textbook SMPTE
// conversion (d = total/17982, m = total%17982, adjusted = total + 18d +
// 2*((m-2)/1798)) computed by hand rather than read off this implementation.
func TestFormatDropFrameBetweenMinuteBoundaries(t *testing.T) {
	const ntsc30 = 30000.0 / 1001.0
	tests := []struct {
		offsetMS int64
		want     string
	}{
		{659683, "00:10:59;19"},
		// Eleven minutes of wall clock does NOT read 00:11:00;00: correction
		// happens in two-frame steps at minute boundaries, so the label sits
		// just behind between them. Pinned because it looks like a bug.
		{660000, "00:10:59;28"},
		// Second ten-minute block, so an error in the block size accumulates
		// rather than cancelling — this is the row the 17962 formula fails.
		{1259683, "00:20:59;19"},
	}
	for _, tt := range tests {
		got, err := Format(tt.offsetMS, ntsc30)
		if err != nil {
			t.Fatalf("Format(%d, 29.97): %v", tt.offsetMS, err)
		}
		if got != tt.want {
			t.Errorf("Format(%d, 29.97) = %q, want %q", tt.offsetMS, got, tt.want)
		}
	}
}

// The whole point of drop-frame is that the label tracks wall clock. Correction
// happens in discrete two-frame steps at minute boundaries, so the label runs
// ahead between them and snaps back — bounded by the skip size, never
// accumulating. Asserting the bound catches a conversion that silently drifts
// with duration, which a table of round-numbered offsets cannot.
func TestDropFrameLabelTracksWallClockWithinSkipSize(t *testing.T) {
	const ntsc30 = 30000.0 / 1001.0
	const maxDriftFrames = 3.0 // two skipped labels plus one frame of rounding
	for ms := int64(0); ms <= 4*3600*1000; ms += 331 {
		got, err := Format(ms, ntsc30)
		if err != nil {
			t.Fatal(err)
		}
		var hh, mm, ss, ff int
		if _, err := fmt.Sscanf(got, "%02d:%02d:%02d;%02d", &hh, &mm, &ss, &ff); err != nil {
			t.Fatalf("unparsable drop-frame label %q at %dms", got, ms)
		}
		labelSeconds := float64(hh*3600+mm*60+ss) + float64(ff)/30.0
		if drift := math.Abs(labelSeconds - float64(ms)/1000.0); drift*30 > maxDriftFrames {
			t.Fatalf("label %s at %dms drifts %.3fs (%.2f frames) from wall clock", got, ms, drift, drift*30)
		}
	}
}

func TestFormatErrors(t *testing.T) {
	tests := []struct {
		name     string
		offsetMS int64
		fps      float64
	}{
		{"zero frame rate", 0, 0},
		{"negative frame rate", 0, -1},
		{"negative offset", -1, 25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := Format(tt.offsetMS, tt.fps); err == nil {
				t.Errorf("Format(%d, %v) = %q, want error", tt.offsetMS, tt.fps, got)
			}
		})
	}
}
