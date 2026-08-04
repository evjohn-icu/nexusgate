package volcasr

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/providers/common"
	"nhooyr.io/websocket"
)

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
		go func() {
			for {
				if _, _, readErr := conn.Read(context.Background()); readErr != nil {
					return
				}
			}
		}()
		time.Sleep(300 * time.Millisecond)
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
