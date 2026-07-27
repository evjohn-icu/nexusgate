package stepfun

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ev/timingdex/internal/providers/common"
)

func TestTranscribeSSEFixture(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/step_plan/v1/audio/asr/sse" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer step-key" {
			t.Fatalf("authorization=%q", got)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("accept=%q", got)
		}
		var body struct {
			Audio struct {
				Data  string `json:"data"`
				Input struct {
					Transcription struct {
						Model    string `json:"model"`
						Language string `json:"language"`
					} `json:"transcription"`
				} `json:"input"`
			} `json:"audio"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Audio.Data == "" || body.Audio.Input.Transcription.Model != "stepaudio-2.5-asr" {
			t.Fatalf("body=%+v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"transcript.text.delta\",\"delta\":\"旧素材\",\"start_time\":10,\"end_time\":420}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"transcript.text.delta\",\"delta\":\"翻新\",\"start_time\":420,\"end_time\":900}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"transcript.text.done\",\"text\":\"旧素材翻新\"}\n\n"))
	}))
	defer server.Close()

	binDir := t.TempDir()
	ffmpeg := filepath.Join(binDir, "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nprintf fixture-pcm\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	audioPath := filepath.Join(t.TempDir(), "sample.m4a")
	if err := os.WriteFile(audioPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &ASR{
		Endpoint:  common.Endpoint{BaseURL: server.URL + "/step_plan/v1", APIKey: "step-key"},
		ModelName: "stepaudio-2.5-asr",
		Path:      "audio/asr/sse",
	}
	transcript, err := provider.Transcribe(context.Background(), common.TranscribeRequest{AudioPath: audioPath, Language: "zh"})
	if err != nil {
		t.Fatal(err)
	}
	if transcript.Text != "旧素材翻新" || len(transcript.Segments) != 2 {
		t.Fatalf("transcript=%+v", transcript)
	}
	if transcript.Segments[0].StartMS != 10 || transcript.Segments[1].EndMS != 900 {
		t.Fatalf("segments=%+v", transcript.Segments)
	}
}
