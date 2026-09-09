// Package openaivideo adapts OpenAI-compatible video-chat endpoints to the
// provider-independent NexusGate video analysis contract. It is used by Qwen,
// Ark/Volcengine endpoints configured for video input, and local VLM gateways.
package openaivideo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	videoanalysis "github.com/evjohn-icu/nexusgate/internal/domain/video_analysis"
	"github.com/evjohn-icu/nexusgate/internal/normalize"
	"github.com/evjohn-icu/nexusgate/internal/providers/common"
	videoproviders "github.com/evjohn-icu/nexusgate/internal/providers/video"
)

type Provider struct {
	ProviderName string
	Endpoint     common.Endpoint
	ModelName    string
	Path         string
	// MaxInlineBytes is the largest request body this endpoint accepts —
	// the number an operator actually has (a 413 names the request that
	// tripped it, never the file inside it). Zero selects
	// defaultMaxInlineBytes. MaxInlineVideoBytes converts this wire ceiling
	// into the pre-encoding file budget the window splitter compares
	// against; callers of MaxInlineVideoBytes never see this field's raw
	// value.
	MaxInlineBytes int64
}

// defaultMaxInlineBytes is deliberately below every endpoint this adapter is
// pointed at rather than tuned to the most generous one, and is itself a
// request-body ceiling (see MaxInlineBytes) — MaxInlineVideoBytes is what
// turns it into a safe file-byte budget. Overshooting costs a whole upload
// before the rejection arrives, which is the expensive way to fail when an
// endpoint does not declare its own limit.
const defaultMaxInlineBytes = 24 << 20

// inlineOverheadReserveBytes reserves headroom, ahead of the base64 expansion
// below, for what rides in the same request alongside the encoded video: the
// JSON envelope, the fixed schema/vocabulary prompt (order 1-2KB, measured),
// and a window's sliced transcript text. It does not attempt to bound the
// variable overshoot a stream-copy extraction can add by snapping to a
// preceding keyframe, or bitrate variance inside one proxy — those are why a
// window that still gets a 413 is bisected and retried
// (internal/app/analysis_segments.go) rather than trusted to fit on the
// first try.
const inlineOverheadReserveBytes = 1 << 20

var _ videoproviders.VideoUnderstandingProvider = (*Provider)(nil)
var _ videoproviders.InlineVideoLimiter = (*Provider)(nil)

// MaxInlineVideoBytes reports the raw (pre-base64) file-byte budget the
// window splitter should target so the encoded request stays under this
// endpoint's request-body ceiling (MaxInlineBytes, or the conservative
// default).
//
// Base64 (encoding/base64.StdEncoding, used by inputVideoURL below) turns
// every 3 source bytes into 4 encoded characters — a file sized to exactly
// the declared ceiling arrives on the wire a third larger than that ceiling
// and the endpoint answers 413 even though the splitter believed the window
// would fit. Multiplying by 3/4 undoes that expansion in advance; subtracting
// inlineOverheadReserveBytes first leaves room for what else shares the
// request body with the video.
func (p *Provider) MaxInlineVideoBytes() int64 {
	ceiling := p.MaxInlineBytes
	if ceiling <= 0 {
		ceiling = defaultMaxInlineBytes
	}
	usable := ceiling - inlineOverheadReserveBytes
	if usable < 0 {
		usable = 0
	}
	budget := (usable / 4) * 3
	if budget <= 0 {
		// A configured ceiling at or below inlineOverheadReserveBytes truly
		// has no room, and must say so rather than fabricate a positive
		// number: InlineVideoBudget (video/interface.go) treats <= 0 as
		// "the provider has no opinion" and substitutes its own, larger
		// fallback — so a plain zero here would make an operator's explicit,
		// unrealistically tight limit produce a *bigger* effective budget
		// than an unconfigured one, exactly backwards. NoInlineRoom is the
		// distinct signal that lets the caller refuse the asset instead.
		return videoproviders.NoInlineRoom
	}
	return budget
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
		return videoanalysis.Result{}, "", common.ReadErrorWithSecret(resp, p.Endpoint.APIKey)
	}
	raw, err := common.ReadBody(resp.Body)
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
		return videoanalysis.Result{}, common.RedactString(string(raw), p.Endpoint.APIKey), common.Errorf(p.Endpoint.APIKey, "decode OpenAI-compatible video response: missing choices: %s", string(raw))
	}
	result, err := decodeUnified(response.Choices[0].Message.Content)
	if err != nil {
		return videoanalysis.Result{}, common.RedactString(string(raw), p.Endpoint.APIKey), common.RedactError(err, p.Endpoint.APIKey)
	}
	return result, common.RedactString(string(raw), p.Endpoint.APIKey), nil
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
	return `Analyze this video and return JSON only. Required schema: {"summary":string,"scenes":[{"description":string,"start_ms":number,"end_ms":number,"tags":[string]}],"objects":[{"name":string,"count":number,"confidence":number}],"actions":[{"name":string,"confidence":number}],"mood":[string],"raw_tags":[string],"shots":[{"start_ms":number,"end_ms":number,"description":string,"tags":[string],"objects":[string],"actions":[string],"mood":[string],"confidence":number}],"confidence":number,"analysis":{"asset_type":string,"scene_tags":[string],"subjects":[string],"people_count":number,"shot_size":string,"camera_motion":string,"lighting":string,"audio_type":string,"has_speech":boolean,"summary":string,"usable_as":[string],"mood_tags":[string],"quality":string,"quality_flags":[string],"extra_tags":[string],"editorial_reason":string}}. Describe only observable footage. ` + normalize.VocabularyPrompt() + normalize.OutputLanguagePrompt(input.Language) + `Metadata: ` + string(metadata) + ` Transcript: ` + transcript
}
