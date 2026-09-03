package qwen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/providers/common"
)

func TestTranscribeOpenAICompatibleFixture(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer qwen-key" {
			t.Fatalf("authorization=%q", got)
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Content []struct {
					Type       string `json:"type"`
					InputAudio struct {
						Data string `json:"data"`
					} `json:"input_audio"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "qwen3-asr-flash" || len(body.Messages) != 1 || len(body.Messages[0].Content) != 1 {
			t.Fatalf("body=%+v", body)
		}
		audio := body.Messages[0].Content[0]
		if audio.Type != "input_audio" || !strings.HasPrefix(audio.InputAudio.Data, "data:audio/wav;base64,") {
			t.Fatalf("audio=%+v", audio)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"这是一段旧素材"}}]}`))
	}))
	defer server.Close()

	audioPath := filepath.Join(t.TempDir(), "sample.wav")
	if err := os.WriteFile(audioPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &ASR{
		Endpoint:  common.Endpoint{BaseURL: server.URL + "/v1", APIKey: "qwen-key"},
		ModelName: "qwen3-asr-flash",
	}
	transcript, err := provider.Transcribe(context.Background(), common.TranscribeRequest{AudioPath: audioPath, Language: "zh"})
	if err != nil {
		t.Fatal(err)
	}
	if transcript.Text != "这是一段旧素材" || transcript.Language != "zh" {
		t.Fatalf("transcript=%+v", transcript)
	}
}
