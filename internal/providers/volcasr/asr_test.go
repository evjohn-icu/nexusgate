package volcasr

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

func TestDecodeFrameRejectsOversizedPayload(t *testing.T) {
	// A frame whose header declares a payload length that exceeds the actual
	// remaining bytes. This exercises the len(frame) < offset+size guard.
	frame := make([]byte, 12)
	frame[0] = 0x11
	frame[1] = 0x10                             // messageType 1, flags 0
	frame[2] = 0x01                             // gzip compression
	binary.BigEndian.PutUint32(frame[4:8], 100) // claim 100-byte payload
	_, _, err := decodeFrame(frame)
	if err == nil {
		t.Fatal("expected error for oversized declared payload")
	}
	if !strings.Contains(err.Error(), "invalid payload length") {
		t.Fatalf("expected 'invalid payload length', got: %v", err)
	}
}

func TestBuildRequestFrameRoundTrip(t *testing.T) {
	frame := buildFrame(0x1, 0x0, 0x1, gzipBytes([]byte(`{"request":{"model_name":"doubao-seed-asr-2.0"}}`)))
	if got, want := frame[0], byte(0x11); got != want {
		t.Fatalf("protocol header = %#x, want %#x", got, want)
	}
	if got, want := frame[1]>>4, byte(0x1); got != want {
		t.Fatalf("message type = %#x, want %#x", got, want)
	}
	if got, want := int(binary.BigEndian.Uint32(frame[4:8])), len(frame)-8; got != want {
		t.Fatalf("payload length = %d, want %d", got, want)
	}
	payload, typ, err := decodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if typ != 0x1 {
		t.Fatalf("decoded type = %#x", typ)
	}
	if !bytes.Contains(payload, []byte("doubao-seed-asr-2.0")) {
		t.Fatalf("unexpected payload %s", payload)
	}
}

func TestDecodeServerResponseAndText(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"code": 20000000, "result": map[string]any{"text": "广州夜景素材"}})
	// Server full response, no sequence field, JSON + gzip.
	frame := buildFrame(0x9, 0x0, 0x1, gzipBytes(raw))
	payload, typ, err := decodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if typ != 0x9 {
		t.Fatalf("decoded type = %#x", typ)
	}
	text, providerErr := transcriptText(payload)
	if providerErr != "" {
		t.Fatalf("unexpected provider error %q", providerErr)
	}
	if text != "广州夜景素材" {
		t.Fatalf("text = %q", text)
	}
}

func TestDecodeServerError(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"code": 45000002, "message": "empty audio"})
	text, providerErr := transcriptText(raw)
	if text != "" || providerErr != "empty audio" {
		t.Fatalf("text=%q providerErr=%q", text, providerErr)
	}
}

// The error-frame path in Transcribe reaches jobs.last_error_message, the same
// way common.ReadError does. A long server response must be truncated so a
// relay that echoes the request cannot park a full Provider key in SQLite.
func TestTranscribeTruncatesLongErrorFrame(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to decode the fixture to PCM")
	}
	dir := t.TempDir()
	audio := filepath.Join(dir, "silence.wav")
	if output, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono", "-t", "0.3", audio).CombinedOutput(); err != nil {
		t.Skipf("test fixture cannot be encoded by local ffmpeg: %v: %s", err, output)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		// Drain the config frame, then send a long error frame.
		if _, _, readErr := conn.Read(context.Background()); readErr != nil {
			return
		}
		longPayload := strings.Repeat("x", 4096)
		// type 0xF (error), no flags, no compression.
		frame := make([]byte, 8+len(longPayload))
		frame[0] = 0x11
		frame[1] = 0xF << 4
		binary.BigEndian.PutUint32(frame[4:8], uint32(len(longPayload)))
		copy(frame[8:], longPayload)
		conn.Write(context.Background(), websocket.MessageBinary, frame)
	}))
	defer server.Close()

	asr := &ASR{URL: "ws" + strings.TrimPrefix(server.URL, "http"), APIKey: "test-key", TimeoutSeconds: 30}
	_, err := asr.Transcribe(context.Background(), common.TranscribeRequest{AudioPath: audio, Language: "zh"})
	if err == nil {
		t.Fatal("expected an error from the error frame")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "Volcengine ASR error") {
		t.Fatalf("error message should mention Volcengine ASR, got: %s", errStr)
	}
	// The payload should be truncated.
	if strings.Contains(errStr, strings.Repeat("x", 4096)) {
		t.Fatal("error payload must be truncated, but full 4096-byte payload found")
	}
	if !strings.Contains(errStr, "…(truncated)") {
		t.Fatal("truncated payload must be marked")
	}
}

// Seed ASR closes the socket normally once it has sent its last sequence.
// Audio with no speech in it reaches that close without ever producing a
// transcript frame, and the correct answer is an empty transcript — not a
// failed job. Every clip whose only sound is wind or room tone lands here,
// and the speech gate cannot tell those from speech in advance.
func TestTranscribeTreatsNormalClosureAsEmptyTranscript(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to decode the fixture to PCM")
	}
	dir := t.TempDir()
	audio := filepath.Join(dir, "silence.wav")
	if output, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono", "-t", "0.3", audio).CombinedOutput(); err != nil {
		t.Skipf("test fixture cannot be encoded by local ffmpeg: %v: %s", err, output)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Drain the config frame and the 200ms audio chunks. The close has to
		// come from a healthy connection, so it is issued here rather than
		// after a cancelled read — a read that fails first would tear the
		// socket down and the client would see EOF instead of a close frame.
		configReceived := make(chan struct{})
		go func() {
			for {
				if _, _, readErr := conn.Read(context.Background()); readErr != nil {
					return
				}
				close(configReceived)
				return
			}
		}()
		select {
		case <-configReceived:
		case <-time.After(time.Second):
			t.Error("timed out waiting for ASR config frame")
			return
		}
		conn.Close(websocket.StatusNormalClosure, "finish last sequence")
	}))
	defer server.Close()

	asr := &ASR{URL: "ws" + strings.TrimPrefix(server.URL, "http"), APIKey: "test-key", TimeoutSeconds: 30}
	transcript, err := asr.Transcribe(context.Background(), common.TranscribeRequest{AudioPath: audio, Language: "zh"})
	if err != nil {
		t.Fatalf("a normal closure must not fail the job: %v", err)
	}
	if transcript.Text != "" {
		t.Fatalf("expected an empty transcript, got %q", transcript.Text)
	}
	if transcript.Language != "zh" {
		t.Fatalf("language = %q, want zh", transcript.Language)
	}
}

func TestASRFormatDoesNotLeakAPIKey(t *testing.T) {
	asr := ASR{
		URL:            "wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_nostream",
		APIKey:         "sk-volc-secret-do-not-leak",
		ResourceID:     "volc.seedasr.sauc.duration",
		RequestModel:   "bigmodel",
		ModelName:      "doubao-seed-asr-2.0",
		UID:            "timingdex",
		TimeoutSeconds: 30,
	}

	secret := "sk-volc-secret-do-not-leak"
	for _, tc := range []struct {
		name string
		out  string
	}{
		{name: "%v", out: fmt.Sprintf("%v", asr)},
		{name: "%+v", out: fmt.Sprintf("%+v", asr)},
		{name: "%s", out: fmt.Sprintf("%s", asr)},
		{name: "%#v", out: fmt.Sprintf("%#v", asr)},
	} {
		if strings.Contains(tc.out, secret) {
			t.Errorf("%s leaked API key: %s", tc.name, tc.out)
		}
		if !strings.Contains(tc.out, "[redacted]") {
			t.Errorf("%s did not redact APIKey: %s", tc.name, tc.out)
		}
	}
}

// P1: ASR GoString must sanitise URL-embedded credentials (userinfo,
// sensitive query params).
func TestASRFormatSanitizesURL(t *testing.T) {
	asr := ASR{
		URL:            "wss://user:pass@openspeech.bytedance.com/api/v3?api_key=sk-leaked&mode=test",
		APIKey:         "sk-volc-secret",
		ResourceID:     "volc.seedasr.sauc.duration",
		RequestModel:   "bigmodel",
		ModelName:      "doubao-seed-asr-2.0",
		UID:            "timingdex",
		TimeoutSeconds: 30,
	}

	for _, tc := range []struct {
		name string
		out  string
	}{
		{name: "%v", out: fmt.Sprintf("%v", asr)},
		{name: "%+v", out: fmt.Sprintf("%+v", asr)},
		{name: "%s", out: fmt.Sprintf("%s", asr)},
		{name: "%#v", out: fmt.Sprintf("%#v", asr)},
	} {
		if strings.Contains(tc.out, "user:pass") {
			t.Errorf("%s leaked URL userinfo: %s", tc.name, tc.out)
		}
		if strings.Contains(tc.out, "sk-leaked") {
			t.Errorf("%s leaked URL api_key: %s", tc.name, tc.out)
		}
		if !strings.Contains(tc.out, "mode=test") {
			t.Errorf("%s lost non-sensitive query param: %s", tc.name, tc.out)
		}
		if !strings.Contains(tc.out, "openspeech.bytedance.com") {
			t.Errorf("%s lost hostname: %s", tc.name, tc.out)
		}
	}
}
