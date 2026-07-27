package media

import (
	"strings"
	"testing"
)

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
