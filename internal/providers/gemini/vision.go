package gemini

import (
	"bytes"
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
	"time"

	"github.com/ev/timingdex/internal/domain"
	videoanalysis "github.com/ev/timingdex/internal/domain/video_analysis"
	"github.com/ev/timingdex/internal/normalize"
	"github.com/ev/timingdex/internal/providers/common"
	videoproviders "github.com/ev/timingdex/internal/providers/video"
)

type Vision struct {
	Endpoint  common.Endpoint
	ModelName string
	Path      string
	Protocol  string
}

var _ videoproviders.VideoPreparer = (*Vision)(nil)

// VideoProvider is the v0.7 adapter. Vision remains available for callers of
// the v0.5 compatibility interface while this adapter exposes the unified
// scene/shot result contract.
type VideoProvider struct {
	*Vision
}

var _ videoproviders.VideoUnderstandingProvider = (*VideoProvider)(nil)

func (p *VideoProvider) Analyze(ctx context.Context, req videoanalysis.Input) (videoanalysis.Result, string, error) {
	return p.Vision.AnalyzeVideo(ctx, req)
}

func (v *Vision) Name() string { return "gemini" }
func (v *Vision) Model() string {
	if v.ModelName == "" {
		return "gemini-2.5-flash"
	}
	return v.ModelName
}

func (v *Vision) Capabilities() []videoproviders.Capability {
	return []videoproviders.Capability{
		videoproviders.CapabilityVideoAnalysis,
		videoproviders.CapabilitySceneAnalysis,
		videoproviders.CapabilityShotAnalysis,
	}
}

func (v *Vision) PrepareVideo(ctx context.Context, req common.PrepareVideoRequest) (common.PreparedVideo, error) {
	f, err := os.Open(req.VideoPath)
	if err != nil {
		return common.PreparedVideo{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return common.PreparedVideo{}, err
	}
	mt := req.MIMEType
	if mt == "" {
		mt = mediaType(req.VideoPath)
	}
	meta := map[string]any{"file": map[string]any{"display_name": req.DisplayName}}
	body, _ := json.Marshal(meta)
	startURL := strings.TrimRight(v.Endpoint.BaseURL, "/")
	if strings.HasSuffix(startURL, "/v1beta") {
		startURL = strings.TrimSuffix(startURL, "/v1beta")
	}
	startURL += "/upload/v1beta/files"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, startURL, bytes.NewReader(body))
	if err != nil {
		return common.PreparedVideo{}, err
	}
	v.applyAuth(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Goog-Upload-Protocol", "resumable")
	httpReq.Header.Set("X-Goog-Upload-Command", "start")
	httpReq.Header.Set("X-Goog-Upload-Header-Content-Length", fmt.Sprint(info.Size()))
	httpReq.Header.Set("X-Goog-Upload-Header-Content-Type", mt)
	resp, err := v.Endpoint.Client().Do(httpReq)
	if err != nil {
		return common.PreparedVideo{}, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return common.PreparedVideo{}, common.ReadError(resp)
	}
	uploadURL := resp.Header.Get("X-Goog-Upload-URL")
	resp.Body.Close()
	if uploadURL == "" {
		return common.PreparedVideo{}, fmt.Errorf("Gemini upload did not return X-Goog-Upload-URL")
	}

	uploadReq, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, f)
	if err != nil {
		return common.PreparedVideo{}, err
	}
	v.applyAuth(uploadReq)
	uploadReq.Header.Set("Content-Length", fmt.Sprint(info.Size()))
	uploadReq.Header.Set("X-Goog-Upload-Offset", "0")
	uploadReq.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	uploadResp, err := v.Endpoint.Client().Do(uploadReq)
	if err != nil {
		return common.PreparedVideo{}, err
	}
	defer uploadResp.Body.Close()
	if uploadResp.StatusCode/100 != 2 {
		return common.PreparedVideo{}, common.ReadError(uploadResp)
	}
	raw, err := io.ReadAll(uploadResp.Body)
	if err != nil {
		return common.PreparedVideo{}, err
	}
	var out struct {
		File struct {
			Name     string `json:"name"`
			URI      string `json:"uri"`
			MIMEType string `json:"mimeType"`
			State    string `json:"state"`
		} `json:"file"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return common.PreparedVideo{}, err
	}
	if out.File.Name == "" || out.File.URI == "" {
		return common.PreparedVideo{}, fmt.Errorf("invalid Gemini file response: %s", string(raw))
	}
	prepared := common.PreparedVideo{RemoteName: out.File.Name, RemoteURI: out.File.URI, MIMEType: mt, State: out.File.State, SizeBytes: info.Size()}
	for prepared.State != "ACTIVE" {
		if prepared.State == "FAILED" {
			return prepared, fmt.Errorf("Gemini file processing failed")
		}
		select {
		case <-ctx.Done():
			return prepared, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		state, err := v.fileState(ctx, prepared.RemoteName)
		if err != nil {
			return prepared, err
		}
		prepared.State = state
	}
	return prepared, nil
}

func (v *Vision) fileState(ctx context.Context, name string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.Endpoint.URL(name), nil)
	if err != nil {
		return "", err
	}
	v.applyAuth(req)
	resp, err := v.Endpoint.Client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", common.ReadError(resp)
	}
	var x struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&x); err != nil {
		return "", err
	}
	return x.State, nil
}

func (v *Vision) Analyze(ctx context.Context, req common.AnalyzeRequest) (domain.StructuredAnalysis, string, error) {
	result, raw, err := v.AnalyzeVideo(ctx, videoanalysis.Input{
		VideoPath:  req.VideoPath,
		RemoteURI:  req.RemoteURI,
		MIMEType:   req.MIMEType,
		Transcript: req.Transcript,
		Metadata:   req.Metadata,
		Prompt:     req.Prompt,
	})
	return result.ToStructuredAnalysis(), raw, err
}

func (v *Vision) AnalyzeVideo(ctx context.Context, req videoanalysis.Input) (videoanalysis.Result, string, error) {
	prompt := req.Prompt
	if prompt == "" {
		prompt = defaultPrompt(req)
	}
	protocol := v.Protocol
	if protocol == "" {
		protocol = "gemini_generate_content"
	}
	if protocol == "openai_chat" {
		return v.analyzeOpenAI(ctx, req, prompt)
	}
	return v.analyzeNative(ctx, req, prompt)
}

func (v *Vision) analyzeNative(ctx context.Context, req videoanalysis.Input, prompt string) (videoanalysis.Result, string, error) {
	parts := []any{map[string]any{"text": prompt}}
	if req.RemoteURI != "" {
		parts = append(parts, map[string]any{"file_data": map[string]any{"mime_type": req.MIMEType, "file_uri": req.RemoteURI}})
	} else {
		b, err := os.ReadFile(req.VideoPath)
		if err != nil {
			return videoanalysis.Result{}, "", err
		}
		parts = append(parts, map[string]any{"inline_data": map[string]any{"mime_type": mediaType(req.VideoPath), "data": base64.StdEncoding.EncodeToString(b)}})
	}
	body := map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": parts}},
		"generationConfig": map[string]any{"responseMimeType": "application/json", "temperature": 0.1},
	}
	path := v.Path
	if path == "" {
		path = "models/" + v.Model() + ":generateContent"
	}
	httpReq, err := v.Endpoint.NewRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return videoanalysis.Result{}, "", err
	}
	resp, err := v.Endpoint.Client().Do(httpReq)
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
	var x struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &x); err != nil || len(x.Candidates) == 0 {
		return videoanalysis.Result{}, string(raw), fmt.Errorf("invalid Gemini response")
	}
	var text string
	for _, p := range x.Candidates[0].Content.Parts {
		text += p.Text
	}
	return decodeAnalysis(text, raw)
}

func (v *Vision) analyzeOpenAI(ctx context.Context, req videoanalysis.Input, prompt string) (videoanalysis.Result, string, error) {
	if req.RemoteURI != "" {
		return videoanalysis.Result{}, "", fmt.Errorf("OpenAI-compatible video mode requires inline video_url; remote Gemini URI is unsupported")
	}
	b, err := os.ReadFile(req.VideoPath)
	if err != nil {
		return videoanalysis.Result{}, "", err
	}
	body := map[string]any{"model": v.Model(), "response_format": map[string]any{"type": "json_object"}, "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": prompt}, map[string]any{"type": "video_url", "video_url": map[string]any{"url": "data:" + mediaType(req.VideoPath) + ";base64," + base64.StdEncoding.EncodeToString(b)}}}}}}
	path := v.Path
	if path == "" {
		path = "chat/completions"
	}
	httpReq, err := v.Endpoint.NewRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return videoanalysis.Result{}, "", err
	}
	resp, err := v.Endpoint.Client().Do(httpReq)
	if err != nil {
		return videoanalysis.Result{}, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return videoanalysis.Result{}, "", common.ReadError(resp)
	}
	raw, _ := io.ReadAll(resp.Body)
	var x struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &x) != nil || len(x.Choices) == 0 {
		return videoanalysis.Result{}, string(raw), fmt.Errorf("invalid OpenAI-compatible response")
	}
	return decodeAnalysis(x.Choices[0].Message.Content, raw)
}

func decodeAnalysis(text string, raw []byte) (videoanalysis.Result, string, error) {
	text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(text), "```json"), "```"))
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		return videoanalysis.Result{}, string(raw), fmt.Errorf("decode structured video analysis: %w", err)
	}
	for _, key := range []string{"asset_type", "scene_tags", "subjects", "people_count", "shot_size", "camera_motion", "lighting", "audio_type", "has_speech", "usable_as", "mood_tags", "quality", "quality_flags", "extra_tags", "editorial_reason"} {
		if _, ok := fields[key]; ok {
			var legacy domain.StructuredAnalysis
			if err := json.Unmarshal([]byte(text), &legacy); err != nil {
				return videoanalysis.Result{}, string(raw), fmt.Errorf("decode legacy structured analysis: %w", err)
			}
			return videoanalysis.Result{Summary: legacy.Summary, Analysis: legacy}, string(raw), nil
		}
	}
	var out videoanalysis.Result
	if err := json.Unmarshal([]byte(text), &out); err == nil && (out.Summary != "" || len(out.Scenes) > 0 || len(out.RawTags) > 0) {
		return out, string(raw), nil
	}
	return out, string(raw), fmt.Errorf("decode structured video analysis: response has no unified fields")
}

func (v *Vision) applyAuth(req *http.Request) {
	header := v.Endpoint.AuthHeader
	if header == "" {
		header = "x-goog-api-key"
	}
	if v.Endpoint.APIKey != "" {
		value := v.Endpoint.APIKey
		if v.Endpoint.AuthScheme != "" && v.Endpoint.AuthScheme != "raw" {
			value = v.Endpoint.AuthScheme + " " + value
		}
		req.Header.Set(header, value)
	}
	for k, val := range v.Endpoint.ExtraHeaders {
		req.Header.Set(k, val)
	}
}

func mediaType(path string) string {
	mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mt == "" {
		return "video/mp4"
	}
	return mt
}

func defaultPrompt(req videoanalysis.Input) string {
	tr := ""
	if req.Transcript != nil {
		tr = req.Transcript.Text
	}
	meta, _ := json.Marshal(req.Metadata)
	return `Analyze this footage and return one JSON object only. Use this unified schema: summary (string), scenes (array of {description,start_ms,end_ms,tags}), objects (array of {name,count,confidence}), actions (array of {name,confidence}), mood (array of strings), raw_tags (array of strings), shots (array of {start_ms,end_ms,description,tags,confidence}), confidence (number). Also include analysis with the legacy fields asset_type, scene_tags, subjects, people_count, shot_size, camera_motion, lighting, audio_type, has_speech, summary, usable_as, mood_tags, quality, quality_flags, extra_tags, editorial_reason. ` + normalize.VocabularyPrompt() + `Metadata: ` + string(meta) + ` Transcript: ` + tr
}
