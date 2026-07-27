package media

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// HardwareConfig controls FFmpeg acceleration for derived media only. Source
// footage is always read-only and a failed accelerator attempt falls back to
// the software profile when AllowFallback is enabled.
type HardwareConfig struct {
	Mode             string `json:"mode"` // auto, software, cuda, qsv, vaapi, videotoolbox
	Device           string `json:"device,omitempty"`
	AllowFallback    bool   `json:"allow_fallback"`
	ProxyBitrateKbps int    `json:"proxy_bitrate_kbps"`
}

type HardwareCapability struct {
	Backend          string `json:"backend"`
	DecodeAvailable  bool   `json:"decode_available"`
	EncodeAvailable  bool   `json:"encode_available"`
	RuntimeAvailable bool   `json:"runtime_available"`
	Encoder          string `json:"encoder,omitempty"`
	Device           string `json:"device,omitempty"`
	Selected         bool   `json:"selected"`
}

type HardwareReport struct {
	RequestedMode string               `json:"requested_mode"`
	SelectedMode  string               `json:"selected_mode"`
	Fallback      bool                 `json:"fallback_enabled"`
	FFmpegFound   bool                 `json:"ffmpeg_found"`
	Capabilities  []HardwareCapability `json:"capabilities"`
	Warning       string               `json:"warning,omitempty"`
}

// HardwarePlan contains only FFmpeg command choices. It does not grant a model
// access to the GPU and does not change the semantic/AI pipeline.
type HardwarePlan struct {
	Mode          string
	Device        string
	DecoderArgs   []string
	EncoderArgs   []string
	ProxyFilter   string
	AllowFallback bool
	BitrateKbps   int
}

func (p HardwarePlan) Profile() string {
	if p.Mode == "software" {
		return "software-h264-x264-v1"
	}
	return "hw-" + p.Mode + "-h264-v1"
}

func DetectHardware(ctx context.Context, cfg HardwareConfig) (HardwareReport, HardwarePlan) {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == "" {
		mode = "auto"
	}
	bitrate := cfg.ProxyBitrateKbps
	if bitrate <= 0 {
		bitrate = 1800
	}
	report := HardwareReport{RequestedMode: mode, Fallback: cfg.AllowFallback}
	plan := HardwarePlan{Mode: "software", AllowFallback: cfg.AllowFallback, BitrateKbps: bitrate}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		report.Warning = "ffmpeg not found; software commands cannot run"
		return report, plan
	}
	report.FFmpegFound = path != ""
	hw, hwErr := ffmpegOutput(ctx, "-hide_banner", "-hwaccels")
	enc, encErr := ffmpegOutput(ctx, "-hide_banner", "-encoders")
	if hwErr != nil || encErr != nil {
		report.Warning = "could not inspect all FFmpeg accelerator capabilities"
	}
	device := cfg.Device
	if device == "" && runtime.GOOS == "linux" {
		device = "/dev/dri/renderD128"
	}
	caps := []HardwareCapability{
		{Backend: "cuda", DecodeAvailable: hasToken(hw, "cuda"), EncodeAvailable: hasToken(enc, "h264_nvenc"), RuntimeAvailable: deviceAvailable("cuda", ""), Encoder: "h264_nvenc"},
		{Backend: "qsv", DecodeAvailable: hasToken(hw, "qsv"), EncodeAvailable: hasToken(enc, "h264_qsv"), RuntimeAvailable: deviceAvailable("qsv", device), Encoder: "h264_qsv", Device: device},
		{Backend: "vaapi", DecodeAvailable: hasToken(hw, "vaapi"), EncodeAvailable: hasToken(enc, "h264_vaapi"), RuntimeAvailable: deviceAvailable("vaapi", device), Encoder: "h264_vaapi", Device: device},
		{Backend: "videotoolbox", DecodeAvailable: hasToken(hw, "videotoolbox"), EncodeAvailable: hasToken(enc, "h264_videotoolbox"), RuntimeAvailable: deviceAvailable("videotoolbox", ""), Encoder: "h264_videotoolbox"},
	}
	report.Capabilities = caps
	selected := "software"
	for _, candidate := range preference(mode) {
		for i := range report.Capabilities {
			cap := &report.Capabilities[i]
			if cap.Backend == candidate && cap.DecodeAvailable && cap.EncodeAvailable && cap.RuntimeAvailable {
				selected, cap.Selected = candidate, true
				break
			}
		}
		if selected != "software" {
			break
		}
	}
	if mode != "auto" && mode != "software" && selected == "software" {
		report.Warning = fmt.Sprintf("requested %s is unavailable in this FFmpeg build; using software", mode)
	} else if mode == "auto" && selected == "software" && report.FFmpegFound {
		report.Warning = "FFmpeg supports no usable hardware media device; using software"
	}
	plan = planFor(selected, device, cfg.AllowFallback, bitrate)
	report.SelectedMode = selected
	return report, plan
}

func preference(mode string) []string {
	if mode != "auto" {
		return []string{mode}
	}
	if runtime.GOOS == "darwin" {
		return []string{"videotoolbox"}
	}
	return []string{"cuda", "qsv", "vaapi"}
}

func planFor(mode, device string, fallback bool, bitrate int) HardwarePlan {
	p := HardwarePlan{Mode: mode, Device: device, AllowFallback: fallback, BitrateKbps: bitrate}
	switch mode {
	case "cuda":
		p.DecoderArgs = []string{"-hwaccel", "cuda"}
		p.EncoderArgs = []string{"-c:v", "h264_nvenc", "-preset", "p4", "-b:v", fmt.Sprintf("%dk", bitrate), "-maxrate", fmt.Sprintf("%dk", bitrate*2), "-bufsize", fmt.Sprintf("%dk", bitrate*4)}
	case "qsv":
		p.DecoderArgs = []string{"-hwaccel", "qsv"}
		p.EncoderArgs = []string{"-c:v", "h264_qsv", "-b:v", fmt.Sprintf("%dk", bitrate)}
	case "vaapi":
		p.DecoderArgs = []string{"-vaapi_device", device, "-hwaccel", "vaapi", "-hwaccel_device", device}
		p.ProxyFilter = ",format=nv12,hwupload"
		p.EncoderArgs = []string{"-c:v", "h264_vaapi", "-b:v", fmt.Sprintf("%dk", bitrate)}
	case "videotoolbox":
		p.DecoderArgs = []string{"-hwaccel", "videotoolbox"}
		p.EncoderArgs = []string{"-c:v", "h264_videotoolbox", "-b:v", fmt.Sprintf("%dk", bitrate)}
	default:
		p.Mode = "software"
		p.EncoderArgs = []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "28"}
	}
	return p
}

func ffmpegOutput(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()
	return string(out), err
}
func hasToken(text, token string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(token))
}

func deviceAvailable(backend, device string) bool {
	switch backend {
	case "cuda":
		if _, err := os.Stat("/dev/nvidia0"); err == nil {
			return true
		}
		_, err := exec.LookPath("nvidia-smi")
		return err == nil
	case "qsv", "vaapi":
		if runtime.GOOS == "windows" {
			return true
		}
		_, err := os.Stat(device)
		return err == nil
	case "videotoolbox":
		return runtime.GOOS == "darwin"
	default:
		return false
	}
}
