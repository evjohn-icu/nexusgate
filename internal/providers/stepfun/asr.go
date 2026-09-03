package stepfun

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"

	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/providers/common"
)

type ASR struct {
	Endpoint  common.Endpoint
	ModelName string
	Path      string
}

func (a *ASR) Name() string { return "stepfun" }
func (a *ASR) Model() string {
	if a.ModelName == "" {
		return "stepaudio-2.5-asr"
	}
	return a.ModelName
}

func pcm16(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-i", path, "-ac", "1", "-ar", "16000", "-f", "s16le", "-")
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("convert audio to pcm16: %w", err)
	}
	return b, nil
}
func (a *ASR) Transcribe(ctx context.Context, req common.TranscribeRequest) (domain.Transcript, error) {
	pcm, err := pcm16(ctx, req.AudioPath)
	if err != nil {
		return domain.Transcript{}, err
	}
	body := map[string]any{"audio": map[string]any{"data": base64.StdEncoding.EncodeToString(pcm), "input": map[string]any{"transcription": map[string]any{"model": a.Model(), "language": req.Language, "enable_itn": true, "enable_timestamp": true}, "format": map[string]any{"type": "pcm", "codec": "pcm_s16le", "rate": 16000, "bits": 16, "channel": 1}}}}
	path := a.Path
	if path == "" {
		path = "audio/asr/sse"
	}
	httpReq, err := a.Endpoint.NewRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return domain.Transcript{}, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := a.Endpoint.Client().Do(httpReq)
	if err != nil {
		return domain.Transcript{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return domain.Transcript{}, common.ReadErrorWithSecret(resp, a.Endpoint.APIKey)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var rawLines []string
	var deltas strings.Builder
	var doneText string
	var segments []domain.TranscriptSegment
	var legacyTexts []string
	for scanner.Scan() {
		line := scanner.Text()
		rawLines = append(rawLines, line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type      string `json:"type"`
			Delta     string `json:"delta"`
			Text      string `json:"text"`
			Message   string `json:"message"`
			StartTime int64  `json:"start_time"`
			EndTime   int64  `json:"end_time"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		switch event.Type {
		case "transcript.text.delta":
			deltas.WriteString(event.Delta)
			if event.Delta != "" {
				segments = append(segments, domain.TranscriptSegment{StartMS: event.StartTime, EndMS: event.EndTime, Text: event.Delta})
			}
		case "transcript.text.done":
			doneText = event.Text
		case "error":
			if event.Message == "" {
				event.Message = "unknown provider error"
			}
			return domain.Transcript{}, common.Errorf(a.Endpoint.APIKey, "stepfun SSE error: %s", event.Message)
		default:
			var value any
			if json.Unmarshal([]byte(payload), &value) == nil {
				collectText(value, &legacyTexts)
			}
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return domain.Transcript{}, err
	}
	text := strings.TrimSpace(doneText)
	if text == "" {
		text = strings.TrimSpace(deltas.String())
	}
	if text == "" {
		text = strings.TrimSpace(strings.Join(unique(legacyTexts), ""))
	}
	if text == "" {
		return domain.Transcript{}, fmt.Errorf("stepfun SSE contained no transcript text")
	}
	if len(segments) == 0 {
		segments = []domain.TranscriptSegment{{Text: text}}
	}
	return domain.Transcript{Language: req.Language, Text: text, Segments: segments, RawResponse: common.RedactString(strings.Join(rawLines, "\n"), a.Endpoint.APIKey)}, nil
}
func collectText(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "text" || k == "transcript" || k == "content" {
				if s, ok := val.(string); ok && s != "" {
					*out = append(*out, s)
				}
			}
			collectText(val, out)
		}
	case []any:
		for _, v := range x {
			collectText(v, out)
		}
	}
}
func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
