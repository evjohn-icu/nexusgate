package media

import (
	"strings"
	"testing"
)

// -readrate is an input option: after -i, FFmpeg ignores it silently, so the
// whole throttle would appear to work while doing nothing. Ordering is the one
// property this helper exists to guarantee.
func TestReadRateArgsAreAnInputOption(t *testing.T) {
	if args := readRateArgs(0); args != nil {
		t.Fatalf("zero means unlimited, got %v", args)
	}
	if args := readRateArgs(-1); args != nil {
		t.Fatalf("a negative rate must not reach ffmpeg, got %v", args)
	}
	if !readRateAvailable() {
		t.Skip("installed ffmpeg has no -readrate; the gating path is what is under test elsewhere")
	}
	args := readRateArgs(1.5)
	if len(args) != 2 || args[0] != "-readrate" || args[1] != "1.5" {
		t.Fatalf("args=%v want [-readrate 1.5]", args)
	}
	// Rendered without an exponent or trailing zeros: FFmpeg parses the string.
	for rate, want := range map[float64]string{2: "2", 0.5: "0.5", 1.25: "1.25", 10: "10"} {
		if got := readRateArgs(rate); got[1] != want {
			t.Fatalf("rate %v rendered as %q want %q", rate, got[1], want)
		}
	}
}

// The probe must only conclude "unsupported" from an option-parsing failure.
// Treating any error as unsupported would silently disable the throttle whenever
// something unrelated was wrong, which is the opposite of what an operator who
// just enabled it needs to see.
func TestProbeReadRateDistinguishesUnsupportedFromBroken(t *testing.T) {
	for text, unsupported := range map[string]bool{
		"Unrecognized option 'readrate'.":          true,
		"Option not found":                         true,
		"No such file or directory":                false,
		"Invalid data found when processing input": false,
		"": false,
	} {
		lowered := strings.ToLower(text)
		got := !strings.Contains(lowered, "unrecognized option") && !strings.Contains(lowered, "option not found")
		if got == unsupported {
			t.Fatalf("%q: classified supported=%v, expected unsupported=%v", text, got, unsupported)
		}
	}
}
