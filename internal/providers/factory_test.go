package providers

import (
	"testing"

	"github.com/ev/timingdex/internal/config"
)

func TestVolcenginePlanRolesAreSeparated(t *testing.T) {
	cfg := config.ProvidersConfig{
		VolcAgentPlan:           config.ProviderConfig{Enabled: true, Protocol: "openai_chat", BaseURL: "https://agent.example/v3", Path: "chat/completions", Model: "agent-model"},
		VolcCodingPlan:          config.ProviderConfig{Enabled: true, Protocol: "openai_chat", BaseURL: "https://coding.example/v3", Path: "chat/completions", Model: "coding-model"},
		VolcAgentPlanEmbedding:  config.ProviderConfig{Enabled: true, Protocol: "openai_embeddings", BaseURL: "https://agent.example/v3", Path: "embeddings", Model: "agent-embedding"},
		VolcCodingPlanEmbedding: config.ProviderConfig{Enabled: true, Protocol: "openai_embeddings", BaseURL: "https://coding.example/v3", Path: "embeddings", Model: "coding-embedding"},
		VolcASR:                 config.VolcASRConfig{Enabled: true, APIKey: "test-key", RequestModel: "bigmodel", Model: "doubao-seed-asr-2.0"},
	}
	curator, err := NewTagCurator("volc_agent_plan", cfg)
	if err != nil || curator == nil || curator.Model() != "agent-model" {
		t.Fatalf("agent curator=%v err=%v", curator, err)
	}
	curator, err = NewTagCurator("volc_coding_plan", cfg)
	if err != nil || curator == nil || curator.Model() != "coding-model" {
		t.Fatalf("coding curator=%v err=%v", curator, err)
	}
	embedder, err := NewEmbedder("volc_agent_plan_embedding", cfg)
	if err != nil || embedder == nil || embedder.Model() != "agent-embedding" {
		t.Fatalf("agent embedder=%v err=%v", embedder, err)
	}
	embedder, err = NewEmbedder("volc_coding_plan_embedding", cfg)
	if err != nil || embedder == nil || embedder.Model() != "coding-embedding" {
		t.Fatalf("coding embedder=%v err=%v", embedder, err)
	}
	asr, err := NewASR("volcengine_asr", cfg)
	if err != nil || asr == nil || asr.Model() != "doubao-seed-asr-2.0" {
		t.Fatalf("asr=%v err=%v", asr, err)
	}
}

func TestVideoFactoryRegistersCompatibleProviderRoutes(t *testing.T) {
	cfg := config.ProvidersConfig{
		QwenVideo: config.ProviderConfig{Enabled: true, Protocol: "openai_video", BaseURL: "https://qwen.example/v1", Path: "chat/completions", Model: "qwen-vl"},
		VolcVideo: config.ProviderConfig{Enabled: true, Protocol: "openai_video", BaseURL: "https://ark.example/v3", Path: "chat/completions", Model: "doubao-vision"},
		LocalVLM:  config.ProviderConfig{Enabled: true, Protocol: "openai_video", BaseURL: "http://127.0.0.1:1234/v1", Path: "chat/completions", Model: "local-vl"},
	}
	for _, name := range []string{"qwen_video", "volcengine_video", "local_vlm"} {
		provider, err := NewVideoUnderstandingProvider(name, nil, cfg)
		if err != nil || provider == nil || provider.Name() != name {
			t.Fatalf("provider %q = %v, err=%v", name, provider, err)
		}
	}
}
