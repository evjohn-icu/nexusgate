package volcasr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/providers/common"
	"nhooyr.io/websocket"
)

func TestASRErrorPaths(t *testing.T) {
	const apiKey = "volc-key-errtest"

	// Fake ffmpeg that returns minimal PCM audio data.
	binDir := t.TempDir()
	ffmpeg := filepath.Join(binDir, "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nprintf '\\x00\\x00\\x00\\x00'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	audioPath := filepath.Join(t.TempDir(), "sample.wav")
	if err := os.WriteFile(audioPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("server sends error frame", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Fatalf("accept: %v", err)
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "done")

			// Drain the client's config and audio frames.
			go func() {
				for {
					if _, _, readErr := conn.Read(context.Background()); readErr != nil {
						return
					}
				}
			}()

			// Give client time to send at least the config frame.
			time.Sleep(100 * time.Millisecond)

			// Send an error frame (messageType=0xF) with compressed JSON.
			errPayload, _ := json.Marshal(map[string]any{
				"code":    45000002,
				"message": "audio is too short",
			})
			errorFrame := buildFrame(0xF, 0x0, 0x1, gzipBytes(errPayload))
			if writeErr := conn.Write(context.Background(), websocket.MessageBinary, errorFrame); writeErr != nil {
				t.Fatalf("write error frame: %v", writeErr)
			}

			// Let client read the frame, then close normally.
			time.Sleep(100 * time.Millisecond)
		}))
		defer server.Close()

		asr := &ASR{
			URL:            "ws" + strings.TrimPrefix(server.URL, "http"),
			APIKey:         apiKey,
			TimeoutSeconds: 5,
		}
		_, err := asr.Transcribe(context.Background(), common.TranscribeRequest{AudioPath: audioPath, Language: "zh"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "Volcengine ASR error") {
			t.Fatalf("error should contain 'Volcengine ASR error', got: %v", err)
		}
		if !strings.Contains(err.Error(), "audio is too short") {
			t.Fatalf("error should contain error message from frame, got: %v", err)
		}
		if strings.Contains(err.Error(), apiKey) {
			t.Fatalf("error text leaks API key: %q", err.Error())
		}
	})

	t.Run("server closes with internal error status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Fatalf("accept: %v", err)
				return
			}
			// Drain the client's frames briefly.
			go func() {
				for i := 0; i < 3; i++ {
					conn.Read(context.Background()) //nolint:errcheck
				}
			}()
			time.Sleep(100 * time.Millisecond)
			// Close with an error status — this is NOT a normal closure.
			conn.Close(websocket.StatusInternalError, "internal server crash")
		}))
		defer server.Close()

		asr := &ASR{
			URL:            "ws" + strings.TrimPrefix(server.URL, "http"),
			APIKey:         apiKey,
			TimeoutSeconds: 5,
		}
		_, err := asr.Transcribe(context.Background(), common.TranscribeRequest{AudioPath: audioPath, Language: "zh"})
		if err == nil {
			t.Fatal("expected error on non-normal close, got nil")
		}
		if strings.Contains(err.Error(), apiKey) {
			t.Fatalf("error text leaks API key: %q", err.Error())
		}
	})
}
