package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

	if encoderRuns(ctx, "cuda", "h264_definitely_not_a_real_encoder", "") {
		t.Error("an encoder FFmpeg does not have must not probe as runnable")
	}
	if encoderRuns(ctx, "cuda", "", "") {
		t.Error("an empty encoder name must not probe as runnable")
	}
	// libx264 stands in for "an encoder that really does run here", so the
	// probe is shown to be capable of answering yes and is not simply refusing
	// everything.
	if !encoderRuns(ctx, "software", "libx264", "") {
		t.Error("libx264 must probe as runnable wherever ffmpeg is installed")
	}
}
