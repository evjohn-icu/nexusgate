package media

import (
	"context"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// -readrate arrived in FFmpeg 5.1 (2022). Plenty of long-lived installs — Ubuntu
// 22.04, most NAS firmware — still ship 4.x, where passing it makes FFmpeg exit
// on an unrecognized option before it reads a single frame.
//
// That failure mode is the reason this probe exists rather than a documented
// minimum version: nothing marks it domain.Permanent, and isRetryableJobError
// retries by default, so it would be treated as transient and retried through the
// whole backoff chain. Turning on a throttle in the settings page would quietly break
// every derive on that machine. Probing once and dropping the flag instead makes
// the feature a no-op there, which is the honest behaviour for a setting the
// installed FFmpeg cannot honour.
var (
	readRateOnce      sync.Once
	readRateSupported bool
)

func readRateAvailable() bool {
	readRateOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		readRateSupported = probeReadRate(ctx)
		if !readRateSupported {
			slog.Warn("installed ffmpeg does not support -readrate; the read-rate limit will be ignored (FFmpeg 5.1 or newer is required)")
		}
	})
	return readRateSupported
}

// probeReadRate runs the cheapest possible command that still forces option
// parsing. A synthetic one-frame input keeps it off the disk entirely.
func probeReadRate(ctx context.Context) bool {
	command := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-readrate", "1", "-f", "lavfi", "-i", "nullsrc=s=16x16:d=0.01", "-frames:v", "1", "-f", "null", "-")
	output, err := command.CombinedOutput()
	if err == nil {
		return true
	}
	// Distinguish "no such option" from "ffmpeg is missing" or any other failure:
	// only the first means the flag itself is unsupported. Anything else leaves the
	// flag enabled so a real problem surfaces as a real error rather than as a
	// silently dropped setting.
	text := strings.ToLower(string(output))
	return !strings.Contains(text, "unrecognized option") && !strings.Contains(text, "option not found")
}

// readRateArgs renders the FFmpeg input option. It must be placed before -i:
// -readrate is an input option, and after -i it would be silently ignored, which
// is the failure mode this helper exists to prevent.
func readRateArgs(rate float64) []string {
	if rate <= 0 || !readRateAvailable() {
		return nil
	}
	return []string{"-readrate", strconv.FormatFloat(rate, 'f', -1, 64)}
}
