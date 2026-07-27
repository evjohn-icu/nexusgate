package volcasr

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
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
