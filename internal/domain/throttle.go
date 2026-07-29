package domain

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PipelineThrottle bounds how hard derive work drives the source disk.
//
// The pipeline is deliberately sequential, so this is not about concurrency:
// one FFmpeg process reading a 4K file as fast as the bus allows is enough to
// saturate a mechanical disk or a NAS link for as long as the queue is busy,
// and a full library scan keeps it there for hours. The two levers that
// actually reduce sustained load are capping the input read rate and pausing
// between jobs; both are off by default so an existing install behaves exactly
// as before until someone opts in.
type PipelineThrottle struct {
	// ReadRate caps FFmpeg's input read speed as a multiple of realtime
	// playback (1.0 reads a 10-minute clip over 10 minutes). Zero means
	// unlimited. Only whole-file reads honour it — thumbnail extraction reads a
	// single frame, where a rate cap would only slow the seek.
	ReadRate float64 `json:"read_rate"`
	// CooldownSeconds is the pause after each finished job. It is what turns a
	// multi-hour scan from continuous load into duty-cycled load.
	CooldownSeconds int `json:"cooldown_seconds"`

	// OffPeakEnabled gates the deferral window below. The window is evaluated
	// in the Hub's local time, because "凌晨" means the operator's night, not UTC.
	OffPeakEnabled bool `json:"off_peak_enabled"`
	// OffPeakStart and OffPeakEnd are "HH:MM". A window whose end is not after
	// its start wraps past midnight (22:00–06:00), which is the common case.
	OffPeakStart string `json:"off_peak_start"`
	OffPeakEnd   string `json:"off_peak_end"`

	// DeferAboveBytes holds back assets larger than this until the window is
	// open. Zero defers nothing. Size is the lever rather than duration because
	// bytes read is what the disk feels.
	DeferAboveBytes int64 `json:"defer_above_bytes"`
	// ImmediateMaxBytes exempts assets at or below this size from both the
	// deferral and the rate cap. A phone clip costs almost nothing to derive,
	// and throttling it only makes the library feel broken while adding no
	// meaningful relief.
	ImmediateMaxBytes int64 `json:"immediate_max_bytes"`
}

// LeaseFilter narrows what the lease predicate is willing to hand out. It is a
// filter on the query rather than a check after leasing because leasing
// increments attempt_count: a deferred job that were leased and then put back
// would burn an attempt every time the queue was polled, and would be
// permanently failed long before its window ever opened.
type LeaseFilter struct {
	// MaxAssetBytes skips jobs whose asset is larger than this. Zero means no
	// ceiling. Jobs with no asset are never filtered — they have no size.
	MaxAssetBytes int64
}

// DefaultPipelineThrottle is unthrottled. Turning this on by default would
// silently slow every existing install after an upgrade.
func DefaultPipelineThrottle() PipelineThrottle {
	return PipelineThrottle{OffPeakStart: "01:00", OffPeakEnd: "07:00"}
}

const maxThrottleCooldownSeconds = 3600

func (t PipelineThrottle) Validate() error {
	if t.ReadRate < 0 {
		return fmt.Errorf("read_rate must not be negative")
	}
	// Below roughly a quarter of realtime a long clip takes longer to derive
	// than to watch several times over, which reads as a hang rather than as
	// throttling.
	if t.ReadRate > 0 && t.ReadRate < 0.25 {
		return fmt.Errorf("read_rate %.2f is below the 0.25x floor; use 0 for unlimited", t.ReadRate)
	}
	if t.CooldownSeconds < 0 {
		return fmt.Errorf("cooldown_seconds must not be negative")
	}
	if t.CooldownSeconds > maxThrottleCooldownSeconds {
		return fmt.Errorf("cooldown_seconds %d exceeds the %d second limit", t.CooldownSeconds, maxThrottleCooldownSeconds)
	}
	if t.DeferAboveBytes < 0 || t.ImmediateMaxBytes < 0 {
		return fmt.Errorf("size thresholds must not be negative")
	}
	// Otherwise a file could be both "always immediate" and "window only", and
	// which rule won would be an implementation detail rather than a decision.
	if t.DeferAboveBytes > 0 && t.ImmediateMaxBytes > t.DeferAboveBytes {
		return fmt.Errorf("immediate_max_bytes (%d) must not exceed defer_above_bytes (%d)", t.ImmediateMaxBytes, t.DeferAboveBytes)
	}
	if _, err := parseClock(t.OffPeakStart); t.OffPeakStart != "" && err != nil {
		return fmt.Errorf("off_peak_start: %w", err)
	}
	if _, err := parseClock(t.OffPeakEnd); t.OffPeakEnd != "" && err != nil {
		return fmt.Errorf("off_peak_end: %w", err)
	}
	if t.OffPeakEnabled {
		start, err := parseClock(t.OffPeakStart)
		if err != nil {
			return fmt.Errorf("off_peak_start: %w", err)
		}
		end, err := parseClock(t.OffPeakEnd)
		if err != nil {
			return fmt.Errorf("off_peak_end: %w", err)
		}
		if start == end {
			return fmt.Errorf("off-peak window is empty: start and end are both %s", t.OffPeakStart)
		}
	}
	return nil
}

// OffPeakOpenAt reports whether the deferral window is open. A disabled window
// is treated as always open so that turning the feature off releases held work
// immediately rather than stranding it.
func (t PipelineThrottle) OffPeakOpenAt(now time.Time) bool {
	if !t.OffPeakEnabled {
		return true
	}
	start, err := parseClock(t.OffPeakStart)
	if err != nil {
		return true
	}
	end, err := parseClock(t.OffPeakEnd)
	if err != nil {
		return true
	}
	minute := now.Hour()*60 + now.Minute()
	if start < end {
		return minute >= start && minute < end
	}
	// Wrapped past midnight: inside means after the start or before the end.
	return minute >= start || minute < end
}

// MaxAssetBytesAt is the size ceiling the lease predicate should apply right
// now. Zero means no ceiling.
func (t PipelineThrottle) MaxAssetBytesAt(now time.Time) int64 {
	if t.DeferAboveBytes <= 0 || t.OffPeakOpenAt(now) {
		return 0
	}
	return t.DeferAboveBytes
}

// ReadRateFor is the rate cap to apply to one asset, after the small-file
// exemption.
func (t PipelineThrottle) ReadRateFor(sizeBytes int64) float64 {
	if t.ReadRate <= 0 {
		return 0
	}
	if t.ImmediateMaxBytes > 0 && sizeBytes > 0 && sizeBytes <= t.ImmediateMaxBytes {
		return 0
	}
	return t.ReadRate
}

// CooldownAt is the pause to take after a job. Inside the off-peak window there
// is nobody to protect, so the pause is skipped and the queue drains at full
// speed — that is the point of having a window at all.
func (t PipelineThrottle) CooldownAt(now time.Time) time.Duration {
	if t.CooldownSeconds <= 0 {
		return 0
	}
	if t.OffPeakEnabled && t.OffPeakOpenAt(now) {
		return 0
	}
	return time.Duration(t.CooldownSeconds) * time.Second
}

// NextOffPeakStart is used to report when held work will run, so the progress
// page can say "01:00" instead of leaving the queue looking stuck.
func (t PipelineThrottle) NextOffPeakStart(now time.Time) (time.Time, bool) {
	if !t.OffPeakEnabled {
		return time.Time{}, false
	}
	start, err := parseClock(t.OffPeakStart)
	if err != nil {
		return time.Time{}, false
	}
	candidate := time.Date(now.Year(), now.Month(), now.Day(), start/60, start%60, 0, 0, now.Location())
	if !candidate.After(now) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate, true
}

// parseClock accepts "HH:MM" and returns minutes past midnight. Deliberately
// strict: a silently-misread window would throttle at the wrong hours, which is
// far harder to notice than a rejected value.
func parseClock(value string) (int, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%q is not HH:MM", value)
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, fmt.Errorf("%q has an out-of-range hour", value)
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("%q has an out-of-range minute", value)
	}
	return hour*60 + minute, nil
}
