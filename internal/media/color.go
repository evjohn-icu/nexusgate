package media

import (
	"path/filepath"
	"strings"
	"unicode"
)

// SourceColorClass is the color/source family inferred without decoding
// frames. It is media-local so callers can choose how (or whether) to persist
// it later.
type SourceColorClass string

const (
	SourceColorSDR        SourceColorClass = "SDR"
	SourceColorHDRHLG     SourceColorClass = "HDR_HLG"
	SourceColorHDRPQ      SourceColorClass = "HDR_PQ"
	SourceColorLogApple   SourceColorClass = "LOG_APPLE"
	SourceColorLogUnknown SourceColorClass = "LOG_UNKNOWN"
	SourceColorRAW        SourceColorClass = "RAW"
	SourceColorUnknown    SourceColorClass = "UNKNOWN"
)

// Short enum names keep the classification vocabulary convenient for media
// package callers while SourceColor* names remain explicit at call sites.
const (
	SDR         = SourceColorSDR
	HDR_HLG     = SourceColorHDRHLG
	HDR_PQ      = SourceColorHDRPQ
	LOG_APPLE   = SourceColorLogApple
	LOG_UNKNOWN = SourceColorLogUnknown
	RAW         = SourceColorRAW
	UNKNOWN     = SourceColorUnknown
)

// PreviewRenderMode describes the operation needed to make a preview from a
// source. A LUT plan is intentionally unavailable until a LUT is supplied.
type PreviewRenderMode string

const (
	PreviewRenderDirect      PreviewRenderMode = "direct"
	PreviewRenderToneMap     PreviewRenderMode = "tone_map"
	PreviewRenderLUT         PreviewRenderMode = "lut"
	PreviewRenderUnavailable PreviewRenderMode = "unavailable"
)

// PreviewRenderPlan is a media-level decision; it does not execute FFmpeg or
// change the source file.
type PreviewRenderPlan struct {
	SourceColor SourceColorClass
	Mode        PreviewRenderMode
	Filter      string
	RequiresLUT bool
	Available   bool
}

var rawExtensions = map[string]struct{}{
	".braw": {},
	".r3d":  {},
	".dng":  {},
	".ari":  {},
	".crm":  {},
	".nev":  {},
}

// ClassifySourceColor infers source color from the first video stream, format
// tags, EXIF tags, and the file extension. RAW extensions take precedence
// because their source representation is unavailable to the normal preview
// renderer even if a probe reports incidental color metadata.
func ClassifySourceColor(probe FFProbeResult, exif map[string]any, path string) SourceColorClass {
	if isRawPath(path) || hasRawMetadata(probe, exif) {
		return SourceColorRAW
	}

	streams := videoStreams(probe)
	if hasAppleLogMetadata(probe, exif, streams) {
		return SourceColorLogApple
	}
	if hasTransfer(streams, isHLGTransfer) || hasTagValue(probe.Format.Tags, exif, isHLGValue) {
		return SourceColorHDRHLG
	}
	if hasTransfer(streams, isPQTransfer) || hasTagValue(probe.Format.Tags, exif, isPQValue) {
		return SourceColorHDRPQ
	}
	if hasLogMetadata(probe, exif, streams) {
		return SourceColorLogUnknown
	}
	if len(streams) > 0 || hasTagValue(probe.Format.Tags, exif, isSDRValue) {
		return SourceColorSDR
	}
	return SourceColorUnknown
}

// SelectPreviewRenderPlan maps a classified source to the only preview
// operation currently supported by the media layer.
func SelectPreviewRenderPlan(source SourceColorClass) PreviewRenderPlan {
	plan := PreviewRenderPlan{SourceColor: source}
	switch source {
	case SourceColorHDRHLG, SourceColorHDRPQ:
		plan.Mode = PreviewRenderToneMap
		plan.Filter = "tonemap"
		plan.Available = true
	case SourceColorLogApple, SourceColorLogUnknown:
		plan.Mode = PreviewRenderLUT
		plan.RequiresLUT = true
	case SourceColorRAW:
		plan.Mode = PreviewRenderUnavailable
	default:
		plan.Mode = PreviewRenderDirect
		plan.Available = true
	}
	return plan
}

// PreviewRenderPlanFor classifies metadata and selects its preview operation
// in one step for callers that have not already classified the source.
func PreviewRenderPlanFor(probe FFProbeResult, exif map[string]any, path string) PreviewRenderPlan {
	return SelectPreviewRenderPlan(ClassifySourceColor(probe, exif, path))
}

func isRawPath(path string) bool {
	_, ok := rawExtensions[strings.ToLower(filepath.Ext(path))]
	return ok
}

func videoStreams(probe FFProbeResult) []FFProbeStream {
	var out []FFProbeStream
	for _, stream := range probe.Streams {
		if stream.CodecType == "video" {
			out = append(out, stream)
		}
	}
	return out
}

func hasRawMetadata(probe FFProbeResult, exif map[string]any) bool {
	if hasTagValue(probe.Format.Tags, exif, isRawValue) {
		return true
	}
	for _, stream := range probe.Streams {
		if isRawValue(stream.CodecName) || isRawValue(stream.CodecTagString) || isRawValue(stream.PixFmt) || hasTagValueOnly(stream.Tags, isRawValue) {
			return true
		}
	}
	return false
}

func isRawValue(value string) bool {
	compact := compactValue(value)
	return compact == "braw" || compact == "r3d" || compact == "dng" || compact == "ari" || compact == "crm" || compact == "nev" || compact == "nraw" || strings.Contains(compact, "blackmagicraw") || strings.Contains(compact, "redcode") || strings.Contains(compact, "cinemadng") || strings.Contains(compact, "bayer")
}

func hasAppleLogMetadata(probe FFProbeResult, exif map[string]any, streams []FFProbeStream) bool {
	for _, stream := range streams {
		if isAppleLogValue(stream.ColorTransfer) || isAppleLogValue(stream.Profile) || hasTagValueOnly(stream.Tags, isAppleLogValue) {
			return true
		}
	}
	return hasTagValue(probe.Format.Tags, exif, isAppleLogValue)
}

func hasLogMetadata(probe FFProbeResult, exif map[string]any, streams []FFProbeStream) bool {
	for _, stream := range streams {
		if isLogValue(stream.ColorTransfer) || isLogValue(stream.Profile) || hasTagValueOnly(stream.Tags, isLogValue) {
			return true
		}
	}
	return hasTagValue(probe.Format.Tags, exif, isLogValue)
}

func hasTransfer(streams []FFProbeStream, match func(string) bool) bool {
	for _, stream := range streams {
		if match(stream.ColorTransfer) {
			return true
		}
	}
	return false
}

func hasTagValue(formatTags map[string]string, exif map[string]any, match func(string) bool) bool {
	return hasTagValueOnly(formatTags, match) || hasAnyValue(exif, match)
}

func hasTagValueOnly(tags map[string]string, match func(string) bool) bool {
	for key, value := range tags {
		if match(key) || match(value) {
			return true
		}
	}
	return false
}

func hasAnyValue(values map[string]any, match func(string) bool) bool {
	for key, value := range values {
		if match(key) || anyStringValue(value, match) {
			return true
		}
	}
	return false
}

func anyStringValue(value any, match func(string) bool) bool {
	switch typed := value.(type) {
	case string:
		return match(typed)
	case []string:
		for _, item := range typed {
			if match(item) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if anyStringValue(item, match) {
				return true
			}
		}
	}
	return false
}

func isAppleLogValue(value string) bool {
	compact := compactValue(value)
	return strings.Contains(compact, "applelog")
}

func isHLGTransfer(value string) bool {
	compact := compactValue(value)
	return compact == "aribstdb67" || compact == "hlg" || strings.Contains(compact, "hybridloggamma")
}

func isPQTransfer(value string) bool {
	compact := compactValue(value)
	return compact == "smpte2084" || compact == "pq" || compact == "perceptualquantizer" || strings.Contains(compact, "st2084")
}

func isHLGValue(value string) bool {
	compact := compactValue(value)
	return isHLGTransfer(value) || strings.Contains(compact, "hlg")
}

func isPQValue(value string) bool {
	compact := compactValue(value)
	return isPQTransfer(value) || compact == "hdr10" || strings.Contains(compact, "hdr10")
}

func isSDRValue(value string) bool {
	compact := compactValue(value)
	return compact == "bt709" || compact == "rec709" || compact == "srgb" || compact == "bt601" || compact == "rec601" || compact == "gamma22" || compact == "gamma24"
}

func isLogValue(value string) bool {
	compact := compactValue(value)
	if compact == "" || isAppleLogValue(value) {
		return false
	}
	return compact == "log" || strings.Contains(compact, "logc") || strings.Contains(compact, "log3g") || strings.Contains(compact, "slog") || strings.Contains(compact, "clog") || strings.Contains(compact, "flog") || strings.Contains(compact, "nlog") || strings.Contains(compact, "vlog") || strings.Contains(compact, "cineon") || strings.Contains(compact, "blackmagiclog") || strings.Contains(compact, "davinciintermediate")
}

func compactValue(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
