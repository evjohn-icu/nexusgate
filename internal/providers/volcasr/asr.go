// Package volcasr implements the native Doubao Seed ASR 2.0 protocol used by
// Volcengine Agent Plan. The service is WebSocket based and is deliberately
// kept separate from the OpenAI-compatible Ark endpoints.
package volcasr

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/providers/common"
)

const (
	defaultURL        = "wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_nostream"
	defaultResourceID = "volc.seedasr.sauc.duration"
	defaultModel      = "doubao-seed-asr-2.0"
	maxPCMChunk       = 6400 // 200ms: mono, 16 kHz, signed 16-bit PCM
)

type ASR struct {
	URL            string
	APIKey         string `json:"-"`
	ResourceID     string
	RequestModel   string
	ModelName      string
	UID            string
	TimeoutSeconds int
}

func (a *ASR) Name() string { return "volcengine_asr" }
func (a *ASR) Model() string {
	if a.ModelName == "" {
		return defaultModel
	}
	return a.ModelName
}

// GoString prevents %#v from exposing the API key. The URL is sanitised
// to strip any embedded credentials (userinfo, sensitive query params,
// fragment credentials). Case-insensitive param matching.
func (a ASR) GoString() string {
	safeURL := a.URL
	if safeURL != "" {
		if u, err := url.Parse(safeURL); err == nil {
			u.User = nil

			sensitiveParams := map[string]bool{
				"api_key": true, "apikey": true, "api_key_id": true,
				"key": true, "token": true, "secret": true, "password": true,
				"access_token": true, "credential": true, "authorization": true,
			}

			q := u.Query()
			stripped := false
			for param := range q {
				if sensitiveParams[strings.ToLower(param)] {
					q.Del(param)
					stripped = true
				}
			}
			if stripped {
				u.RawQuery = q.Encode()
			}

			// Strip sensitive parameters from fragment.
			if u.Fragment != "" {
				fragValues, fragErr := url.ParseQuery(u.Fragment)
				if fragErr == nil {
					fragStripped := false
					for param := range fragValues {
						if sensitiveParams[strings.ToLower(param)] {
							fragValues.Del(param)
							fragStripped = true
						}
					}
					if fragStripped {
						u.Fragment = fragValues.Encode()
					}
				}
			}

			safeURL = u.String()
		}
	}
	return fmt.Sprintf("volcasr.ASR{URL:%q APIKey:[redacted] ResourceID:%q RequestModel:%q ModelName:%q UID:%q TimeoutSeconds:%d}",
		safeURL, a.ResourceID, a.RequestModel, a.ModelName, a.UID, a.TimeoutSeconds)
}

// Format implements fmt.Formatter so that %v, %+v, and %s never expose the
// API key. Delegates to GoString for a safe representation.
func (a ASR) Format(f fmt.State, verb rune) {
	switch verb {
	case 'v', 's':
		f.Write([]byte(a.GoString()))
	default:
		fmt.Fprintf(f, "%%!%c(volcasr.ASR)", verb)
	}
}

func (a *ASR) Transcribe(ctx context.Context, req common.TranscribeRequest) (domain.Transcript, error) {
	if strings.TrimSpace(a.APIKey) == "" {
		return domain.Transcript{}, fmt.Errorf("Volcengine ASR API key is empty")
	}
	pcm, err := toPCM16(ctx, req.AudioPath)
	if err != nil {
		return domain.Transcript{}, err
	}
	if len(pcm) == 0 {
		return domain.Transcript{}, fmt.Errorf("Volcengine ASR input has no audio samples")
	}
	if a.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(a.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	url := a.URL
	if url == "" {
		url = defaultURL
	}
	headers := map[string][]string{
		"X-Api-Key":         {a.APIKey},
		"X-Api-Resource-Id": {a.resourceID()},
		"X-Api-Connect-Id":  {connectID()},
	}
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		return domain.Transcript{}, fmt.Errorf("connect Volcengine ASR: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	config := map[string]any{
		"user":    map[string]any{"uid": a.uid()},
		"audio":   map[string]any{"format": "pcm", "rate": 16000, "bits": 16, "channel": 1, "language": languageOrAuto(req.Language)},
		"request": map[string]any{"model_name": a.requestModel(), "enable_itn": true, "enable_punc": true, "enable_ddc": true},
	}
	if err := writeCompressedJSON(ctx, conn, config); err != nil {
		return domain.Transcript{}, err
	}
	for offset := 0; offset < len(pcm); offset += maxPCMChunk {
		end := offset + maxPCMChunk
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := writeCompressedAudio(ctx, conn, pcm[offset:end], end == len(pcm)); err != nil {
			return domain.Transcript{}, err
		}
	}

	var raw []string
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			// The server closes normally once it has sent its last sequence.
			// Audio that carries no speech legitimately produces no text, so
			// that closure arrives before any transcript frame. An empty
			// transcript is the correct answer there, not a failure — treating
			// it as one fails the job for every clip whose only sound is wind
			// or room tone, which the speech gate cannot tell from speech.
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return domain.Transcript{Language: req.Language, RawResponse: common.RedactString(strings.Join(raw, "\n"), a.APIKey)}, nil
			}
			return domain.Transcript{}, fmt.Errorf("read Volcengine ASR response: %w", err)
		}
		payload, responseType, err := decodeFrame(data)
		if err != nil {
			return domain.Transcript{}, fmt.Errorf("decode Volcengine ASR frame: %w", err)
		}
		if responseType == 0xF {
			payloadStr := strings.TrimSpace(string(payload))
			if len(payloadStr) > 2048 {
				payloadStr = payloadStr[:2048] + "…(truncated)"
			}
			return domain.Transcript{}, common.RedactError(fmt.Errorf("Volcengine ASR error: %s: %w", payloadStr, &common.StatusError{StatusCode: http.StatusBadGateway, Body: payloadStr}), a.APIKey)
		}
		if len(payload) == 0 {
			continue
		}
		raw = append(raw, string(payload))
		text, providerErr := transcriptText(payload)
		if providerErr != "" {
			return domain.Transcript{}, common.RedactError(fmt.Errorf("Volcengine ASR error: %s: %w", providerErr, &common.StatusError{StatusCode: http.StatusBadGateway, Body: providerErr}), a.APIKey)
		}
		if text != "" {
			return domain.Transcript{Language: req.Language, Text: text, Segments: []domain.TranscriptSegment{{Text: text}}, RawResponse: common.RedactString(strings.Join(raw, "\n"), a.APIKey)}, nil
		}
	}
}

func (a *ASR) resourceID() string {
	if a.ResourceID == "" {
		return defaultResourceID
	}
	return a.ResourceID
}
func (a *ASR) requestModel() string {
	if a.RequestModel == "" {
		return "bigmodel"
	}
	return a.RequestModel
}
func (a *ASR) uid() string {
	if a.UID == "" {
		return "nexusgate"
	}
	return a.UID
}
func languageOrAuto(language string) string {
	if strings.TrimSpace(language) == "" {
		return "auto"
	}
	return language
}

func toPCM16(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-i", path, "-ac", "1", "-ar", "16000", "-f", "s16le", "-")
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("convert audio to 16k PCM: %w", err)
	}
	return data, nil
}

func connectID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("nexusgate-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// Volcengine speech frames are [4 byte header][4 byte big-endian payload
// length][gzip payload]. Full JSON requests use message type 1; PCM chunks use
// type 2 and their last flag marks the end of an offline transcription.
func writeCompressedJSON(ctx context.Context, conn *websocket.Conn, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageBinary, buildFrame(0x1, 0x0, 0x1, gzipBytes(raw)))
}
func writeCompressedAudio(ctx context.Context, conn *websocket.Conn, pcm []byte, last bool) error {
	flags := byte(0x0)
	if last {
		flags = 0x2
	}
	return conn.Write(ctx, websocket.MessageBinary, buildFrame(0x2, flags, 0x0, gzipBytes(pcm)))
}
func buildFrame(messageType, flags, serialization byte, payload []byte) []byte {
	frame := make([]byte, 8+len(payload))
	frame[0] = 0x11 // protocol v1, four-byte header
	frame[1] = messageType<<4 | flags
	frame[2] = serialization<<4 | 0x1 // gzip compression
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	return frame
}
func gzipBytes(raw []byte) []byte {
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	_, _ = w.Write(raw)
	_ = w.Close()
	return out.Bytes()
}

func decodeFrame(frame []byte) ([]byte, byte, error) {
	if len(frame) < 8 {
		return nil, 0, fmt.Errorf("frame shorter than header")
	}
	messageType := frame[1] >> 4
	flags := frame[1] & 0x0F
	compression := frame[2] & 0x0F
	offset := 4
	// Server messages with a positive/negative sequence number insert it before
	// the payload length. The client does not depend on sequence values, but it
	// must skip them to read the result correctly.
	if flags == 0x1 || flags == 0x3 {
		if len(frame) < offset+8 {
			return nil, messageType, fmt.Errorf("frame missing sequence and payload length")
		}
		offset += 4
	}
	if len(frame) < offset+4 {
		return nil, messageType, fmt.Errorf("frame missing payload length")
	}
	size := int(binary.BigEndian.Uint32(frame[offset : offset+4]))
	offset += 4
	if len(frame) < offset+size {
		return nil, messageType, fmt.Errorf("invalid payload length %d", size)
	}
	payload := frame[offset : offset+size]
	if compression == 0x1 {
		reader, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, messageType, fmt.Errorf("open gzip payload: %w", err)
		}
		decompressed, err := io.ReadAll(reader)
		closeErr := reader.Close()
		if err != nil {
			return nil, messageType, err
		}
		if closeErr != nil {
			return nil, messageType, closeErr
		}
		payload = decompressed
	}
	return payload, messageType, nil
}

func transcriptText(payload []byte) (string, string) {
	var value any
	if json.Unmarshal(payload, &value) != nil {
		return "", ""
	}
	if m, ok := value.(map[string]any); ok {
		if code, exists := m["code"]; exists && !successCode(code) {
			return "", firstString(m, "message", "msg", "error")
		}
	}
	return firstTranscript(value), ""
}
func successCode(code any) bool {
	switch value := code.(type) {
	case float64:
		return value == 0 || value == 20000000
	case json.Number:
		return value.String() == "0" || value.String() == "20000000"
	default:
		return fmt.Sprint(value) == "0" || fmt.Sprint(value) == "20000000"
	}
}
func firstTranscript(value any) string {
	switch v := value.(type) {
	case map[string]any:
		for _, key := range []string{"text", "transcript", "utterance"} {
			if text, ok := v[key].(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
		for _, key := range []string{"result", "data", "payload"} {
			if text := firstTranscript(v[key]); text != "" {
				return text
			}
		}
		for _, child := range v {
			if text := firstTranscript(child); text != "" {
				return text
			}
		}
	case []any:
		for _, child := range v {
			if text := firstTranscript(child); text != "" {
				return text
			}
		}
	}
	return ""
}
func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "unknown provider error"
}
