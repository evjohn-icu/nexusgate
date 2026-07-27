package media

import "testing"

func TestRecognizeCaptureProfileUsesKnownExtensions(t *testing.T) {
	tests := []struct {
		name        string
		filename    string
		wantVendor  string
		wantFamily  string
		wantColor   string
		wantRaw     string
		wantSidecar bool
	}{
		{name: "Blackmagic BRAW", filename: "A001_0001.BRAW", wantVendor: "Blackmagic", wantFamily: "raw-video", wantRaw: "BRAW", wantSidecar: true},
		{name: "Canon CRM", filename: "C001.CRM", wantVendor: "Canon", wantFamily: "raw-video", wantRaw: "CRM", wantSidecar: true},
		{name: "Nikon N-RAW", filename: "NRAW_0001.NEV", wantVendor: "Nikon", wantFamily: "raw-video", wantRaw: "N-RAW", wantSidecar: false},
		{name: "RED R3D", filename: "A001_C001.R3D", wantVendor: "RED", wantFamily: "raw-video", wantRaw: "R3D", wantSidecar: true},
		{name: "ARRI ARRIRAW", filename: "A001C001.ARI", wantVendor: "ARRI", wantFamily: "raw-video", wantRaw: "ARI", wantSidecar: true},
		{name: "Insta360 INSV", filename: "VID_001.INSV", wantVendor: "Insta360", wantFamily: "360-video", wantSidecar: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RecognizeCaptureProfile(tt.filename, nil, nil)
			if got.Vendor != tt.wantVendor || got.MediaFamily != tt.wantFamily || got.ColorProfile != tt.wantColor || got.RawFormat != tt.wantRaw {
				t.Fatalf("profile=%+v want vendor=%q family=%q color=%q raw=%q", got, tt.wantVendor, tt.wantFamily, tt.wantColor, tt.wantRaw)
			}
			if got.HasSidecarCandidates != tt.wantSidecar {
				t.Fatalf("has sidecar candidates=%t want %t", got.HasSidecarCandidates, tt.wantSidecar)
			}
		})
	}
}

func TestRecognizeCaptureProfileUsesRawFFProbeAndExifFields(t *testing.T) {
	tests := []struct {
		name       string
		filename   string
		ffprobe    []string
		exif       []string
		wantVendor string
		wantFamily string
		wantColor  string
		wantRaw    string
	}{
		{
			name:       "DJI D-Log",
			filename:   "DJI_0001.MP4",
			ffprobe:    []string{"codec_name=h264", "profile=D-Log"},
			exif:       []string{"Make: DJI"},
			wantVendor: "DJI",
			wantFamily: "video",
			wantColor:  "D-Log",
		},
		{
			name:       "DJI HLG",
			filename:   "DJI_0002.MP4",
			ffprobe:    []string{"color_transfer=arib-std-b67"},
			exif:       []string{"CameraModelName: DJI Mavic 3"},
			wantVendor: "DJI",
			wantFamily: "video",
			wantColor:  "HLG",
		},
		{
			name:       "Sony S-Log and X-OCN",
			filename:   "C001.MXF",
			ffprobe:    []string{"codec_name=X-OCN", "color_transfer=S-Log3"},
			exif:       []string{"Make=SONY"},
			wantVendor: "Sony",
			wantFamily: "raw-video",
			wantColor:  "S-Log",
			wantRaw:    "X-OCN",
		},
		{
			name:       "Canon C-Log and CRM",
			filename:   "C001.MXF",
			ffprobe:    []string{"codec_tag_string=CRM", "color_transfer=C-Log3"},
			exif:       []string{"Manufacturer: Canon"},
			wantVendor: "Canon",
			wantFamily: "raw-video",
			wantColor:  "C-Log",
			wantRaw:    "CRM",
		},
		{
			name:       "Apple Log and ProRes RAW",
			filename:   "iPhone_0001.MOV",
			ffprobe:    []string{"codec_name=prores_raw", "color_transfer=Apple Log"},
			exif:       []string{"com.apple.quicktime.camera.log: Apple Log"},
			wantVendor: "Apple",
			wantFamily: "raw-video",
			wantColor:  "Apple Log",
			wantRaw:    "ProRes RAW",
		},
		{
			name:       "GoPro GP-Log",
			filename:   "GOPR0001.MP4",
			ffprobe:    []string{"profile=GP-Log"},
			exif:       []string{"Make: GoPro"},
			wantVendor: "GoPro",
			wantFamily: "video",
			wantColor:  "GP-Log",
		},
		{
			name:       "Panasonic V-Log",
			filename:   "P1000001.MOV",
			ffprobe:    []string{"color_transfer=V-Log"},
			exif:       []string{"Make: Panasonic"},
			wantVendor: "Panasonic",
			wantFamily: "video",
			wantColor:  "V-Log",
		},
		{
			name:       "Nikon N-Log",
			filename:   "NIKON_0001.MOV",
			ffprobe:    []string{"profile=N-Log"},
			exif:       []string{"Make=NIKON"},
			wantVendor: "Nikon",
			wantFamily: "video",
			wantColor:  "N-Log",
		},
		{
			name:       "Fujifilm F-Log",
			filename:   "FUJI_0001.MOV",
			ffprobe:    []string{"color_transfer=F-Log2"},
			exif:       []string{"Make: FUJIFILM"},
			wantVendor: "Fujifilm",
			wantFamily: "video",
			wantColor:  "F-Log",
		},
		{
			name:       "RED Log3G10",
			filename:   "A001_C001.MOV",
			ffprobe:    []string{"codec_name=redcode", "color_transfer=Log3G10"},
			exif:       []string{"Camera: RED V-RAPTOR"},
			wantVendor: "RED",
			wantFamily: "raw-video",
			wantColor:  "Log3G10",
			wantRaw:    "R3D",
		},
		{
			name:       "ARRI LogC",
			filename:   "A001_C001.MOV",
			ffprobe:    []string{"codec_name=arriraw", "color_transfer=LogC3"},
			exif:       []string{"CameraModelName: ARRI ALEXA 35"},
			wantVendor: "ARRI",
			wantFamily: "raw-video",
			wantColor:  "LogC",
			wantRaw:    "ARI",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RecognizeCaptureProfile(tt.filename, tt.ffprobe, tt.exif)
			if got.Vendor != tt.wantVendor || got.MediaFamily != tt.wantFamily || got.ColorProfile != tt.wantColor || got.RawFormat != tt.wantRaw {
				t.Fatalf("profile=%+v want vendor=%q family=%q color=%q raw=%q", got, tt.wantVendor, tt.wantFamily, tt.wantColor, tt.wantRaw)
			}
		})
	}
}

func TestRecognizeCaptureProfileReturnsKnownSidecarPatterns(t *testing.T) {
	tests := []struct {
		name      string
		filename  string
		metadata  []string
		wantMatch string
	}{
		{name: "BRAW sidecar", filename: "clip.braw", wantMatch: "<basename>.sidecar"},
		{name: "DJI subtitle sidecar", filename: "clip.mp4", metadata: []string{"Make=DJI", "Profile=D-Log"}, wantMatch: "<basename>.srt"},
		{name: "GoPro low resolution sidecar", filename: "clip.mp4", metadata: []string{"Make=GoPro", "Profile=GP-Log"}, wantMatch: "<basename>.lrv"},
		{name: "RED metadata sidecar", filename: "clip.r3d", wantMatch: "<basename>.rmd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RecognizeCaptureProfile(tt.filename, tt.metadata, nil)
			if !got.HasSidecarCandidates {
				t.Fatalf("profile=%+v has no sidecar candidates", got)
			}
			found := false
			for _, pattern := range got.SidecarCandidatePatterns {
				if pattern == tt.wantMatch {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("sidecar patterns=%v want %q", got.SidecarCandidatePatterns, tt.wantMatch)
			}
		})
	}
}

func TestRecognizeCaptureProfileLeavesUnsupportedMediaUnknown(t *testing.T) {
	got := RecognizeCaptureProfile("document.pdf", []string{"producer=Acme"}, []string{"Description: report"})
	if got.Vendor != "" || got.MediaFamily != "" || got.ColorProfile != "" || got.RawFormat != "" || got.HasSidecarCandidates || len(got.SidecarCandidatePatterns) != 0 {
		t.Fatalf("unsupported media profile=%+v want zero profile", got)
	}
}
