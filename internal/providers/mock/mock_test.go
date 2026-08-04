package mock

import (
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

func TestAnalyze_AssetType(t *testing.T) {
	meta := domain.MediaMetadata{Orientation: "landscape", HasAudio: true}

	// hasSpeech=true → talking_to_camera
	result := Analyze("clip.mp4", meta, true)
	if result.AssetType != "talking_to_camera" {
		t.Errorf("expected talking_to_camera, got %s", result.AssetType)
	}

	// hasSpeech=false → b_roll
	result = Analyze("clip.mp4", meta, false)
	if result.AssetType != "b_roll" {
		t.Errorf("expected b_roll, got %s", result.AssetType)
	}

	// Filename containing "screen" → screen_recording (overrides hasSpeech).
	result = Analyze("/path/to/screen_recording.mp4", meta, true)
	if result.AssetType != "screen_recording" {
		t.Errorf("expected screen_recording, got %s", result.AssetType)
	}
}

func TestAnalyze_AudioType(t *testing.T) {
	// HasAudio=false → silence
	meta := domain.MediaMetadata{Orientation: "landscape", HasAudio: false}
	result := Analyze("clip.mp4", meta, false)
	if result.AudioType != "silence" {
		t.Errorf("expected silence, got %s", result.AudioType)
	}

	// HasAudio=true, hasSpeech=true → speech
	meta.HasAudio = true
	result = Analyze("clip.mp4", meta, true)
	if result.AudioType != "speech" {
		t.Errorf("expected speech, got %s", result.AudioType)
	}

	// HasAudio=true, hasSpeech=false → ambient
	result = Analyze("clip.mp4", meta, false)
	if result.AudioType != "ambient" {
		t.Errorf("expected ambient, got %s", result.AudioType)
	}
}

func TestAnalyze_ShotSize(t *testing.T) {
	meta := domain.MediaMetadata{Orientation: "portrait", HasAudio: true}
	result := Analyze("clip.mp4", meta, false)
	if result.ShotSize != "medium" {
		t.Errorf("expected medium for portrait, got %s", result.ShotSize)
	}

	meta.Orientation = "landscape"
	result = Analyze("clip.mp4", meta, false)
	if result.ShotSize != "wide" {
		t.Errorf("expected wide for landscape, got %s", result.ShotSize)
	}
}

func TestAnalyze_HasSpeechPassThrough(t *testing.T) {
	meta := domain.MediaMetadata{Orientation: "landscape", HasAudio: true}
	result := Analyze("clip.mp4", meta, true)
	if !result.HasSpeech {
		t.Error("expected HasSpeech=true")
	}
	result = Analyze("clip.mp4", meta, false)
	if result.HasSpeech {
		t.Error("expected HasSpeech=false")
	}
}
