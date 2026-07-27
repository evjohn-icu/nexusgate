package media

import (
	"encoding/json"
	"testing"
)

func TestClassifySourceColorFromProbeExifAndPath(t *testing.T) {
	tests := []struct {
		name  string
		probe string
		exif  map[string]any
		path  string
		want  SourceColorClass
	}{
		{
			name:  "ordinary video is SDR",
			probe: `{"streams":[{"codec_type":"video","color_transfer":"bt709","color_space":"bt709","color_primaries":"bt709"}]}`,
			path:  "clip.mp4",
			want:  SourceColorSDR,
		},
		{
			name:  "HLG transfer is HDR HLG",
			probe: `{"streams":[{"codec_type":"video","color_transfer":"arib-std-b67","color_space":"bt2020nc"}]}`,
			path:  "clip.mov",
			want:  SourceColorHDRHLG,
		},
		{
			name:  "PQ transfer is HDR PQ",
			probe: `{"streams":[{"codec_type":"video","color_transfer":"smpte2084","color_space":"bt2020nc"}]}`,
			path:  "clip.mov",
			want:  SourceColorHDRPQ,
		},
		{
			name:  "HDR10 tag implies PQ",
			probe: `{"streams":[{"codec_type":"video","color_transfer":"unknown"}],"format":{"tags":{"HDR_Format":"HDR10"}}}`,
			path:  "clip.mp4",
			want:  SourceColorHDRPQ,
		},
		{
			name:  "Apple Log transfer is Apple Log",
			probe: `{"streams":[{"codec_type":"video","color_transfer":"apple-log"}]}`,
			path:  "iphone.mov",
			want:  SourceColorLogApple,
		},
		{
			name:  "Apple Log tag is Apple Log",
			probe: `{"streams":[{"codec_type":"video","color_transfer":"unknown"}]}`,
			exif:  map[string]any{"ColorProfile": "Apple Log"},
			path:  "iphone.mov",
			want:  SourceColorLogApple,
		},
		{
			name:  "generic log is unknown log",
			probe: `{"streams":[{"codec_type":"video","color_transfer":"log"}]}`,
			path:  "camera.mov",
			want:  SourceColorLogUnknown,
		},
		{
			name:  "raw extension wins over probe metadata",
			probe: `{"streams":[{"codec_type":"video","color_transfer":"smpte2084"}]}`,
			path:  "CAMERA.BRAW",
			want:  SourceColorRAW,
		},
		{name: "r3d extension is raw", path: "clip.R3D", want: SourceColorRAW},
		{name: "dng extension is raw", path: "frame.DNG", want: SourceColorRAW},
		{name: "nev extension is raw", path: "frame.NEV", want: SourceColorRAW},
		{name: "ari extension is raw", path: "frame.ARI", want: SourceColorRAW},
		{name: "crm extension is raw", path: "frame.CRM", want: SourceColorRAW},
		{
			name: "no evidence is unknown",
			path: "metadata.bin",
			want: SourceColorUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var probe FFProbeResult
			if tt.probe != "" {
				if err := json.Unmarshal([]byte(tt.probe), &probe); err != nil {
					t.Fatal(err)
				}
			}
			if got := ClassifySourceColor(probe, tt.exif, tt.path); got != tt.want {
				t.Fatalf("classify source color=%q want %q", got, tt.want)
			}
		})
	}
}

func TestClassifySourceColorUsesFormatTags(t *testing.T) {
	var probe FFProbeResult
	if err := json.Unmarshal([]byte(`{
		"streams": [{"codec_type":"video","color_transfer":"unknown"}],
		"format": {"tags": {"com.apple.quicktime.camera.log": "Apple Log"}}
	}`), &probe); err != nil {
		t.Fatal(err)
	}
	if got := ClassifySourceColor(probe, nil, "clip.mov"); got != SourceColorLogApple {
		t.Fatalf("format Apple Log tag classified as %q", got)
	}
}

func TestClassifySourceColorDetectsRawProbeCodec(t *testing.T) {
	var probe FFProbeResult
	if err := json.Unmarshal([]byte(`{"streams":[{"codec_type":"video","codec_name":"braw"}]}`), &probe); err != nil {
		t.Fatal(err)
	}
	if got := ClassifySourceColor(probe, nil, "clip.mov"); got != SourceColorRAW {
		t.Fatalf("RAW probe codec classified as %q", got)
	}
}

func TestClassifySourceColorUsesExifHDRAndLogTags(t *testing.T) {
	tests := []struct {
		name string
		exif map[string]any
		want SourceColorClass
	}{
		{name: "HLG", exif: map[string]any{"ColorTransfer": "HLG"}, want: SourceColorHDRHLG},
		{name: "PQ", exif: map[string]any{"TransferFunction": "SMPTE ST 2084"}, want: SourceColorHDRPQ},
		{name: "unknown log", exif: map[string]any{"Gamma": "LogC3"}, want: SourceColorLogUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifySourceColor(FFProbeResult{}, tt.exif, "still.jpg"); got != tt.want {
				t.Fatalf("EXIF classification=%q want %q", got, tt.want)
			}
		})
	}
}

func TestPreviewRenderPlanForSourceColor(t *testing.T) {
	tests := []struct {
		name       string
		class      SourceColorClass
		wantMode   PreviewRenderMode
		wantFilter string
		wantLUT    bool
		wantAvail  bool
	}{
		{name: "SDR direct", class: SourceColorSDR, wantMode: PreviewRenderDirect, wantAvail: true},
		{name: "HLG tone map", class: SourceColorHDRHLG, wantMode: PreviewRenderToneMap, wantFilter: "tonemap", wantAvail: true},
		{name: "PQ tone map", class: SourceColorHDRPQ, wantMode: PreviewRenderToneMap, wantFilter: "tonemap", wantAvail: true},
		{name: "Apple Log needs LUT", class: SourceColorLogApple, wantMode: PreviewRenderLUT, wantLUT: true, wantAvail: false},
		{name: "unknown Log needs transform", class: SourceColorLogUnknown, wantMode: PreviewRenderLUT, wantLUT: true, wantAvail: false},
		{name: "RAW unavailable", class: SourceColorRAW, wantMode: PreviewRenderUnavailable, wantAvail: false},
		{name: "unknown direct fallback", class: SourceColorUnknown, wantMode: PreviewRenderDirect, wantAvail: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SelectPreviewRenderPlan(tt.class)
			if got.SourceColor != tt.class || got.Mode != tt.wantMode || got.Filter != tt.wantFilter || got.RequiresLUT != tt.wantLUT || got.Available != tt.wantAvail {
				t.Fatalf("preview plan=%+v want mode=%q filter=%q lut=%t available=%t", got, tt.wantMode, tt.wantFilter, tt.wantLUT, tt.wantAvail)
			}
		})
	}
}

func TestPreviewRenderPlanFromSourceMetadata(t *testing.T) {
	var probe FFProbeResult
	if err := json.Unmarshal([]byte(`{"streams":[{"codec_type":"video","color_transfer":"arib-std-b67"}]}`), &probe); err != nil {
		t.Fatal(err)
	}
	plan := PreviewRenderPlanFor(probe, nil, "clip.mov")
	if plan.SourceColor != SourceColorHDRHLG || plan.Mode != PreviewRenderToneMap {
		t.Fatalf("preview plan=%+v", plan)
	}
}

func TestFFProbeResultCapturesColorFields(t *testing.T) {
	var probe FFProbeResult
	if err := json.Unmarshal([]byte(`{"streams":[{
		"codec_type":"video",
		"pix_fmt":"yuv420p10le",
		"profile":"High 10",
		"color_space":"bt2020nc",
		"color_transfer":"smpte2084",
		"color_primaries":"bt2020",
		"bits_per_raw_sample":"10",
		"codec_tag_string":"hvc1"
	}]}`), &probe); err != nil {
		t.Fatal(err)
	}
	stream := probe.Streams[0]
	if stream.PixFmt != "yuv420p10le" || stream.Profile != "High 10" || stream.ColorSpace != "bt2020nc" || stream.ColorTransfer != "smpte2084" || stream.ColorPrimaries != "bt2020" || stream.BitsPerRawSample != "10" || stream.CodecTagString != "hvc1" {
		t.Fatalf("decoded stream color fields missing: %+v", stream)
	}
}
