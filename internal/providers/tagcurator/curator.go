package tagcurator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/curator"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

// Provider asks a small OpenAI-compatible model to group unresolved tags.
// Model output is treated as an untrusted draft and converted into the same
// bounded proposals used by the deterministic curator.
type Provider struct {
	Endpoint  common.Endpoint
	ModelName string
	Path      string
	Protocol  string
}

func (p *Provider) Name() string {
	if p.Protocol == "gemini_generate_content" {
		return "gemini_generate_content"
	}
	return "openai_chat"
}

func (p *Provider) Model() string {
	if p.ModelName == "" {
		return "local-small-instruct"
	}
	return p.ModelName
}

type groupDraft struct {
	CanonicalName string   `json:"canonical_name"`
	Aliases       []string `json:"aliases"`
	Category      string   `json:"category"`
	Confidence    float64  `json:"confidence"`
	Reason        string   `json:"reason"`
}

func (p *Provider) Curate(ctx context.Context, unresolved []domain.UnresolvedTag, existing []domain.CanonicalTag) ([]domain.TagProposal, error) {
	input, err := json.Marshal(map[string]any{
		"unresolved_tags": unresolved,
		"canonical_tags":  existing,
	})
	if err != nil {
		return nil, err
	}
	text, err := p.completeJSON(ctx,
		"You group noisy footage tags. Return JSON only as {\"groups\":[{\"canonical_name\":\"snake_case\",\"aliases\":[\"only values from unresolved_tags\"],\"category\":\"general|scene|subject|mood|usable_as|quality\",\"confidence\":0.0,\"reason\":\"short reason\"}]}. Never invent aliases, SQL, IDs, or database actions. Prefer an existing canonical tag when meanings match. Omit uncertain groups.",
		string(input))
	if err != nil {
		return nil, err
	}
	var draft struct {
		Groups []groupDraft `json:"groups"`
	}
	if err := json.Unmarshal([]byte(text), &draft); err != nil {
		return nil, fmt.Errorf("decode tag curator JSON: %w", err)
	}
	return validateDrafts(draft.Groups, unresolved, existing), nil
}

// SummarizeLibrary turns a bounded statistics snapshot into editorial language.
// It cannot inspect raw media and cannot make governance changes.
func (p *Provider) SummarizeLibrary(ctx context.Context, input domain.LibrarySummaryInput) (domain.LibrarySummaryDraft, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return domain.LibrarySummaryDraft{}, err
	}
	text, err := p.completeJSON(ctx,
		"You summarize a personal footage library from statistics only. Return JSON only as {\"summary\":\"a concise Chinese paragraph\",\"themes\":[\"up to 6 short themes\"],\"suitable_for\":[\"up to 6 practical reuse directions\"]}. Do not invent locations, people, events, or footage that are absent from the input. State uncertainty conservatively.",
		string(raw))
	if err != nil {
		return domain.LibrarySummaryDraft{}, err
	}
	var draft domain.LibrarySummaryDraft
	if err := json.Unmarshal([]byte(text), &draft); err != nil {
		return domain.LibrarySummaryDraft{}, fmt.Errorf("decode library summary JSON: %w", err)
	}
	draft.Summary = strings.TrimSpace(draft.Summary)
	if draft.Summary == "" {
		return domain.LibrarySummaryDraft{}, fmt.Errorf("decode library summary JSON: missing summary")
	}
	draft.Themes = boundedStrings(draft.Themes, 6)
	draft.SuitableFor = boundedStrings(draft.SuitableFor, 6)
	return draft, nil
}

func (p *Provider) completeJSON(ctx context.Context, system, user string) (string, error) {
	protocol := p.Protocol
	if protocol == "" {
		protocol = "openai_chat"
	}
	path := p.Path
	var body map[string]any
	if protocol == "gemini_generate_content" {
		if path == "" {
			path = "models/" + p.Model() + ":generateContent"
		}
		body = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": system + "\n\nInput JSON:\n" + user}}}}, "generationConfig": map[string]any{"responseMimeType": "application/json", "temperature": 0.1}}
	} else {
		if protocol != "openai_chat" {
			return "", fmt.Errorf("unsupported tag curator protocol %q", protocol)
		}
		if path == "" {
			path = "chat/completions"
		}
		body = map[string]any{"model": p.Model(), "messages": []any{map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": user}}, "response_format": map[string]any{"type": "json_object"}, "temperature": 0.1}
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
	var text string
	if protocol == "gemini_generate_content" {
		var envelope struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Candidates) == 0 {
			return "", fmt.Errorf("decode Gemini tag curator response: missing candidates")
		}
		for _, part := range envelope.Candidates[0].Content.Parts {
			text += part.Text
		}
	} else {
		var envelope struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Choices) == 0 {
			return "", fmt.Errorf("decode tag curator response: missing choices")
		}
		text = envelope.Choices[0].Message.Content
	}
	text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(text), "```json"), "```"))
	if text == "" {
		return "", fmt.Errorf("empty tag curator response")
	}
	return text, nil
}

func boundedStrings(values []string, max int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, max)
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
			if len(out) == max {
				break
			}
		}
	}
	return out
}

func validateDrafts(drafts []groupDraft, unresolved []domain.UnresolvedTag, existing []domain.CanonicalTag) []domain.TagProposal {
	allowed := make(map[string]domain.UnresolvedTag, len(unresolved))
	for _, item := range unresolved {
		allowed[curator.Normalize(item.NormalizedTag)] = item
	}
	canonicalByName := make(map[string]domain.CanonicalTag, len(existing))
	for _, tag := range existing {
		canonicalByName[curator.Normalize(tag.CanonicalName)] = tag
	}

	usedAliases := map[string]bool{}
	usedCanonical := map[string]bool{}
	proposals := make([]domain.TagProposal, 0, len(drafts))
	for _, draft := range drafts {
		canonical := curator.Normalize(draft.CanonicalName)
		if canonical == "" || usedCanonical[canonical] {
			continue
		}
		aliasSet := map[string]bool{}
		for _, raw := range draft.Aliases {
			normalized := curator.Normalize(raw)
			if _, ok := allowed[normalized]; ok && !usedAliases[normalized] {
				aliasSet[normalized] = true
			}
		}
		if _, ok := allowed[canonical]; ok && !usedAliases[canonical] {
			aliasSet[canonical] = true
		}
		if len(aliasSet) == 0 {
			continue
		}
		aliases := make([]string, 0, len(aliasSet))
		affected := 0
		for alias := range aliasSet {
			aliases = append(aliases, alias)
			affected += allowed[alias].AssetCount
		}
		sort.Strings(aliases)

		confidence := draft.Confidence
		if confidence <= 0 {
			confidence = 0.65
		}
		if confidence > 1 {
			confidence = 1
		}
		reason := strings.TrimSpace(draft.Reason)
		if reason == "" {
			reason = "小模型语义聚类建议；仅生成待人工审核的治理提案"
		}
		category := validCategory(draft.Category)
		payload := map[string]any{"canonical_name": canonical, "aliases": aliases, "category": category}
		proposalType := "create_canonical_with_aliases"
		if tag, ok := canonicalByName[canonical]; ok {
			proposalType = "add_aliases"
			payload = map[string]any{"canonical_tag_id": tag.ID, "aliases": aliases}
		}
		proposals = append(proposals, domain.TagProposal{
			State:          "pending",
			ProposalType:   proposalType,
			CanonicalName:  canonical,
			Payload:        payload,
			Confidence:     confidence,
			Reason:         reason,
			AffectedAssets: affected,
		})
		usedCanonical[canonical] = true
		for _, alias := range aliases {
			usedAliases[alias] = true
		}
	}
	return proposals
}

func validCategory(category string) string {
	switch category {
	case "scene", "subject", "mood", "usable_as", "quality":
		return category
	default:
		return "general"
	}
}
