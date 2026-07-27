package qwen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ev/timingdex/internal/domain"
	"github.com/ev/timingdex/internal/providers/common"
)

type ASR struct {
	Endpoint  common.Endpoint
	ModelName string
	Path      string
}

func (a *ASR) Name() string { return "qwen" }
func (a *ASR) Model() string {
	if a.ModelName == "" {
		return "qwen3-asr-flash"
	}
	return a.ModelName
}

func (a *ASR) Transcribe(ctx context.Context, req common.TranscribeRequest) (domain.Transcript, error) {
	data, err := os.ReadFile(req.AudioPath)
	if err != nil {
		return domain.Transcript{}, err
	}
	if len(data) > 10*1024*1024 {
		return domain.Transcript{}, fmt.Errorf("qwen OpenAI-compatible inline audio limit exceeded: %d bytes", len(data))
	}
	ext := strings.ToLower(filepath.Ext(req.AudioPath))
	// Use the conventional media type expected by Qwen's inline-audio API.
	// On Ubuntu, mime.TypeByExtension(".wav") returns audio/vnd.wave, which is
	// equivalent but not accepted consistently by OpenAI-compatible gateways.
	mt := ""
	if ext == ".wav" || ext == ".wave" {
		mt = "audio/wav"
	} else {
		mt = mime.TypeByExtension(ext)
	}
	if mt == "" {
		mt = "audio/mp4"
	}
	dataURI := "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data)
	body := map[string]any{
		"model": a.Model(), "stream": false,
		"messages":    []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": dataURI}}}}},
		"asr_options": map[string]any{"language": req.Language, "enable_itn": true},
	}
	path := a.Path
	if path == "" {
		path = "chat/completions"
	}
	httpReq, err := a.Endpoint.NewRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return domain.Transcript{}, err
	}
	resp, err := a.Endpoint.Client().Do(httpReq)
	if err != nil {
		return domain.Transcript{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return domain.Transcript{}, common.ReadError(resp)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return domain.Transcript{}, err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return domain.Transcript{}, fmt.Errorf("decode qwen response: %w", err)
	}
	if len(out.Choices) == 0 {
		return domain.Transcript{}, fmt.Errorf("qwen response has no choices")
	}
	text := strings.TrimSpace(out.Choices[0].Message.Content)
	return domain.Transcript{Language: req.Language, Text: text, Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 0, Text: text}}, RawResponse: string(raw)}, nil
}
