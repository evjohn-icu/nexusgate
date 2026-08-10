package media

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNormalizeMetadataPersistsAppleLogPreviewRequirement(t *testing.T) {
	probe := FFProbeResult{}
	probe.Format.Filename = "apple-log.mov"
	probe.Streams = []FFProbeStream{{CodecType: "video", CodecName: "hevc", PixFmt: "yuv420p10le", ColorTransfer: "apple_log", ColorPrimaries: "bt2020", BitsPerRawSample: "10"}}
	metadata := NormalizeMetadata(probe, map[string]any{})
	if metadata.SourceColor != string(SourceColorLogApple) || !metadata.PreviewRequiresLUT || metadata.PreviewAvailable {
		t.Fatalf("metadata=%+v", metadata)
	}
	if metadata.BitDepth != 10 || metadata.ColorPrimaries != "bt2020" {
		t.Fatalf("color metadata not preserved: %+v", metadata)
	}
}

func TestNormalizeMetadataUsesEmbeddedCaptureTimeBeforeFilesystemFallback(t *testing.T) {
	metadata := NormalizeMetadata(FFProbeResult{}, map[string]any{
		"CreateDate": "2026:07:26 14:15:16+08:00",
	})
	if metadata.CapturedAt == nil {
		t.Fatal("embedded CreateDate was not normalized into captured_at")
	}
	want := time.Date(2026, 7, 26, 6, 15, 16, 0, time.UTC)
	if !metadata.CapturedAt.Equal(want) {
		t.Fatalf("captured_at=%s, want %s", metadata.CapturedAt, want)
	}
	if metadata.CaptureTimeSource != "embedded_exif" || metadata.CaptureTimeConfidence != .95 {
		t.Fatalf("capture provenance=%q/%v", metadata.CaptureTimeSource, metadata.CaptureTimeConfidence)
	}
}

func TestNormalizeMetadataProvenanceFallbacksAndCompleteCoordinates(t *testing.T) {
	probe := FFProbeResult{}
	probe.Format.Tags = map[string]string{"creation_time": "2026-07-26T14:15:16Z", "gps_latitude": "35.0", "gps_longitude": "139.0"}
	metadata := NormalizeMetadata(probe, map[string]any{"FileModifyDate": "2026:07:25 14:15:16"})
	if metadata.CaptureTimeSource != "embedded_probe" || metadata.CaptureTimeConfidence != .75 {
		t.Fatalf("probe provenance=%q/%v", metadata.CaptureTimeSource, metadata.CaptureTimeConfidence)
	}
	if metadata.Latitude == nil || metadata.Longitude == nil || metadata.LocationSource != "embedded_probe" || metadata.LocationPrecision != "exact" {
		t.Fatalf("probe coordinates=%+v", metadata)
	}
	metadata = NormalizeMetadata(FFProbeResult{}, map[string]any{"FileModifyDate": "2026:07:25 14:15:16"})
	if metadata.CaptureTimeSource != "filesystem" || metadata.CaptureTimeConfidence != .25 {
		t.Fatalf("filesystem provenance=%q/%v", metadata.CaptureTimeSource, metadata.CaptureTimeConfidence)
	}
	metadata = NormalizeMetadata(FFProbeResult{}, map[string]any{"GPSLatitude": 35.0})
	if metadata.Latitude != nil || metadata.Longitude != nil {
		t.Fatalf("partial GPS pair should be absent: %+v", metadata)
	}
}

func TestNormalizeMetadataPersistsCameraCaptureProfile(t *testing.T) {
	var probe FFProbeResult
	probe.Format.Filename = "DJI_0001.mp4"
	probe.Streams = []FFProbeStream{{CodecType: "video", CodecName: "h264", Profile: "D-Log"}}
	metadata := NormalizeMetadata(probe, map[string]any{
		"Make":            "DJI",
		"CameraModelName": "DJI Mavic 3",
		"SerialNumber":    "DJI-SERIAL-1",
	})
	if metadata.CaptureVendor != "DJI" || metadata.ColorProfile != "D-Log" {
		t.Fatalf("capture profile vendor=%q color=%q", metadata.CaptureVendor, metadata.ColorProfile)
	}
	if metadata.CameraMake != "DJI" || metadata.CameraModel != "DJI Mavic 3" || metadata.CameraSerial != "DJI-SERIAL-1" {
		t.Fatalf("camera identity=%+v", metadata)
	}
	if metadata.PreviewStatus != "lut_required" {
		t.Fatalf("preview status=%q, want lut_required", metadata.PreviewStatus)
	}
}

func TestNormalizeMetadataExtractsCaptureFactsFromFFProbeAndExif(t *testing.T) {
	var probe FFProbeResult
	if err := json.Unmarshal([]byte(`{
		"format": {
			"filename": "A001_C003.r3d",
			"tags": {
				"creation_time": "2026-07-26T14:15:16+08:00",
				"reel_name": "REEL-07",
				"clip_name": "C003",
				"timecode": "01:02:03:04",
				"raw_format": "R3D",
				"color_profile": "Log3G10"
			}
		},
		"streams": [{
			"codec_type": "video",
			"codec_name": "redcode",
			"tags": {"lens_model": "Angenieux EZ-1 30-90"}
		}]
	}`), &probe); err != nil {
		t.Fatal(err)
	}
	metadata := NormalizeMetadata(probe, map[string]any{
		"Make":         "RED Digital Cinema",
		"Model":        "V-RAPTOR",
		"SerialNumber": "RAPTOR-42",
	})

	if metadata.CapturedAt == nil {
		t.Fatal("ffprobe creation_time was not normalized into captured_at")
	}
	wantCapturedAt := time.Date(2026, 7, 26, 6, 15, 16, 0, time.UTC)
	if !metadata.CapturedAt.Equal(wantCapturedAt) {
		t.Fatalf("captured_at=%s, want %s", metadata.CapturedAt, wantCapturedAt)
	}
	if metadata.CameraMake != "RED Digital Cinema" || metadata.CameraModel != "V-RAPTOR" || metadata.CameraSerial != "RAPTOR-42" {
		t.Fatalf("camera identity=%+v", metadata)
	}
	if metadata.CaptureVendor != "RED" || metadata.RawFormat != "R3D" || metadata.ColorProfile != "Log3G10" {
		t.Fatalf("capture profile=%+v", metadata)
	}
	if metadata.PreviewRenderMode == string(PreviewRenderDirect) || metadata.PreviewAvailable {
		t.Fatalf("RAW/Log sources must not render direct: %+v", metadata)
	}

	// These fields are part of the JSON contract consumed by the v0.15 capture
	// ingestion boundary, so the test remains focused on the public metadata
	// shape rather than a particular internal lookup helper.
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"lens_model":      "Angenieux EZ-1 30-90",
		"reel":            "REEL-07",
		"clip":            "C003",
		"source_timecode": "01:02:03:04",
	} {
		if got, _ := values[key].(string); got != want {
			t.Fatalf("metadata[%q]=%q, want %q; metadata=%s", key, got, want, raw)
		}
	}
}
