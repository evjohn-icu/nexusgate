package media

import (
	"path/filepath"
	"strings"
	"unicode"
)

const (
	CaptureMediaFamilyVideo    = "video"
	CaptureMediaFamilyRawVideo = "raw-video"
	CaptureMediaFamily360Video = "360-video"
)

// CaptureProfile is a metadata-only description of a camera capture family.
// It deliberately contains no decoder or color-conversion promise: callers
// can use the result to route media while deciding separately whether a
// particular source is renderable.
type CaptureProfile struct {
	Vendor                   string   `json:"vendor"`
	MediaFamily              string   `json:"media_family"`
	ColorProfile             string   `json:"color_profile"`
	RawFormat                string   `json:"raw_format"`
	SidecarCandidatePatterns []string `json:"sidecar_candidate_patterns"`
	HasSidecarCandidates     bool     `json:"has_sidecar_candidates"`
}

type captureMetadataField struct {
	key   string
	value string
}

var captureExtensionProfiles = map[string]CaptureProfile{
	".braw": {Vendor: "Blackmagic", MediaFamily: CaptureMediaFamilyRawVideo, RawFormat: "BRAW"},
	".crm":  {Vendor: "Canon", MediaFamily: CaptureMediaFamilyRawVideo, RawFormat: "CRM"},
	".insv": {Vendor: "Insta360", MediaFamily: CaptureMediaFamily360Video},
	".nev":  {Vendor: "Nikon", MediaFamily: CaptureMediaFamilyRawVideo, RawFormat: "N-RAW"},
	".r3d":  {Vendor: "RED", MediaFamily: CaptureMediaFamilyRawVideo, RawFormat: "R3D"},
	".ari":  {Vendor: "ARRI", MediaFamily: CaptureMediaFamilyRawVideo, RawFormat: "ARI"},
}

var captureSidecarPatterns = map[string][]string{
	"Blackmagic": {"<basename>.sidecar", "<basename>.braw.sidecar"},
	"DJI":        {"<basename>.srt"},
	"Sony":       {"<basename>.xml"},
	"Canon":      {"<basename>.xml"},
	"GoPro":      {"<basename>.lrv", "<basename>.thm"},
	"Insta360":   {"<basename>_00_*.insv", "<basename>_01_*.insv"},
	"Panasonic":  {"<basename>.xml"},
	"RED":        {"<basename>.rmd", "<basename>.rsx"},
	"ARRI":       {"<basename>.xml", "<basename>.ale"},
}

// RecognizeCaptureProfile infers a capture profile from a filename extension
// and raw ffprobe/exiftool key/value lines. Each line may be key=value or
// key: value; an unkeyed line is treated as a value. No files or external
// tools are accessed.
func RecognizeCaptureProfile(filename string, ffprobeFields, exifFields []string) CaptureProfile {
	fields := captureMetadataFields(ffprobeFields, exifFields)
	profile := captureExtensionProfiles[strings.ToLower(filepath.Ext(filename))]

	if profile.RawFormat == "" {
		profile.RawFormat = captureRawFormat(fields)
	}
	if profile.Vendor == "" {
		profile.Vendor = captureVendor(fields, profile.RawFormat, "")
	}
	if profile.ColorProfile == "" {
		profile.ColorProfile = captureColorProfile(fields)
	}
	if profile.Vendor == "" {
		profile.Vendor = captureVendor(fields, profile.RawFormat, profile.ColorProfile)
	}

	if profile.MediaFamily == "" && (profile.Vendor != "" || profile.RawFormat != "" || profile.ColorProfile != "") {
		profile.MediaFamily = CaptureMediaFamilyVideo
	}
	if profile.RawFormat != "" {
		profile.MediaFamily = CaptureMediaFamilyRawVideo
	}
	if profile.Vendor == "Insta360" {
		profile.MediaFamily = CaptureMediaFamily360Video
	}

	if patterns := captureSidecarPatterns[profile.Vendor]; len(patterns) > 0 {
		profile.SidecarCandidatePatterns = append([]string(nil), patterns...)
		profile.HasSidecarCandidates = true
	}
	return profile
}

func captureMetadataFields(groups ...[]string) []captureMetadataField {
	var fields []captureMetadataField
	for _, group := range groups {
		for _, raw := range group {
			for _, line := range strings.Split(raw, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				key, value := splitCaptureKeyValue(line)
				fields = append(fields, captureMetadataField{key: key, value: value})
			}
		}
	}
	return fields
}

func splitCaptureKeyValue(line string) (string, string) {
	separator := -1
	for index, character := range line {
		if character == '=' || character == ':' {
			separator = index
			break
		}
	}
	if separator < 0 {
		return "", line
	}
	return strings.TrimSpace(line[:separator]), strings.TrimSpace(line[separator+1:])
}

func captureRawFormat(fields []captureMetadataField) string {
	switch {
	case captureHasMarker(fields, "braw", "blackmagicraw"):
		return "BRAW"
	case captureHasMarker(fields, "xocn"):
		return "X-OCN"
	case captureHasMarker(fields, "proresraw"):
		return "ProRes RAW"
	case captureHasMarker(fields, "cinemararawlight", "crm"):
		return "CRM"
	case captureHasMarker(fields, "nraw"):
		return "N-RAW"
	case captureHasMarker(fields, "r3d", "redcode"):
		return "R3D"
	case captureHasMarker(fields, "arriraw") || captureHasWord(fields, "ari"):
		return "ARI"
	default:
		return ""
	}
}

func captureColorProfile(fields []captureMetadataField) string {
	switch {
	case captureHasMarker(fields, "applelog"):
		return "Apple Log"
	case captureHasMarker(fields, "aribstdb67", "hybridloggamma", "hlg"):
		return "HLG"
	case captureHasMarker(fields, "dlog"):
		return "D-Log"
	case captureHasMarker(fields, "slog"):
		return "S-Log"
	case captureHasMarker(fields, "clog"):
		return "C-Log"
	case captureHasMarker(fields, "gplog"):
		return "GP-Log"
	case captureHasMarker(fields, "vlog"):
		return "V-Log"
	case captureHasMarker(fields, "nlog"):
		return "N-Log"
	case captureHasMarker(fields, "flog"):
		return "F-Log"
	case captureHasMarker(fields, "log3g10"):
		return "Log3G10"
	case captureHasMarker(fields, "logc"):
		return "LogC"
	case captureHasMarker(fields, "blackmagicfilm", "blackmagiclog"):
		return "Blackmagic Film"
	default:
		return ""
	}
}

func captureVendor(fields []captureMetadataField, rawFormat, colorProfile string) string {
	switch rawFormat {
	case "BRAW":
		return "Blackmagic"
	case "X-OCN":
		return "Sony"
	case "ProRes RAW":
		return "Apple"
	case "CRM":
		return "Canon"
	case "N-RAW":
		return "Nikon"
	case "R3D":
		return "RED"
	case "ARI":
		return "ARRI"
	}

	switch colorProfile {
	case "Apple Log":
		return "Apple"
	case "D-Log":
		return "DJI"
	case "S-Log":
		return "Sony"
	case "C-Log":
		return "Canon"
	case "GP-Log":
		return "GoPro"
	case "V-Log":
		return "Panasonic"
	case "N-Log":
		return "Nikon"
	case "F-Log":
		return "Fujifilm"
	case "Log3G10":
		return "RED"
	case "LogC":
		return "ARRI"
	}

	for _, candidate := range []struct {
		vendor  string
		markers []string
	}{
		{vendor: "Blackmagic", markers: []string{"blackmagic", "braw"}},
		{vendor: "DJI", markers: []string{"dji"}},
		{vendor: "Sony", markers: []string{"sony", "xocn"}},
		{vendor: "Canon", markers: []string{"canon", "crm"}},
		{vendor: "Apple", markers: []string{"apple", "proresraw"}},
		{vendor: "GoPro", markers: []string{"gopro"}},
		{vendor: "Insta360", markers: []string{"insta360", "insv"}},
		{vendor: "Panasonic", markers: []string{"panasonic"}},
		{vendor: "Nikon", markers: []string{"nikon", "nraw"}},
		{vendor: "Fujifilm", markers: []string{"fujifilm", "fuji"}},
		{vendor: "RED", markers: []string{"redcode", "r3d"}},
		{vendor: "ARRI", markers: []string{"arriraw"}},
	} {
		if captureHasMarker(fields, candidate.markers...) || (candidate.vendor == "RED" && captureHasWord(fields, "red")) || (candidate.vendor == "ARRI" && captureHasWord(fields, "arri")) {
			return candidate.vendor
		}
	}
	return ""
}

func captureHasWord(fields []captureMetadataField, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	for _, field := range fields {
		for _, word := range strings.FieldsFunc(strings.ToLower(field.key+" "+field.value), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}) {
			if word == want {
				return true
			}
		}
	}
	return false
}

func captureHasMarker(fields []captureMetadataField, markers ...string) bool {
	for _, field := range fields {
		text := compactValue(field.key + " " + field.value)
		for _, marker := range markers {
			if strings.Contains(text, compactValue(marker)) {
				return true
			}
		}
	}
	return false
}
