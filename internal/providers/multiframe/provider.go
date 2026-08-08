// Package multiframe adapts OpenAI-compatible multimodal chat endpoints to
// Timingdex's multiframe analysis contract. The endpoint never sees a video:
// Timingdex extracts deterministic frames from a shot, tags each with its
// timestamp, slices the transcript to the shot's window, and the model simply
// describes the pixels it is shown — it is never asked to guess a timeline.
//
// The same OpenAI chat/completions surface is served by llama.cpp, LM Studio,
// vLLM, SGLang and most other local multimodal runtimes, so the protocol
// binds no specific model.
package multiframe

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

	"github.com/evjohn-icu/timingdex/internal/domain"
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
}

var _ videoproviders.VideoUnderstandingProvider = (*Provider)(nil)
var _ videoproviders.MultiframeShotAnalyzer = (*Provider)(nil)

func (p *Provider) Name() string {
	if p.ProviderName == "" {
		return "local_multiframe"
	}
	return p.ProviderName
}

func (p *Provider) Model() string {
	if p.ModelName == "" {
		return "multiframe-vision-model"
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

// Analyze is the asset-level summary call: a handful of representative frames
// plus the transcript, answered as the legacy analysis object. It carries no
// per-shot timeline — the shot boundaries were decided by Timingdex's
// detector before this call was ever made.
func (p *Provider) Analyze(ctx context.Context, input videoanalysis.Input) (videoanalysis.Result, string, error) {
	if len(input.Frames) == 0 {
		return videoanalysis.Result{}, "", fmt.Errorf("multiframe summary call requires at least one frame")
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		prompt = summaryPrompt(input)
	}
	content := make([]any, 0, len(input.Frames)+1)
	content = append(content, map[string]any{"type": "text", "text": prompt})
	for _, frame := range input.Frames {
		url, err := frameDataURL(frame.Path)
		if err != nil {
			return videoanalysis.Result{}, "", err
		}
		content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
	}
	contentText, raw, err := p.chat(ctx, content)
	if err != nil {
		return videoanalysis.Result{}, raw, err
	}
	var result videoanalysis.Result
	if err := json.Unmarshal([]byte(strings.TrimSpace(stripFences(contentText))), &result); err != nil {
		return videoanalysis.Result{}, raw, domain.Permanent(fmt.Errorf("decode multiframe summary: %w", err))
	}
	if strings.TrimSpace(result.Summary) == "" && result.Analysis.Summary == "" {
		return videoanalysis.Result{}, raw, domain.Permanent(fmt.Errorf("decode multiframe summary: response has no summary"))
	}
	return result, raw, nil
}

// AnalyzeShot is the per-shot call. The request already knows the shot's
// window and transcript slice; the model returns pure shot metadata, and any
// timeline field it invents is discarded at decode time.
func (p *Provider) AnalyzeShot(ctx context.Context, req videoproviders.ShotAnalysisRequest) (videoproviders.ShotMetadata, string, error) {
	if len(req.Frames) == 0 {
		return videoproviders.ShotMetadata{}, "", fmt.Errorf("multiframe shot call requires at least one frame")
	}
	content := make([]any, 0, len(req.Frames)+1)
	content = append(content, map[string]any{"type": "text", "text": shotPrompt(req)})
	for _, frame := range req.Frames {
		url, err := frameDataURL(frame.Path)
		if err != nil {
			return videoproviders.ShotMetadata{}, "", err
		}
		content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
	}
	contentText, raw, err := p.chat(ctx, content)
	if err != nil {
		return videoproviders.ShotMetadata{}, raw, err
	}
	body := strings.TrimSpace(stripFences(contentText))
	// Decode into a wire shape first so fields the model was told not to send
	// (start_ms, end_ms) cannot smuggle in through unknown-field leniency.
	var wire struct {
		Description  string   `json:"description"`
		Objects      []string `json:"objects"`
		Actions      []string `json:"actions"`
		Mood         []string `json:"mood"`
		Tags         []string `json:"tags"`
		ShotSize     string   `json:"shot_size"`
		CameraMotion string   `json:"camera_motion"`
		Quality      string   `json:"quality"`
		UsableAs     []string `json:"usable_as"`
		Confidence   float64  `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return videoproviders.ShotMetadata{}, raw, domain.Permanent(fmt.Errorf("decode multiframe shot: %w", err))
	}
	if strings.TrimSpace(wire.Description) == "" {
		return videoproviders.ShotMetadata{}, raw, domain.Permanent(fmt.Errorf("decode multiframe shot: description is required"))
	}
	return videoproviders.ShotMetadata{
		Description:  strings.TrimSpace(wire.Description),
		Objects:      cleanStrings(wire.Objects),
		Actions:      cleanStrings(wire.Actions),
		Mood:         cleanStrings(wire.Mood),
		Tags:         cleanStrings(wire.Tags),
		ShotSize:     normOptionalEnum(wire.ShotSize, normalize.ShotSizeValues),
		CameraMotion: normOptionalEnum(wire.CameraMotion, normalize.MotionValues),
		Quality:      normOptionalEnum(wire.Quality, normalize.QualityValues),
		UsableAs:     cleanStrings(wire.UsableAs),
		Confidence:   wire.Confidence,
	}, raw, nil
}

// normOptionalEnum normalizes an enum the model sent, and leaves a field it
// did not send empty. NormalizeEnumValue maps an unrecognized value to the
// vocabulary's unknown marker, and an absent answer must stay absent: the
// fold that aggregates per-shot enums counts non-empty answers only, so an
// unobserved field must not vote as "unknown".
func normOptionalEnum(value string, allowed []string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return normalize.NormalizeEnumValue(value, allowed)
}

// chat returns the model's content and the full raw response body (recorded
// as the model run's evidence), or an error classified by the endpoint.
func (p *Provider) chat(ctx context.Context, content []any) (string, string, error) {
	body := map[string]any{
		"model":           p.Model(),
		"response_format": map[string]any{"type": "json_object"},
		"temperature":     0.1,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": content,
		}},
	}
	path := p.Path
	if path == "" {
		path = "chat/completions"
	}
	req, err := p.Endpoint.NewRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return "", "", err
	}
	resp, err := p.Endpoint.Client().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", "", common.ReadError(resp)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || len(response.Choices) == 0 || strings.TrimSpace(response.Choices[0].Message.Content) == "" {
		return "", string(raw), fmt.Errorf("decode OpenAI-compatible multiframe response: missing choices")
	}
	return response.Choices[0].Message.Content, string(raw), nil
}

// summaryPrompt asks for the asset-level analysis object. The model sees a
// few frames spread across the timeline and the whole aligned transcript, so
// it can summarize what the asset is about without ever being asked for
// boundaries.
func summaryPrompt(input videoanalysis.Input) string {
	tr := ""
	if input.Transcript != nil {
		tr = input.Transcript.Text
	}
	meta, _ := json.Marshal(input.Metadata)
	return `Analyze this video from its sampled frames and return one JSON object only. Required schema: {"summary":string,"analysis":{"asset_type":string,"scene_tags":[string],"subjects":[string],"people_count":number,"shot_size":string,"camera_motion":string,"lighting":string,"audio_type":string,"has_speech":boolean,"summary":string,"usable_as":[string],"mood_tags":[string],"quality":string,"quality_flags":[string],"extra_tags":[string],"editorial_reason":string}}. You cannot hear the audio: derive audio_type from the transcript below only (speech present, otherwise unknown), and answer with the transcript's language and content in mind. Describe only observable footage. ` + normalize.VocabularyPrompt() + `Metadata: ` + string(meta) + ` Transcript: ` + tr
}

// shotPrompt asks for pure shot metadata. The shot's window and each frame's
// timestamp are stated so the model reasons about the shot's own evidence;
// the schema deliberately contains no timeline fields.
func shotPrompt(req videoproviders.ShotAnalysisRequest) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Analyze this single shot of a video. The shot spans %dms to %dms on the asset timeline. Frames were sampled from this shot at: ", req.ShotStartMS, req.ShotEndMS))
	for i, frame := range req.Frames {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(fmt.Sprintf("%dms", frame.TimestampMS))
	}
	b.WriteString(".\n")
	if req.Transcript != nil && strings.TrimSpace(req.Transcript.Text) != "" {
		b.WriteString("Transcript within this shot:\n")
		for _, segment := range req.Transcript.Segments {
			b.WriteString(fmt.Sprintf("[%dms-%dms] %s\n", segment.StartMS, segment.EndMS, segment.Text))
		}
		if len(req.Transcript.Segments) == 0 {
			b.WriteString(req.Transcript.Text)
			b.WriteString("\n")
		}
	}
	b.WriteString(`Describe only what these frames show. Return one JSON object only with exactly this schema: {"description":string,"objects":[string],"actions":[string],"mood":[string],"tags":[string],"shot_size":string,"camera_motion":string,"quality":string,"usable_as":[string],"confidence":number}. `)
	b.WriteString(normalize.VocabularyPrompt())
	b.WriteString(`Do not include any time fields: the shot boundaries are already known.`)
	return b.String()
}

// frameDataURL encodes a local frame file as a data URL, the only form every
// OpenAI-compatible runtime accepts for local images. The frames come from
// Timingdex's own proxy and are already at VLM-friendly resolution.
func frameDataURL(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mimeType == "" {
		mimeType = "image/jpeg"
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func cleanStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// stripFences removes a ```json fence a model sometimes wraps its answer in
// despite the response_format, mirroring the other adapters' tolerance.
func stripFences(content string) string {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	return strings.TrimSpace(content)
}
