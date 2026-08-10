package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

func NormalizeMetadata(probe FFProbeResult, exif map[string]any) domain.MediaMetadata {
	rawProbe, _ := json.Marshal(probe)
	rawExif, _ := json.Marshal(exif)
	m := domain.MediaMetadata{FFProbeRaw: string(rawProbe), ExifToolRaw: string(rawExif)}
	if v, err := strconv.ParseFloat(probe.Format.Duration, 64); err == nil {
		m.DurationMS = int64(v * 1000)
	}
	for _, s := range probe.Streams {
		switch s.CodecType {
		case "video":
			if m.VideoCodec == "" {
				m.VideoCodec = s.CodecName
				m.Width = s.Width
				m.Height = s.Height
				m.FPS = parseRate(s.AvgFrameRate)
				m.PixelFormat = s.PixFmt
				m.ColorSpace = s.ColorSpace
				m.ColorTransfer = s.ColorTransfer
				m.ColorPrimaries = s.ColorPrimaries
				m.BitDepth = parseBitDepth(s.BitsPerRawSample, s.PixFmt)
			}
		case "audio":
			if m.AudioCodec == "" {
				m.AudioCodec = s.CodecName
				m.HasAudio = true
			}
		}
	}
	profile := RecognizeCaptureProfile(probe.Format.Filename, captureProbeFields(probe), captureExifFields(exif))
	metadataGroups := captureMetadataGroups(probe, exif)
	if profile.Vendor == "" {
		profile.Vendor = firstMetadataString(metadataGroups, "CaptureVendor", "Vendor", "Manufacturer", "Make")
	}
	if profile.RawFormat == "" {
		profile.RawFormat = firstMetadataString(metadataGroups, "RawFormat", "RawCodec", "CaptureFormat")
	}
	if profile.ColorProfile == "" {
		profile.ColorProfile = firstMetadataString(metadataGroups, "ColorProfile", "ColorProfileName", "PictureProfile")
	}
	plan := PreviewRenderPlanFor(probe, exif, probe.Format.Filename)
	if isLogCaptureProfile(profile.ColorProfile) && (plan.SourceColor == SourceColorSDR || plan.SourceColor == SourceColorUnknown) {
		plan = SelectPreviewRenderPlan(SourceColorLogUnknown)
	}
	m.SourceColor = string(plan.SourceColor)
	m.PreviewRenderMode = string(plan.Mode)
	m.PreviewAvailable = plan.Available
	m.PreviewRequiresLUT = plan.RequiresLUT
	m.PreviewStatus = previewStatus(plan)
	m.CaptureVendor = profile.Vendor
	m.ColorProfile = profile.ColorProfile
	m.RawFormat = profile.RawFormat
	switch {
	case m.Width == m.Height && m.Width > 0:
		m.Orientation = "square"
	case m.Width > m.Height:
		m.Orientation = "landscape"
	case m.Height > 0:
		m.Orientation = "portrait"
	default:
		m.Orientation = "unknown"
	}
	m.CameraMake = firstMetadataString(metadataGroups, "Make", "Manufacturer", "CameraMake", "CameraManufacturer")
	m.CameraModel = firstMetadataString(metadataGroups, "Model", "CameraModelName", "CameraModel", "CameraModelDescription")
	m.CameraSerial = firstMetadataString(metadataGroups, "SerialNumber", "CameraSerialNumber", "InternalSerialNumber", "DeviceSerialNumber", "CameraSerial")
	m.LensModel = firstMetadataString(metadataGroups, "LensModel", "LensName", "Lens", "LensID", "CameraLensModel")
	m.Reel = firstMetadataString(metadataGroups, "Reel", "ReelName", "ReelNumber", "RollName", "TapeName")
	m.Clip = firstMetadataString(metadataGroups, "Clip", "ClipName", "ClipNumber", "CameraClipName")
	m.SourceTimecode = firstMetadataString(metadataGroups, "SourceTimecode", "Timecode", "TimeCode", "StartTimecode", "SMPTETimecode")
	if m.CaptureVendor == "" {
		m.CaptureVendor = m.CameraMake
	}
	if capturedAt, ok := captureTime(exif); ok {
		m.CapturedAt = &capturedAt
		m.CaptureTimeSource, m.CaptureTimeConfidence = captureTimeProvenance(exif)
	} else if capturedAt, ok := captureTimeFromProbe(probe); ok {
		m.CapturedAt = &capturedAt
		m.CaptureTimeSource = "embedded_probe"
		m.CaptureTimeConfidence = 0.75
	} else if capturedAt, ok := captureTimeValue(exif["FileModifyDate"]); ok {
		m.CapturedAt = &capturedAt
		m.CaptureTimeSource, m.CaptureTimeConfidence = "filesystem", 0.25
	}
	if lat, lon, ok := completeCoordinates(exif); ok {
		m.Latitude, m.Longitude = &lat, &lon
		m.LocationSource, m.LocationPrecision = "embedded_exif", "exact"
	} else if lat, lon, ok := probeCoordinates(probe); ok {
		m.Latitude, m.Longitude = &lat, &lon
		m.LocationSource, m.LocationPrecision = "embedded_probe", "exact"
	}
	return m
}

func isLogCaptureProfile(profile string) bool {
	switch profile {
	case "Apple Log", "D-Log", "S-Log", "C-Log", "GP-Log", "V-Log", "N-Log", "F-Log", "Log3G10", "LogC", "Blackmagic Film":
		return true
	default:
		return false
	}
}

func previewStatus(plan PreviewRenderPlan) string {
	switch {
	case plan.SourceColor == SourceColorRAW:
		return "renderer_required"
	case plan.RequiresLUT:
		return "lut_required"
	case plan.Available:
		return "ready"
	default:
		return "unavailable"
	}
}

type metadataField struct {
	key   string
	value string
}

func captureMetadataGroups(probe FFProbeResult, exif map[string]any) [][]metadataField {
	return [][]metadataField{exifMetadataFields(exif), probeMetadataFields(probe)}
}

func exifMetadataFields(exif map[string]any) []metadataField {
	keys := make([]string, 0, len(exif))
	for key := range exif {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fields := make([]metadataField, 0, len(keys))
	for _, key := range keys {
		value := strings.TrimSpace(fmt.Sprint(exif[key]))
		if value == "" || value == "<nil>" {
			continue
		}
		fields = append(fields, metadataField{key: key, value: value})
	}
	return fields
}

func probeMetadataFields(probe FFProbeResult) []metadataField {
	fields := make([]metadataField, 0, len(probe.Format.Tags)+len(probe.Streams))
	fields = append(fields, sortedTagFields(probe.Format.Tags)...)
	for _, stream := range probe.Streams {
		fields = append(fields, sortedTagFields(stream.Tags)...)
	}
	return fields
}

func sortedTagFields(tags map[string]string) []metadataField {
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fields := make([]metadataField, 0, len(keys))
	for _, key := range keys {
		value := strings.TrimSpace(tags[key])
		if value != "" {
			fields = append(fields, metadataField{key: key, value: value})
		}
	}
	return fields
}

func firstMetadataString(groups [][]metadataField, keys ...string) string {
	for _, suffixMatch := range []bool{false, true} {
		for _, group := range groups {
			for _, key := range keys {
				want := compactMetadataKey(key)
				if want == "" {
					continue
				}
				for _, field := range group {
					got := compactMetadataKey(field.key)
					if got == want || (suffixMatch && strings.HasSuffix(got, want)) {
						return field.value
					}
				}
			}
		}
	}
	return ""
}

func compactMetadataKey(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func captureProbeFields(probe FFProbeResult) []string {
	fields := []string{"format=" + probe.Format.FormatName}
	for key, value := range probe.Format.Tags {
		fields = append(fields, key+"="+value)
	}
	for _, stream := range probe.Streams {
		fields = append(fields,
			"codec_name="+stream.CodecName,
			"codec_tag_string="+stream.CodecTagString,
			"profile="+stream.Profile,
			"color_transfer="+stream.ColorTransfer,
			"color_space="+stream.ColorSpace,
			"color_primaries="+stream.ColorPrimaries,
		)
		for key, value := range stream.Tags {
			fields = append(fields, key+"="+value)
		}
	}
	return fields
}

func captureExifFields(exif map[string]any) []string {
	fields := make([]string, 0, len(exif))
	for key, value := range exif {
		fields = append(fields, key+"="+fmt.Sprint(value))
	}
	return fields
}

// captureTime deliberately prefers camera/QuickTime creation fields over
// filesystem timestamps. A timestamp without an explicit offset is retained
// as UTC for the legacy MediaMetadata field; CaptureContext records its source
// and timezone confidence before it is used for automatic session grouping.
func captureTime(exif map[string]any) (time.Time, bool) {
	for _, key := range []string{"DateTimeOriginal", "CreateDate", "MediaCreateDate", "TrackCreateDate"} {
		value, ok := exif[key]
		if !ok {
			continue
		}
		switch v := value.(type) {
		case time.Time:
			return v.UTC(), true
		case string:
			if parsed, ok := parseCaptureTime(v); ok {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}

func captureTimeProvenance(exif map[string]any) (string, float64) {
	for _, key := range []string{"DateTimeOriginal", "CreateDate", "MediaCreateDate", "TrackCreateDate"} {
		if _, ok := captureTimeValue(exif[key]); ok {
			return "embedded_exif", 0.95
		}
	}
	if _, ok := captureTimeValue(exif["FileModifyDate"]); ok {
		return "filesystem", 0.25
	}
	return "unknown", 0
}

func captureTimeValue(value any) (time.Time, bool) {
	switch v := value.(type) {
	case time.Time:
		return v.UTC(), true
	case string:
		return parseCaptureTime(v)
	default:
		return time.Time{}, false
	}
}

func completeCoordinates(values map[string]any) (float64, float64, bool) {
	lat, latOK := number(values["GPSLatitude"])
	lon, lonOK := number(values["GPSLongitude"])
	return lat, lon, latOK && lonOK
}

func probeCoordinates(probe FFProbeResult) (float64, float64, bool) {
	fields := [][]metadataField{probeMetadataFields(probe)}
	lat, latOK := numberString(firstMetadataString(fields, "GPSLatitude"))
	lon, lonOK := numberString(firstMetadataString(fields, "GPSLongitude"))
	return lat, lon, latOK && lonOK
}

func numberString(value string) (float64, bool) {
	if value == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	return v, err == nil
}

func captureTimeFromProbe(probe FFProbeResult) (time.Time, bool) {
	groups := [][]metadataField{probeMetadataFields(probe)}
	for _, key := range []string{"CreationTime", "CreationDate", "DateCreated", "CreateDate", "MediaCreateDate", "TrackCreateDate"} {
		if value := firstMetadataString(groups, key); value != "" {
			if parsed, ok := parseCaptureTime(value); ok {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}

func parseCaptureTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006:01:02 15:04:05Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006:01:02 15:04:05 -07:00",
		"2006-01-02 15:04:05 -07:00",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	for _, layout := range []string{"2006:01:02 15:04:05", "2006-01-02 15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func parseBitDepth(raw, pixelFormat string) int {
	if bits, err := strconv.Atoi(raw); err == nil && bits > 0 {
		return bits
	}
	compact := strings.ToLower(pixelFormat)
	for _, bits := range []int{16, 14, 12, 10, 9, 8} {
		if strings.Contains(compact, strconv.Itoa(bits)) {
			return bits
		}
	}
	return 0
}

func GenerateThumbnail(ctx context.Context, src, dst string, plan HardwarePlan) error {
	previewPlan, err := previewPlanForSource(ctx, src)
	if err != nil {
		return err
	}
	return NewPreviewRenderer("").RenderThumbnail(ctx, src, dst, plan, previewPlan)
}

func GenerateProxy(ctx context.Context, src, dst string, plan HardwarePlan) error {
	previewPlan, err := previewPlanForSource(ctx, src)
	if err != nil {
		return err
	}
	return NewPreviewRenderer("").RenderProxy(ctx, src, dst, plan, previewPlan)
}

// previewPlanForSource keeps the legacy Generate* signatures intact while
// making their media output color-aware. A failed probe falls back to the
// legacy direct path for ordinary files; RAW paths are rejected before probe.
func previewPlanForSource(ctx context.Context, src string) (PreviewRenderPlan, error) {
	if isRawPath(src) {
		return SelectPreviewRenderPlan(SourceColorRAW), nil
	}
	probe, err := Probe(ctx, src)
	return PreviewPlanForProbeResult(probe, err, src), nil
}

// PreviewPlanForProbeResult builds a preview plan from a probe the caller
// already ran (or already attempted), applying the exact same fallbacks
// previewPlanForSource does internally: a RAW path always resolves to the RAW
// plan regardless of what the probe returned, and a failed probe on any other
// path falls back to the direct/"unknown" plan rather than propagating the
// error.
//
// It exists so a caller that needs one FFProbeResult for several purposes
// (e.g. picking a preview plan for both a thumbnail and a proxy render, and
// checking for an audio stream) can probe a source exactly once instead of
// once per purpose -- GenerateThumbnail and GenerateProxy each probing their
// own copy is what used to turn a single derive into three or four ffprobe
// invocations over the same file.
func PreviewPlanForProbeResult(probe FFProbeResult, probeErr error, src string) PreviewRenderPlan {
	if isRawPath(src) {
		return SelectPreviewRenderPlan(SourceColorRAW)
	}
	if probeErr != nil {
		return SelectPreviewRenderPlan(SourceColorUnknown)
	}
	return PreviewRenderPlanFor(probe, nil, src)
}

// ExtractAudio reads the whole source, so it honours readRate (a multiple of
// realtime playback; zero means unlimited) for the same reason the proxy render
// does. See domain.PipelineThrottle.
func ExtractAudio(ctx context.Context, src, dst string, readRate float64) error {
	if err := rejectSourceOverwrite(src, dst); err != nil {
		return err
	}
	return atomicFFmpegOutput(dst, func(out string) error {
		args := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, readRateArgs(readRate)...)
		args = append(args, "-i", src, "-vn", "-ac", "1", "-ar", "16000", "-c:a", "aac", "-b:a", "64k", out)
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		if raw, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("audio: %w: %s", err, truncateStderr(raw))
		}
		return nil
	})
}

// atomicFFmpegOutput runs produce against a temporary file and only publishes it
// to dst once produce succeeds. ffmpeg writes its output path directly, so a run
// killed midway — Ctrl-C, OOM, a full disk, power loss — used to leave a
// truncated file at the final path. The pipeline decides "already derived" with
// a bare os.Stat, so that truncated file was then treated as a finished artifact
// and silently poisoned every downstream stage until the cache was cleared by
// hand. Publishing via rename makes the artifact either absent or complete.
func atomicFFmpegOutput(dst string, produce func(outputPath string) error) error {
	directory := filepath.Dir(dst)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	// ffmpeg picks the muxer from the file extension, so the temporary name has
	// to carry the same one or the artifact is written in the wrong container.
	extension := filepath.Ext(dst)
	stem := strings.TrimSuffix(filepath.Base(dst), extension)
	temporary, err := os.CreateTemp(directory, "."+stem+"-*"+extension)
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	defer os.Remove(temporaryPath)

	if err := produce(temporaryPath); err != nil {
		return err
	}
	return os.Rename(temporaryPath, dst)
}

func runWithFallback(ctx context.Context, label string, args []string, plan HardwarePlan, software func() []string) error {
	// The accelerated attempt runs with the plan's environment, which is how a
	// probed libva driver reaches the encode; the software fallback below is
	// deliberately left on the plain environment.
	hardwareCommand := exec.CommandContext(ctx, "ffmpeg", args...)
	if len(plan.Env) > 0 {
		hardwareCommand.Env = append(os.Environ(), plan.Env...)
	}
	out, err := hardwareCommand.CombinedOutput()
	if err == nil {
		return nil
	}
	if plan.Mode != "software" && plan.AllowFallback {
		fallbackOut, fallbackErr := exec.CommandContext(ctx, "ffmpeg", software()...).CombinedOutput()
		if fallbackErr == nil {
			return nil
		}
		return fmt.Errorf("%s hardware %s failed: %s; software fallback failed: %w: %s", label, plan.Mode, truncateStderr(out), fallbackErr, truncateStderr(fallbackOut))
	}
	return fmt.Errorf("%s (%s): %w: %s", label, plan.Mode, err, truncateStderr(out))
}

const mostlySilentThreshold = 0.80

var silenceEventPattern = regexp.MustCompile(`silence_(start|end):\s*([0-9]+(?:\.[0-9]+)?)`)

func SpeechGate(ctx context.Context, audioPath string, durationMS int64) (domain.SpeechClassification, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-i", audioPath, "-af", "silencedetect=noise=-35dB:d=0.8", "-f", "null", "-")
	out, err := cmd.CombinedOutput()
	if err != nil && ctx.Err() != nil {
		return domain.SpeechClassification{}, ctx.Err()
	}
	return classifySpeechFromSilenceLog(string(out), durationMS)
}

func classifySpeechFromSilenceLog(log string, durationMS int64) (domain.SpeechClassification, error) {
	if durationMS <= 0 {
		return domain.SpeechClassification{}, fmt.Errorf("audio duration is required for speech gate")
	}
	durationSeconds := float64(durationMS) / 1000
	type interval struct{ start, end float64 }
	var intervals []interval
	openStart := -1.0
	for _, match := range silenceEventPattern.FindAllStringSubmatch(log, -1) {
		point, err := strconv.ParseFloat(match[2], 64)
		if err != nil {
			continue
		}
		point = max(0, min(point, durationSeconds))
		switch match[1] {
		case "start":
			if openStart >= 0 {
				intervals = append(intervals, interval{start: openStart, end: point})
			}
			openStart = point
		case "end":
			if openStart >= 0 && point > openStart {
				intervals = append(intervals, interval{start: openStart, end: point})
			}
			openStart = -1
		}
	}
	if openStart >= 0 && durationSeconds > openStart {
		intervals = append(intervals, interval{start: openStart, end: durationSeconds})
	}
	// Sort/merge is unnecessary for FFmpeg's normal sequential output, but
	// makes malformed/duplicated log lines safe for the duration calculation.
	silentSeconds := 0.0
	for i := range intervals {
		for j := i + 1; j < len(intervals); j++ {
			if intervals[j].start < intervals[i].start {
				intervals[i], intervals[j] = intervals[j], intervals[i]
			}
		}
	}
	mergedEnd := -1.0
	for _, current := range intervals {
		if current.end <= mergedEnd {
			continue
		}
		if current.start < mergedEnd {
			current.start = mergedEnd
		}
		silentSeconds += current.end - current.start
		mergedEnd = current.end
	}
	silentRatio := min(1, max(0, silentSeconds/durationSeconds))
	classification := "speech_candidate"
	reason := "audio activity detected; ASR provider should confirm speech"
	if silentRatio >= mostlySilentThreshold {
		classification = "mostly_silent"
		reason = "silent duration exceeds configured threshold"
	}
	speechProbability := max(0.05, 1-silentRatio)
	raw, _ := json.Marshal(map[string]any{"silence_regions": len(intervals), "silent_duration_ms": int64(silentSeconds * 1000), "silent_ratio": silentRatio})
	return domain.SpeechClassification{Classification: classification, SpeechProbability: speechProbability, Reason: reason, RawJSON: string(raw)}, nil
}

const maxStderrBytes = 4096

// truncateStderr keeps CombinedOutput stderr from ballooning error strings.
// Several ffmpeg failure modes (hardware encoder init, corrupt container)
// produce multi-MB logs; embedding the full output in an error both bloats
// the database and risks leaking paths. Only the tail is retained.
func truncateStderr(raw []byte) string {
	if len(raw) <= maxStderrBytes {
		return string(raw)
	}
	return fmt.Sprintf("[truncated %d bytes] ...%s", len(raw)-maxStderrBytes, string(raw[len(raw)-maxStderrBytes:]))
}

func min(left, right float64) float64 {
	if left < right {
		return left
	}
	return right
}

func max(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}

func parseRate(v string) float64 {
	parts := strings.Split(v, "/")
	if len(parts) == 2 {
		a, _ := strconv.ParseFloat(parts[0], 64)
		b, _ := strconv.ParseFloat(parts[1], 64)
		if b != 0 {
			return a / b
		}
	}
	n, _ := strconv.ParseFloat(v, 64)
	return n
}
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case json.Number:
		n, e := x.Float64()
		return n, e == nil
	}
	return 0, false
}
