package embedding

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"

	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

// Provider implements real remote embeddings. OpenAI-compatible endpoints
// cover Ollama, Qwen and most local gateways; Gemini's native protocol is
// available for users who prefer its embedding model.
type Provider struct {
	Endpoint  common.Endpoint
	ModelName string
	Path      string
	Protocol  string
}

func (p *Provider) Name() string { return "embedding" }
func (p *Provider) Model() string {
	if p.ModelName == "" {
		return "text-embedding-model"
	}
	return p.ModelName
}

func (p *Provider) Embed(ctx context.Context, inputs []string) ([][]float64, error) {
	if len(inputs) == 0 {
		return [][]float64{}, nil
	}
	protocol := p.Protocol
	if protocol == "" {
		protocol = "openai_embeddings"
	}
	path := p.Path
	if protocol == "gemini_embed_content" {
		if path == "" {
			path = "models/" + p.Model() + ":embedContent"
		}
		vectors := make([][]float64, 0, len(inputs))
		for _, input := range inputs {
			vector, err := p.embedGemini(ctx, path, input)
			if err != nil {
				return nil, err
			}
			vectors = append(vectors, vector[0])
		}
		return vectors, nil
	}
	if protocol != "openai_embeddings" {
		return nil, fmt.Errorf("unsupported embedding protocol %q", protocol)
	}
	if path == "" {
		path = "embeddings"
	}
	req, err := p.Endpoint.NewRequest(ctx, http.MethodPost, path, map[string]any{"model": p.Model(), "input": inputs, "encoding_format": "float"})
	if err != nil {
		return nil, err
	}
	resp, err := p.Endpoint.Client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, common.ReadErrorWithSecret(resp, p.Endpoint.APIKey)
	}
	raw, err := common.ReadBody(resp.Body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode embeddings: %w", err)
	}
	if len(out.Data) != len(inputs) {
		return nil, fmt.Errorf("embedding response count %d does not match request count %d", len(out.Data), len(inputs))
	}
	vectors := make([][]float64, len(inputs))
	for _, item := range out.Data {
		if item.Index < 0 || item.Index >= len(vectors) || vectors[item.Index] != nil || !validVector(item.Embedding) {
			return nil, fmt.Errorf("invalid embedding response")
		}
		vectors[item.Index] = item.Embedding
	}
	for _, vector := range vectors {
		if vector == nil {
			return nil, fmt.Errorf("invalid embedding response")
		}
	}
	return vectors, nil
}

func (p *Provider) embedGemini(ctx context.Context, path, input string) ([][]float64, error) {
	body := map[string]any{"model": "models/" + p.Model(), "content": map[string]any{"parts": []any{map[string]any{"text": input}}}}
	req, err := p.Endpoint.NewRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	resp, err := p.Endpoint.Client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, common.ReadErrorWithSecret(resp, p.Endpoint.APIKey)
	}
	var out struct {
		Embedding struct {
			Values []float64 `json:"values"`
		} `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode Gemini embedding: %w", err)
	}
	if !validVector(out.Embedding.Values) {
		return nil, fmt.Errorf("Gemini embedding response is empty")
	}
	return [][]float64{out.Embedding.Values}, nil
}

func validVector(vector []float64) bool {
	if len(vector) == 0 {
		return false
	}
	var norm float64
	for _, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
		norm += value * value
	}
	return !math.IsNaN(norm) && !math.IsInf(norm, 0) && norm > 0
}
