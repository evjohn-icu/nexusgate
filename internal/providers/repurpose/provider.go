package repurpose

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

type Provider struct {
	Endpoint  common.Endpoint
	ModelName string
	Path      string
}

var _ common.RepurposePlanner = (*Provider)(nil)

func (p *Provider) Name() string { return "openai_chat_repurpose" }

func (p *Provider) Model() string {
	if p.ModelName == "" {
		return "local-small-instruct"
	}
	return p.ModelName
}

func (p *Provider) Plan(ctx context.Context, brief domain.RepurposeBrief) (domain.RepurposePlanDraft, error) {
	input, err := json.Marshal(brief)
	if err != nil {
		return domain.RepurposePlanDraft{}, err
	}
	system := `You are a footage repurpose planner. Return JSON only with this exact shape: {"title":"short plan title","sections":[{"role":"opening|human_activity|mood|transition|ending","query":"one concise searchable semantic phrase","duration_ms":5000,"required":true,"rationale":"short reason"}]}. Create 3 to 6 sections whose duration_ms adds up approximately to the requested duration. Do not invent asset IDs, shot IDs, timestamps, or media facts. The query must be useful for a local footage search index.`
	text, err := p.completeJSON(ctx, system, string(input))
	if err != nil {
		return domain.RepurposePlanDraft{}, err
	}
	var draft domain.RepurposePlanDraft
	if err := json.Unmarshal([]byte(text), &draft); err != nil {
		return domain.RepurposePlanDraft{}, fmt.Errorf("decode repurpose planner JSON: %w", err)
	}
	draft.Title = strings.TrimSpace(draft.Title)
	if draft.Title == "" || len(draft.Sections) == 0 || len(draft.Sections) > 6 {
		return domain.RepurposePlanDraft{}, fmt.Errorf("invalid repurpose planner draft")
	}
	for i := range draft.Sections {
		draft.Sections[i].Role = strings.TrimSpace(draft.Sections[i].Role)
		draft.Sections[i].Query = strings.TrimSpace(draft.Sections[i].Query)
		if draft.Sections[i].Role == "" || draft.Sections[i].Query == "" || draft.Sections[i].DurationMS <= 0 {
			return domain.RepurposePlanDraft{}, fmt.Errorf("invalid repurpose planner section %d", i)
		}
	}
	return draft, nil
}

func (p *Provider) completeJSON(ctx context.Context, system, user string) (string, error) {
	path := p.Path
	if path == "" {
		path = "chat/completions"
	}
	body := map[string]any{
		"model": p.Model(),
		"messages": []any{
			map[string]any{"role": "system", "content": system},
			map[string]any{"role": "user", "content": user},
		},
		"response_format": map[string]any{"type": "json_object"},
		"temperature":     0.1,
	}
	req, err := p.Endpoint.NewRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return "", err
	}
	resp, err := p.Endpoint.Client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", common.ReadError(resp)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Choices) == 0 {
		return "", fmt.Errorf("decode repurpose planner response: missing choices")
	}
	text := strings.TrimSpace(envelope.Choices[0].Message.Content)
	text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "```json"), "```"))
	if text == "" {
		return "", fmt.Errorf("empty repurpose planner response")
	}
	return text, nil
}
