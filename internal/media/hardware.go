package media

import (
	"context"
	"fmt"
	"io"
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
	Backend         string `json:"backend"`
	DecodeAvailable bool   `json:"decode_available"`
	EncodeAvailable bool   `json:"encode_available"`
	// RuntimeAvailable means only "a device node or driver library is present
	// here", full stop. It used to be reset to false whenever the encode
	// probe below failed, which collapsed "no GPU" and "GPU present, driver
	// broken" into the same boolean — exactly the distinction an operator
	// asked to be able to tell apart. Read DeviceOpenable and Verified for
	// the rest of the picture; this field alone answers only "is anything
	// here at all".
	RuntimeAvailable bool `json:"runtime_available"`
	// DeviceOpenable reports that the device node was actually opened for
	// read/write, not merely stat'd. os.Stat succeeds on a /dev/dri node the
	// calling uid has no rights to — the ordinary shape of a Docker
	// container that was handed --device=/dev/dri but not the render/video
	// group that owns it — so it cannot separate "present" from "present but
	// forbidden". Opening it can, and it is what turns a permission error
	// that looks exactly like a broken driver into a diagnosable state.
	// cuda and videotoolbox have no comparable device node in this codebase
	// (cuda is detected through /dev/nvidia0, nvidia-smi or a WSL library
	// path; videotoolbox is a macOS framework, not a file), so for them this
	// just mirrors RuntimeAvailable — there is no separate permission layer
	// to probe.
	DeviceOpenable bool   `json:"device_openable"`
	Encoder        string `json:"encoder,omitempty"`
	Device         string `json:"device,omitempty"`
	// Driver is the libva driver the probe had to name explicitly to make this
	// backend work. Empty means libva's own choice was already right, which is
	// the common case; a value here is worth showing because it is otherwise
	// invisible and it is what the operator would have to set by hand.
	Driver   string `json:"driver,omitempty"`
	Selected bool   `json:"selected"`
	// Verified is true only once probeEncoder has actually run a frame
	// through this backend and it worked. False does not mean broken:
	// detection stops probing as soon as one backend in the preference order
	// succeeds, because each probe is a real FFmpeg encode and repeating it
	// for every backend on every detection would make an already-working
	// machine measurably slower for no benefit. A backend past the winner is
	// therefore reported honestly as "untested", not silently marked failed
	// — check RuntimeAvailable/DeviceOpenable/Remedy for what is actually
	// known about an unverified backend.
	Verified bool `json:"verified"`
	// Remedy is the actionable fix for whatever state this backend is in:
	// no device (pass it through), device present but not openable (fix
	// group membership), or openable but the probe failed (install/upgrade
	// the driver). It is empty when the backend is confirmed working, and
	// also empty when it was simply never probed (see Verified) — an empty
	// Remedy on an unverified backend means "nothing checked", not "nothing
	// wrong".
	Remedy string `json:"remedy,omitempty"`
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
	// Env holds environment assignments the hardware command needs, in
	// exec.Cmd form. It applies to the accelerated attempt only: a software
	// fallback must not inherit a driver override that just failed.
	Env []string
}

// Profile names the derived artifact so a software proxy is never mislabelled
// as a hardware one. It deliberately ignores Env: iHD and i965 are two ways of
// reaching the same VAAPI encoder on the same GPU, so folding the driver name
// in here would invalidate every existing artifact the first time libva's
// default changed, without any difference in what was produced.
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
	hw, hwErr := ffmpegOutput(ctx, nil, "-hide_banner", "-hwaccels")
	enc, encErr := ffmpegOutput(ctx, nil, "-hide_banner", "-encoders")
	if hwErr != nil || encErr != nil {
		report.Warning = "could not inspect all FFmpeg accelerator capabilities"
	}
	device := cfg.Device
	if device == "" && runtime.GOOS == "linux" {
		device = "/dev/dri/renderD128"
	}
	caps := []HardwareCapability{
		{Backend: "cuda", DecodeAvailable: hasToken(hw, "cuda"), EncodeAvailable: hasToken(enc, "h264_nvenc"), RuntimeAvailable: deviceAvailable("cuda", ""), DeviceOpenable: deviceOpenable("cuda", ""), Encoder: "h264_nvenc"},
		{Backend: "qsv", DecodeAvailable: hasToken(hw, "qsv"), EncodeAvailable: hasToken(enc, "h264_qsv"), RuntimeAvailable: deviceAvailable("qsv", device), DeviceOpenable: deviceOpenable("qsv", device), Encoder: "h264_qsv", Device: device},
		{Backend: "vaapi", DecodeAvailable: hasToken(hw, "vaapi"), EncodeAvailable: hasToken(enc, "h264_vaapi"), RuntimeAvailable: deviceAvailable("vaapi", device), DeviceOpenable: deviceOpenable("vaapi", device), Encoder: "h264_vaapi", Device: device},
		{Backend: "videotoolbox", DecodeAvailable: hasToken(hw, "videotoolbox"), EncodeAvailable: hasToken(enc, "h264_videotoolbox"), RuntimeAvailable: deviceAvailable("videotoolbox", ""), DeviceOpenable: deviceOpenable("videotoolbox", ""), Encoder: "h264_videotoolbox"},
	}
	report.Capabilities = caps
	selected := "software"
	selectedDriver := ""
	// probeFailed remembers which backends were actually put through
	// probeEncoder and lost, as opposed to backends that were never reached
	// because the preference order already found a winner. The distinction
	// matters for Remedy below: a probed-and-failed backend gets a driver
	// remedy, an unreached one gets none, because inventing a verdict for
	// something never tested would be worse than saying nothing.
	probeFailed := map[string]bool{}
	for _, candidate := range preference(mode) {
		for i := range report.Capabilities {
			cap := &report.Capabilities[i]
			if cap.Backend != candidate || !cap.DecodeAvailable || !cap.EncodeAvailable || !cap.RuntimeAvailable || !cap.DeviceOpenable {
				continue
			}
			// The checks above establish that FFmpeg was built with the encoder,
			// that a device node is present, and that this process can open it.
			// None of that means the encoder can actually run here: a driver too
			// old for the build's NVENC API, or a GPU whose kernel module is
			// loaded but whose firmware is missing, still passes every check
			// above and fails on the first real frame. Encoding one throwaway
			// frame is what tells them apart, and it costs a fraction of a
			// second once per detection.
			driver, runs := probeEncoderFunc(ctx, cap.Backend, cap.Encoder, device)
			if !runs {
				probeFailed[cap.Backend] = true
				continue
			}
			cap.Verified = true
			cap.Driver = driver
			selected, selectedDriver, cap.Selected = candidate, driver, true
			break
		}
		if selected != "software" {
			break
		}
	}
	// A verdict for every backend, not only the ones the preference order
	// reached, is the point of the report — but repeating the FFmpeg encode
	// above for a backend that will not be used just to fill in its Remedy
	// would make detection slower for nothing. So this pass is static: it
	// derives a Remedy from fields already known (RuntimeAvailable,
	// DeviceOpenable, and whether the loop above actually probed this
	// backend), never from a probe of its own.
	for i := range report.Capabilities {
		cap := &report.Capabilities[i]
		cap.Remedy = remedyFor(*cap, probeFailed[cap.Backend])
	}
	if mode != "auto" && mode != "software" && selected == "software" {
		report.Warning = fmt.Sprintf("requested %s is unavailable in this FFmpeg build; using software", mode)
	} else if mode == "auto" && selected == "software" && report.FFmpegFound {
		report.Warning = "FFmpeg supports no usable hardware media device; using software"
	}
	plan = planFor(selected, device, cfg.AllowFallback, bitrate)
	plan.Env = libvaDriverEnv(selectedDriver)
	report.SelectedMode = selected
	return report, plan
}

// SelectedBackends returns the accelerator DetectHardware actually chose, as
// a single-element slice, or nil when it fell back to software. It is the
// compact, scheduling-facing summary that travels in
// remote.WorkerCapabilities.Hardware so the Hub can route work toward a GPU
// Worker; the full per-backend diagnosis (decode/encode/runtime/openable/
// verified/remedy) already lives in HardwareReport itself and a caller that
// needs more than "which one, if any" should read that instead of this
// growing another field.
func (r HardwareReport) SelectedBackends() []string {
	if r.SelectedMode == "" || r.SelectedMode == "software" {
		return nil
	}
	return []string{r.SelectedMode}
}

// FormatHardwareReport renders the per-backend diagnosis table shared by
// `timingdex doctor` (Hub) and `timingdex worker doctor`. The Worker is the
// process that actually owns the GPU and runs the encode, so both need the
// same table; keeping one implementation means a wording change cannot fix
// one and forget the other.
func FormatHardwareReport(w io.Writer, r HardwareReport) {
	fmt.Fprintf(w, "hardware acceleration: requested=%s selected=%s fallback=%t\n", r.RequestedMode, r.SelectedMode, r.Fallback)
	for _, cap := range r.Capabilities {
		fmt.Fprintf(w, "  %s: decode=%t encode=%t runtime=%t openable=%t verified=%t selected=%t\n",
			cap.Backend, cap.DecodeAvailable, cap.EncodeAvailable, cap.RuntimeAvailable, cap.DeviceOpenable, cap.Verified, cap.Selected)
		if cap.Driver != "" {
			// Only shown when the probe had to override libva's own choice,
			// because that is the one case an operator would otherwise have to
			// rediscover by hand.
			fmt.Fprintf(w, "    libva driver: %s\n", cap.Driver)
		}
		if cap.Remedy != "" {
			fmt.Fprintf(w, "    remedy: %s\n", cap.Remedy)
		}
	}
	if r.Warning != "" {
		fmt.Fprintf(w, "  warning: %s\n", r.Warning)
	}
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
		// NVENC H.264 encodes 8-bit 4:2:0 only, so a 10-bit source reaches it
		// as an unsupported surface and the encoder reports "No capable devices
		// found" — a message that reads like a missing GPU rather than a
		// pixel format. Cameras make this the common case, not the exception:
		// HEVC 10-bit and ProRes 4:2:2 10-bit are what drones, phones and
		// cinema bodies record. vaapi below already converts for the same
		// reason; videotoolbox does it internally and needs no filter.
		p.ProxyFilter = ",format=yuv420p"
		p.EncoderArgs = []string{"-c:v", "h264_nvenc", "-preset", "p4", "-b:v", fmt.Sprintf("%dk", bitrate), "-maxrate", fmt.Sprintf("%dk", bitrate*2), "-bufsize", fmt.Sprintf("%dk", bitrate*4)}
	case "qsv":
		p.DecoderArgs = []string{"-hwaccel", "qsv"}
		// Same 8-bit constraint as NVENC above: h264_qsv encodes 4:2:0 8-bit,
		// so a 10-bit source has to be converted or the encoder refuses it.
		// This one matters more than its share of hardware suggests — an Intel
		// iGPU is what a NAS has, and a NAS is where this runs.
		p.ProxyFilter = ",format=nv12"
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

// libvaDriverCandidates are the LIBVA_DRIVER_NAME values a libva-backed probe
// tries, in order. The empty first entry is libva's own choice and normally
// wins, so a working machine pays one probe.
//
// The rest exist because that choice is often wrong on exactly the hardware
// this project targets. libva maps the kernel driver name to a userspace one,
// and on Debian- and Ubuntu-derived releases an i915 device still resolves to
// i965 — a driver that supports nothing from Gen12 onward. A current Intel
// iGPU, which is what a NAS has, therefore reports no usable encoder at all
// while the very same FFmpeg encodes at full speed once the driver is named.
// i965 is listed for the mirror-image case: iHD dropped the pre-Gen8 parts that
// i965 still serves.
//
// Naming the driver is the whole fix, and it is not discoverable from any
// error message FFmpeg produces, so it belongs in the probe rather than in
// documentation telling an operator to export a variable.
var libvaDriverCandidates = []string{"", "iHD", "i965"}

// libvaDriverEnv converts a probed driver name into exec.Cmd environment.
// An empty name means libva's default was already correct and no override
// should be forced on the command.
func libvaDriverEnv(driver string) []string {
	if driver == "" {
		return nil
	}
	return []string{"LIBVA_DRIVER_NAME=" + driver}
}

// probeEncoder encodes a single synthetic frame to confirm the accelerator is
// usable, discarding the output, and reports which libva driver made it work.
// It answers "can this machine encode at all with this backend", not "can it
// encode a given source" — a 10-bit source still needs the pixel-format
// conversion each plan carries, because a probe frame that is already 8-bit
// would pass either way.
// probeEncoderFunc is a test seam: DetectHardware calls it instead of
// probeEncoder directly so a test can force a probe to succeed or fail
// without depending on the test machine actually having (or actually
// lacking) working GPU hardware. Production code never reassigns it.
var probeEncoderFunc = probeEncoder

func probeEncoder(ctx context.Context, backend, encoder, device string) (driver string, ok bool) {
	if encoder == "" {
		return "", false
	}
	candidates := []string{""}
	// QSV reaches the GPU through libva on Linux just as VAAPI does, so it is
	// subject to the same mis-selection; CUDA and VideoToolbox are not.
	if backend == "vaapi" || backend == "qsv" {
		candidates = libvaDriverCandidates
	}
	for _, candidate := range candidates {
		if encoderRuns(ctx, backend, encoder, device, libvaDriverEnv(candidate)) {
			return candidate, true
		}
	}
	return "", false
}

func encoderRuns(ctx context.Context, backend, encoder, device string, env []string) bool {
	if encoder == "" {
		return false
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	if backend == "vaapi" {
		// VAAPI encodes from GPU surfaces, so the frame has to be uploaded;
		// the other backends accept software frames directly.
		args = append(args, "-vaapi_device", device)
	}
	args = append(args, "-f", "lavfi", "-i", "color=c=black:s=256x144:r=25:d=0.2", "-frames:v", "1")
	if backend == "vaapi" {
		args = append(args, "-vf", "format=nv12,hwupload")
	}
	args = append(args, "-c:v", encoder, "-f", "null", "-")
	_, err := ffmpegOutput(ctx, env, args...)
	return err == nil
}

func ffmpegOutput(ctx context.Context, env []string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "ffmpeg", args...)
	if len(env) > 0 {
		command.Env = append(os.Environ(), env...)
	}
	out, err := command.CombinedOutput()
	return string(out), err
}
func hasToken(text, token string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(token))
}

// wslNVENCLibrary is where the WSL NVIDIA driver installs the encoder library.
// It is a variable so the test can point at a fixture instead of requiring a
// GPU, and is never used as a library search path — FFmpeg dlopens the real one
// through ldconfig.
var wslNVENCLibrary = "/usr/lib/wsl/lib/libnvidia-encode.so.1"

func deviceAvailable(backend, device string) bool {
	switch backend {
	case "cuda":
		if _, err := os.Stat("/dev/nvidia0"); err == nil {
			return true
		}
		if _, err := exec.LookPath("nvidia-smi"); err == nil {
			return true
		}
		// WSL2 has neither of the above: the GPU arrives through /dev/dxg
		// instead of /dev/nvidia0, and the driver lands in /usr/lib/wsl/lib
		// without putting nvidia-smi on PATH. Both checks above therefore fail
		// on a machine whose NVENC works perfectly.
		//
		// Test for the encoder library rather than for /dev/dxg, which is also
		// present for AMD and Intel GPUs that cannot serve h264_nvenc at all.
		_, err := os.Stat(wslNVENCLibrary)
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

// deviceOpenable is deviceAvailable's stricter sibling: it actually opens the
// device node for read/write and closes it immediately, rather than
// stat'ing it. A Docker container that receives --device=/dev/dri but not
// the render/video group that owns it passes os.Stat and fails to open —
// which, without this check, looks identical to a broken driver once the
// encode probe also fails. Distinguishing the two is the entire point of
// this function.
func deviceOpenable(backend, device string) bool {
	switch backend {
	case "qsv", "vaapi":
		if runtime.GOOS == "windows" {
			// deviceAvailable treats Windows as always-present because there
			// is no /dev node to check there; mirror that here instead of
			// opening a Linux-shaped path that can never exist on Windows.
			return true
		}
		f, err := os.OpenFile(device, os.O_RDWR, 0)
		if err != nil {
			return false
		}
		_ = f.Close()
		return true
	default:
		// cuda is detected through /dev/nvidia0 presence, nvidia-smi on PATH,
		// or a WSL library path — none of which is "this process's own
		// device node" the way /dev/dri is, so there is no separate open to
		// attempt. videotoolbox is a macOS framework, not a file. Both
		// collapse to whatever deviceAvailable already found.
		return deviceAvailable(backend, device)
	}
}

// remedyFor picks the Remedy for one capability from what DetectHardware
// already knows about it, once detection is finished. probed is true only
// when probeEncoder actually ran for this backend (whether it passed or
// failed), which is what tells "openable but never tried because an earlier
// backend already won" apart from "openable and actually tried and failed" —
// both share the same RuntimeAvailable/DeviceOpenable/Verified combination
// and only probed distinguishes them. It is its own function, separate from
// the loop that calls it, so the four states this feature exists to expose
// are unit-testable without a GPU in the test machine.
func remedyFor(cap HardwareCapability, probed bool) string {
	switch {
	case cap.Verified:
		return ""
	case !cap.DecodeAvailable || !cap.EncodeAvailable:
		return fmt.Sprintf("this ffmpeg build has no %s encoder; nothing about the GPU can change that", cap.Encoder)
	case !cap.RuntimeAvailable:
		return remedyForNoDevice(cap.Backend)
	case !cap.DeviceOpenable:
		return remedyForPermission(cap.Device)
	case probed:
		return remedyForFailedProbe(cap.Backend)
	default:
		// RuntimeAvailable && DeviceOpenable && never probed: an earlier
		// backend in the preference order already won selection (or this
		// mode never asked for this one), so nothing ran here. Empty Remedy
		// — Verified == false already says "untested" without this code
		// claiming to know it is broken.
		return ""
	}
}

// remedyForNoDevice explains how to make a backend's device visible at all.
// Once the device is through, everything past this point is the driver's
// problem rather than the host's, which is why this is the first branch to
// rule out.
//
// Both a container and a bare-metal remedy are named because this binary runs
// either way — Docker and Unraid on one side, a systemd unit on a NAS on the
// other — and a message that assumes the wrong one sends the operator looking
// for a compose file that does not exist.
func remedyForNoDevice(backend string) string {
	switch backend {
	case "cuda":
		return "no NVIDIA device visible; in a container install the NVIDIA Container Toolkit and pass --gpus all (or the compose deploy.resources.reservations.devices entry), on a host check that the driver is loaded and nvidia-smi runs"
	case "qsv", "vaapi":
		return "no /dev/dri device visible; in a container pass --device=/dev/dri:/dev/dri or add it under compose devices:, on a host confirm the GPU has a render node (run deploy/prepare-node.sh --check)"
	default:
		// videotoolbox is platform-locked, not passed through — there is no
		// device to add on a non-Darwin host, so no remedy applies.
		return ""
	}
}

// remedyForPermission names the fix for a device node that exists but that
// this process could not open — deploy/prepare-node.sh diagnoses the same
// condition on bare metal with `[ -r "$node" ] && [ -w "$node" ]` and fixes
// it with usermod; this is the container-shaped equivalent of that fix.
func remedyForPermission(device string) string {
	return fmt.Sprintf("%s exists but this process could not open it for read/write; it is normally group-owned by render or video — add that GID under compose group_add: in a container, or run deploy/prepare-node.sh on a host to add the user to the group", device)
}

// remedyForFailedProbe names the fix for a device that opens cleanly but
// still fails to encode a single frame — the state that means the driver
// itself, not the plumbing around it, is the problem.
func remedyForFailedProbe(backend string) string {
	switch backend {
	case "qsv", "vaapi":
		return "device opens but every encode failed; the installed driver is likely wrong for this GPU generation — install intel-media-va-driver-non-free (Gen8+) or intel-media-va-driver (older) and retry"
	case "cuda":
		return "device opens but NVENC failed; check that NVIDIA_DRIVER_CAPABILITIES includes video, or that libnvidia-encode.so.1 is actually being injected into the container"
	default:
		return "device opens but the encoder failed to run"
	}
}
