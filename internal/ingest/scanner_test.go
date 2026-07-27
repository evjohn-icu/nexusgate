package ingest

import "testing"

func TestSupportedVideoRecognizesMainstreamCameraMedia(t *testing.T) {
	for _, path := range []string{
		"A001_08151512_C001.braw", // Blackmagic RAW
		"A001_C001_0101AB.r3d",    // REDCODE RAW
		"A001C001_240101.ari",     // ARRIRAW
		"C0001.crm",               // Canon Cinema RAW Light
		"frame.dng",               // CinemaDNG still frame
		"NRAW_0001.nev",           // Nikon N-RAW
		"DJI_0001.mp4",
		"C0001.mxf",             // Sony / Canon / Panasonic professional media
		"VID_20260726_001.insv", // Insta360 source
	} {
		if !supportedVideo(path) {
			t.Errorf("supportedVideo(%q) = false, want true", path)
		}
	}
}

func TestSupportedVideoRejectsSidecarsAndStillImages(t *testing.T) {
	for _, path := range []string{"DJI_0001.srt", "A001.sidecar", "IMG_0001.dng.xmp", "frame.jpg"} {
		if supportedVideo(path) {
			t.Errorf("supportedVideo(%q) = true, want false", path)
		}
	}
}
