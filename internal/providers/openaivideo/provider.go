// Package openaivideo adapts OpenAI-compatible video-chat endpoints to the
// provider-independent Timingdex video analysis contract. It is used by Qwen,
// Ark/Volcengine endpoints configured for video input, and local VLM gateways.
package openaivideo

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

	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
	"github.com/evjohn-icu/timingdex/internal/normalize"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
	videoproviders "github.com/evjohn-icu/timingdex/internal/providers/video"
)

type Provider struct {
	ProviderName string
	Endpoint     common.Endpoint
	ModelName    string
	Path         string
	// MaxInlineBytes is the largest video this endpoint accepts in a request
	// body, before base64 expansion. Zero selects defaultMaxInlineBytes.
	MaxInlineBytes int64
}

// defaultMaxInlineBytes is deliberately below every endpoint this adapter is
// pointed at rather than tuned to the most generous one. Overshooting costs a
// whole upload before the rejection arrives, and the request body is a third
// larger than the file once base64-encoded — so the conservative default is
// the one that fails least expensively when an endpoint does not declare its
// own limit.
const defaultMaxInlineBytes = 24 << 20

var _ videoproviders.VideoUnderstandingProvider = (*Provider)(nil)
var _ videoproviders.InlineVideoLimiter = (*Provider)(nil)

// MaxInlineVideoBytes reports the configured inline ceiling for this endpoint.
func (p *Provider) MaxInlineVideoBytes() int64 {
	if p.MaxInlineBytes > 0 {
		return p.MaxInlineBytes
	}
	return defaultMaxInlineBytes
}

func (p *Provider) Name() string {
	if p.ProviderName == "" {
		return "openai_video"
	}
	return p.ProviderName
}

func (p *Provider) Model() string {
	if p.ModelName == "" {
		return "video-understanding-model"
	}
	return p.ModelName
}

func (p *Provider) Capabilities() []videoproviders.Capability {
	return []videoproviders.Capability{
		videoproviders.CapabilityVideoAnalysis,
		videoproviders.CapabilitySceneAnalysis,
		videoproviders.CapabilityShotAnalysis,
	}
}

func (p *Provider) Analyze(ctx context.Context, input videoanalysis.Input) (videoanalysis.Result, string, error) {
	videoURL, err := inputVideoURL(input)
	if err != nil {
		return videoanalysis.Result{}, "", err
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		prompt = unifiedPrompt(input)
	}
	body := map[string]any{
		"model":           p.Model(),
		"response_format": map[string]any{"type": "json_object"},
		"temperature":     0.1,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": prompt},
				map[string]any{"type": "video_url", "video_url": map[string]any{"url": videoURL}},
			},
		}},
	}
	path := p.Path
	if path == "" {
		path = "chat/completions"
	}
	req, err := p.Endpoint.NewRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return videoanalysis.Result{}, "", err
	}
	resp, err := p.Endpoint.Client().Do(req)
	if err != nil {
		return videoanalysis.Result{}, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return videoanalysis.Result{}, "", common.ReadError(resp)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return videoanalysis.Result{}, "", err
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || len(response.Choices) == 0 {
		return videoanalysis.Result{}, string(raw), fmt.Errorf("decode OpenAI-compatible video response: missing choices")
	}
	result, err := decodeUnified(response.Choices[0].Message.Content)
	if err != nil {
		return videoanalysis.Result{}, string(raw), err
	}
	return result, string(raw), nil
}

func inputVideoURL(input videoanalysis.Input) (string, error) {
	if strings.TrimSpace(input.RemoteURI) != "" {
		return input.RemoteURI, nil
	}
	if strings.TrimSpace(input.VideoPath) == "" {
		return "", fmt.Errorf("video path or remote URI is required")
	}
	data, err := os.ReadFile(input.VideoPath)
	if err != nil {
		return "", err
	}
	mimeType := input.MIMEType
	if mimeType == "" {
		mimeType = mime.TypeByExtension(strings.ToLower(filepath.Ext(input.VideoPath)))
	}
	if mimeType == "" {
		mimeType = "video/mp4"
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func decodeUnified(content string) (videoanalysis.Result, error) {
	content = strings.TrimSpace(content)
	content = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(content, "```json"), "```"))
	var result videoanalysis.Result
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return videoanalysis.Result{}, fmt.Errorf("decode unified video analysis: %w", err)
	}
	if strings.TrimSpace(result.Summary) == "" && len(result.Scenes) == 0 && len(result.Shots) == 0 && len(result.RawTags) == 0 {
		return videoanalysis.Result{}, fmt.Errorf("decode unified video analysis: response has no semantic fields")
	}
	return result, nil
}

func unifiedPrompt(input videoanalysis.Input) string {
	transcript := ""
	if input.Transcript != nil {
		transcript = input.Transcript.Text
	}
	metadata, _ := json.Marshal(input.Metadata)
	return `Analyze this video and return JSON only. Required schema: {"summary":string,"scenes":[{"description":string,"start_ms":number,"end_ms":number,"tags":[string]}],"objects":[{"name":string,"count":number,"confidence":number}],"actions":[{"name":string,"confidence":number}],"mood":[string],"raw_tags":[string],"shots":[{"start_ms":number,"end_ms":number,"description":string,"tags":[string],"objects":[string],"actions":[string],"mood":[string],"confidence":number}],"confidence":number,"analysis":{"asset_type":string,"scene_tags":[string],"subjects":[string],"people_count":number,"shot_size":string,"camera_motion":string,"lighting":string,"audio_type":string,"has_speech":boolean,"summary":string,"usable_as":[string],"mood_tags":[string],"quality":string,"quality_flags":[string],"extra_tags":[string],"editorial_reason":string}}. Describe only observable footage. ` + normalize.VocabularyPrompt() + `Metadata: ` + string(metadata) + ` Transcript: ` + transcript
}
