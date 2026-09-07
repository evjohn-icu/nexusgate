package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A hardware encoder that cannot take the source's pixel format must be given
// a conversion in the filter chain, or it fails on most real footage: drones,
// phones and cinema cameras record 10-bit, and NVENC H.264 is 8-bit only.
func TestHardwarePlansConvertPixelFormatForEightBitOnlyEncoders(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{"cuda", "format=yuv420p"},
		{"qsv", "format=nv12"},
		{"vaapi", "format=nv12"},
	} {
		plan := planFor(tc.mode, "/dev/dri/renderD128", true, 1800)
		if !strings.Contains(plan.ProxyFilter, tc.want) {
			t.Errorf("%s plan must convert the pixel format, ProxyFilter=%q want it to contain %q", tc.mode, plan.ProxyFilter, tc.want)
		}
	}
	// Software x264 encodes 10-bit directly, so converting there would discard
	// precision for no reason.
	if planFor("software", "", true, 1800).ProxyFilter != "" {
		t.Errorf("software plan must not force a pixel format, got %q", planFor("software", "", true, 1800).ProxyFilter)
	}
}

// Under WSL2 the GPU is not /dev/nvidia0 and nvidia-smi is not on PATH, so the
// two original probes both miss a machine whose NVENC works. Detecting the
// driver's encoder library is what closes that gap.
func TestCUDADetectedFromWSLDriverLibrary(t *testing.T) {
	if _, err := os.Stat("/dev/nvidia0"); err == nil {
		t.Skip("host has a native NVIDIA device node; the WSL path is not what would be exercised")
	}
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		t.Skip("host has nvidia-smi on PATH; the WSL path is not what would be exercised")
	}

	missing := filepath.Join(t.TempDir(), "libnvidia-encode.so.1")
	original := wslNVENCLibrary
	t.Cleanup(func() { wslNVENCLibrary = original })

	wslNVENCLibrary = missing
	if deviceAvailable("cuda", "") {
		t.Fatal("cuda must not be reported available when the encoder library is absent")
	}

	if err := os.WriteFile(missing, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !deviceAvailable("cuda", "") {
		t.Fatal("cuda must be reported available once the WSL encoder library is present")
	}
}

func TestHardwarePlansUseExpectedFFmpegProfiles(t *testing.T) {
	cases := []struct {
		mode, wantEncoder, wantDecoder string
	}{
		{"software", "libx264", ""},
		{"cuda", "h264_nvenc", "cuda"},
		{"qsv", "h264_qsv", "qsv"},
		{"vaapi", "h264_vaapi", "vaapi"},
		{"videotoolbox", "h264_videotoolbox", "videotoolbox"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			plan := planFor(tc.mode, "/dev/dri/renderD128", true, 1800)
			if !strings.Contains(strings.Join(plan.EncoderArgs, " "), tc.wantEncoder) {
				t.Fatalf("encoder args=%v want %s", plan.EncoderArgs, tc.wantEncoder)
			}
			if tc.wantDecoder != "" && !strings.Contains(strings.Join(plan.DecoderArgs, " "), tc.wantDecoder) {
				t.Fatalf("decoder args=%v want %s", plan.DecoderArgs, tc.wantDecoder)
			}
			if !plan.AllowFallback || plan.BitrateKbps != 1800 {
				t.Fatalf("plan=%+v", plan)
			}
		})
	}
}

func TestHardwareProfileSeparatesAccelerators(t *testing.T) {
	if planFor("cuda", "", true, 1800).Profile() == planFor("software", "", true, 1800).Profile() {
		t.Fatal("hardware and software artifacts must have separate profiles")
	}
}

// The static checks — FFmpeg lists the encoder, a device node exists — are
// satisfied by configurations that cannot encode a single frame. A build whose
// NVENC API is newer than the installed driver is the case that prompted this:
// it lists h264_nvenc, the device is present, and every encode fails. Without a
// real probe the plan selects that accelerator, then silently falls back to
// software while the artifact keeps the accelerator's name.
func TestEncoderRunsDistinguishesRunnableFromMerelyListed(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required for this probe test")
	}
	ctx := context.Background()

	if _, ok := probeEncoder(ctx, "cuda", "h264_definitely_not_a_real_encoder", ""); ok {
		t.Error("an encoder FFmpeg does not have must not probe as runnable")
	}
	if _, ok := probeEncoder(ctx, "cuda", "", ""); ok {
		t.Error("an empty encoder name must not probe as runnable")
	}
	// libx264 stands in for "an encoder that really does run here", so the
	// probe is shown to be capable of answering yes and is not simply refusing
	// everything.
	driver, ok := probeEncoder(ctx, "software", "libx264", "")
	if !ok {
		t.Error("libx264 must probe as runnable wherever ffmpeg is installed")
	}
	if driver != "" {
		t.Errorf("a non-libva backend must not report a libva driver, got %q", driver)
	}
}

// A machine whose libva default is wrong is the common Intel-on-NAS case, and
// the only visible symptom is that every hardware encode fails. The probe has
// to keep trying named drivers rather than concluding the GPU is unusable, and
// the name it settles on has to reach the encode command — an override that
// stops at the report would leave the plan selecting an accelerator that only
// works in the probe.
func TestProbedLibvaDriverReachesTheEncodeCommand(t *testing.T) {
	if got := libvaDriverEnv(""); got != nil {
		t.Errorf("libva's own choice must not be overridden, got %v", got)
	}
	got := libvaDriverEnv("iHD")
	if len(got) != 1 || got[0] != "LIBVA_DRIVER_NAME=iHD" {
		t.Errorf("probed driver must be passed as LIBVA_DRIVER_NAME, got %v", got)
	}
	if libvaDriverCandidates[0] != "" {
		t.Error("the first candidate must be libva's own choice so a correct machine pays one probe")
	}
	var named []string
	for _, candidate := range libvaDriverCandidates[1:] {
		named = append(named, candidate)
	}
	if len(named) < 2 {
		t.Errorf("both Intel driver generations must be attempted, got %v", named)
	}
}

// The user's stated requirement is to tell "no GPU" apart from "GPU present
// but the driver is broken" by machine-checkable logic, not just by reading
// two numbers side by side. remedyFor is where every combination of
// RuntimeAvailable/DeviceOpenable/Verified resolves to that verdict, so this
// exercises it directly instead of depending on the test machine actually
// being in each of the four states — most CI and dev machines have none of
// cuda/qsv/vaapi's hardware at all.
func TestRemedyForDistinguishesFourStatesAndEmptyMeansWorkingOrUntested(t *testing.T) {
	base := HardwareCapability{Backend: "vaapi", DecodeAvailable: true, EncodeAvailable: true, Encoder: "h264_vaapi", Device: "/dev/dri/renderD128"}

	noDevice := base
	noDevice.RuntimeAvailable = false
	noDeviceRemedy := remedyFor(noDevice, false)
	if noDeviceRemedy == "" {
		t.Fatal("no device visible at all must produce an actionable remedy")
	}

	notOpenable := base
	notOpenable.RuntimeAvailable = true
	notOpenable.DeviceOpenable = false
	permissionRemedy := remedyFor(notOpenable, false)
	if permissionRemedy == "" {
		t.Fatal("device present but not openable by this process must produce an actionable remedy")
	}

	failedProbe := base
	failedProbe.RuntimeAvailable = true
	failedProbe.DeviceOpenable = true
	failedProbeRemedy := remedyFor(failedProbe, true)
	if failedProbeRemedy == "" {
		t.Fatal("device openable but the encode failed must produce an actionable remedy")
	}

	// Same RuntimeAvailable/DeviceOpenable/Verified combination as
	// failedProbe above, but probed=false: this backend was simply never
	// tried because an earlier one in the preference order already won (or
	// this mode never asked for it). It must not borrow failedProbe's
	// message — that would tell an operator their driver is broken when
	// nothing was ever checked.
	unprobed := base
	unprobed.RuntimeAvailable = true
	unprobed.DeviceOpenable = true
	if got := remedyFor(unprobed, false); got != "" {
		t.Fatalf("a backend that was never probed must not get a remedy implying it is broken, got %q", got)
	}

	working := base
	working.RuntimeAvailable = true
	working.DeviceOpenable = true
	working.Verified = true
	if got := remedyFor(working, true); got != "" {
		t.Fatalf("a verified working backend must have an empty remedy, got %q", got)
	}

	distinct := map[string]string{"no-device": noDeviceRemedy, "permission-denied": permissionRemedy, "failed-probe": failedProbeRemedy}
	for a, textA := range distinct {
		for b, textB := range distinct {
			if a != b && textA == textB {
				t.Fatalf("%s and %s must not share the same remedy text -- an operator reading it needs to know which of the three problems they actually have, both read %q", a, b, textA)
			}
		}
	}
}

// os.Stat succeeds on a /dev/dri node this process has no rights to open —
// exactly the ordinary shape of a Docker container handed --device=/dev/dri
// without the matching render/video group. deviceOpenable exists to catch
// that before a single frame is encoded, so it has to fail where os.Stat
// would have quietly passed.
// permissionsAreEnforced reports whether this process is genuinely refused a
// file it holds no read bit for. It is a behavioural probe rather than an
// os.Getuid() == 0 check because those are different questions:
// CAP_DAC_OVERRIDE can sit in an ordinary process's *ambient* capability set
// — a common container default, and what this repository's dev shell hands to
// uid 1000 — and a process holding it opens a mode-000 file exactly as root
// would. A test that builds an unreadable file in order to assert a refusal
// has nothing left to assert there, and asserting it anyway makes the suite
// permanently red for an environment reason with no bearing on the code.
func permissionsAreEnforced(t *testing.T) bool {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "permission-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(probe, 0o644) })
	f, err := os.Open(probe)
	if err != nil {
		return true
	}
	_ = f.Close()
	return false
}

func TestDeviceOpenableDistinguishesPermissionFromAbsence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deviceOpenable mirrors deviceAvailable's always-present behavior on windows; there is no /dev node or permission state to probe there")
	}
	if !permissionsAreEnforced(t) {
		t.Skip("this process bypasses file permission bits (root, or CAP_DAC_OVERRIDE in the ambient set), so a node it cannot open cannot be built here")
	}

	missing := filepath.Join(t.TempDir(), "renderD128")
	if deviceOpenable("vaapi", missing) {
		t.Fatal("a device node that does not exist must not be reported openable")
	}

	locked := filepath.Join(t.TempDir(), "renderD128")
	if err := os.WriteFile(locked, []byte{}, 0o000); err != nil {
		t.Fatal(err)
	}
	if deviceOpenable("vaapi", locked) {
		t.Fatal("a device node this process has no read/write rights to must not be reported openable; os.Stat alone would have said it exists, which is exactly the gap DeviceOpenable closes")
	}

	openable := filepath.Join(t.TempDir(), "renderD128")
	if err := os.WriteFile(openable, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	if !deviceOpenable("vaapi", openable) {
		t.Fatal("a device node this process can read and write must be reported openable")
	}
}

// This is the regression the whole feature exists to fix: RuntimeAvailable
// used to be reset to false the moment a probe failed, which collapsed
// "device present, driver broken" into the exact same state as "no device at
// all" -- the one distinction the user explicitly asked to be able to tell
// apart. Forcing every probe to fail through the probeEncoderFunc seam makes
// this deterministic on any machine, including ones with no GPU backend to
// actually break.
func TestRuntimeAvailableSurvivesFailedProbe(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required for DetectHardware")
	}
	ctx := context.Background()
	baseline, _ := DetectHardware(ctx, HardwareConfig{Mode: "auto", AllowFallback: true})

	original := probeEncoderFunc
	t.Cleanup(func() { probeEncoderFunc = original })
	probeEncoderFunc = func(context.Context, string, string, string) (string, bool) { return "", false }

	forced, _ := DetectHardware(ctx, HardwareConfig{Mode: "auto", AllowFallback: true})

	if len(forced.Capabilities) != len(baseline.Capabilities) {
		t.Fatalf("capability list shape must not change between runs, got %d want %d", len(forced.Capabilities), len(baseline.Capabilities))
	}
	for i, want := range baseline.Capabilities {
		got := forced.Capabilities[i]
		if got.Verified {
			t.Fatalf("%s: probe was forced to fail, must not report Verified", got.Backend)
		}
		if got.RuntimeAvailable != want.RuntimeAvailable {
			t.Fatalf("%s: RuntimeAvailable changed from %t to %t after a failed probe; it must reflect only device presence, not encode success", got.Backend, want.RuntimeAvailable, got.RuntimeAvailable)
		}
	}
	if forced.SelectedMode != "software" {
		t.Fatalf("every backend's probe was forced to fail, selection must fall back to software, got %q", forced.SelectedMode)
	}
}

// SelectedBackends feeds remote.WorkerCapabilities.Hardware, which the Hub
// uses to route work toward a GPU Worker. A software-only report claiming a
// backend would make the Hub prefer a Worker for accelerated work it cannot
// actually do.
func TestSelectedBackendsReturnsNothingForSoftwareOnlyReport(t *testing.T) {
	software := HardwareReport{SelectedMode: "software"}
	if got := software.SelectedBackends(); got != nil {
		t.Fatalf("a software-only report must not claim a hardware backend, got %v", got)
	}
	empty := HardwareReport{}
	if got := empty.SelectedBackends(); got != nil {
		t.Fatalf("an empty/undetected report must not claim a hardware backend, got %v", got)
	}
	hardware := HardwareReport{SelectedMode: "vaapi"}
	if got := hardware.SelectedBackends(); len(got) != 1 || got[0] != "vaapi" {
		t.Fatalf("a selected hardware backend must be reported, got %v", got)
	}
}
