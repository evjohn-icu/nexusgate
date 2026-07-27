package providers

import (
	"fmt"

	"github.com/ev/timingdex/internal/config"
	"github.com/ev/timingdex/internal/providers/common"
	"github.com/ev/timingdex/internal/providers/embedding"
	"github.com/ev/timingdex/internal/providers/externalalign"
	"github.com/ev/timingdex/internal/providers/gemini"
	"github.com/ev/timingdex/internal/providers/openaivideo"
	"github.com/ev/timingdex/internal/providers/qwen"
	"github.com/ev/timingdex/internal/providers/repurpose"
	"github.com/ev/timingdex/internal/providers/stepfun"
	"github.com/ev/timingdex/internal/providers/tagcurator"
	videoproviders "github.com/ev/timingdex/internal/providers/video"
	"github.com/ev/timingdex/internal/providers/volcasr"
)

func endpoint(c config.ProviderConfig) common.Endpoint {
	return common.Endpoint{BaseURL: c.BaseURL, APIKey: c.APIKey, AuthHeader: c.AuthHeader, AuthScheme: c.AuthScheme, ExtraHeaders: c.ExtraHeaders, TimeoutSeconds: c.TimeoutSeconds}
}

func NewASR(name string, c config.ProvidersConfig) (ASR, error) {
	switch name {
	case "", "none":
		return nil, nil
	case "stepfun":
		if !c.StepFun.Enabled {
			return nil, nil
		}
		return &stepfun.ASR{Endpoint: endpoint(c.StepFun), ModelName: c.StepFun.Model, Path: c.StepFun.Path}, nil
	case "qwen":
		if !c.Qwen.Enabled {
			return nil, nil
		}
		return &qwen.ASR{Endpoint: endpoint(c.Qwen), ModelName: c.Qwen.Model, Path: c.Qwen.Path}, nil
	case "volcengine_asr":
		if !c.VolcASR.Enabled {
			return nil, nil
		}
		return &volcasr.ASR{
			URL: c.VolcASR.URL, APIKey: c.VolcASR.APIKey, ResourceID: c.VolcASR.ResourceID,
			RequestModel: c.VolcASR.RequestModel, ModelName: c.VolcASR.Model, UID: c.VolcASR.UID, TimeoutSeconds: c.VolcASR.TimeoutSeconds,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported ASR provider %q", name)
	}
}

func NewVision(name string, c config.ProvidersConfig) (Vision, error) {
	switch name {
	case "", "none":
		return nil, nil
	case "gemini":
		if !c.Gemini.Enabled {
			return nil, nil
		}
		return &gemini.Vision{Endpoint: endpoint(c.Gemini), ModelName: c.Gemini.Model, Path: c.Gemini.Path, Protocol: c.Gemini.Protocol}, nil
	default:
		return nil, fmt.Errorf("unsupported vision provider %q", name)
	}
}

func NewVideoUnderstandingProvider(name string, fallbacks []string, c config.ProvidersConfig) (videoproviders.VideoUnderstandingProvider, error) {
	if name == "" || name == "none" {
		return nil, nil
	}
	registry := videoproviders.NewRegistry()
	if c.Gemini.Enabled {
		if err := registry.Register(&gemini.VideoProvider{Vision: &gemini.Vision{Endpoint: endpoint(c.Gemini), ModelName: c.Gemini.Model, Path: c.Gemini.Path, Protocol: c.Gemini.Protocol}}); err != nil {
			return nil, err
		}
	}
	for _, candidate := range []struct {
		name string
		cfg  config.ProviderConfig
	}{
		{name: "qwen_video", cfg: c.QwenVideo},
		{name: "volcengine_video", cfg: c.VolcVideo},
		{name: "local_vlm", cfg: c.LocalVLM},
	} {
		if !candidate.cfg.Enabled {
			continue
		}
		if candidate.cfg.Protocol != "" && candidate.cfg.Protocol != "openai_video" {
			return nil, fmt.Errorf("video provider %q requires openai_video protocol, got %q", candidate.name, candidate.cfg.Protocol)
		}
		if err := registry.Register(&openaivideo.Provider{ProviderName: candidate.name, Endpoint: endpoint(candidate.cfg), ModelName: candidate.cfg.Model, Path: candidate.cfg.Path}); err != nil {
			return nil, err
		}
	}
	return videoproviders.NewRouter(registry, name, fallbacks)
}

func NewAlignment(name string, c config.ProvidersConfig) (Alignment, error) {
	switch name {
	case "", "none":
		return nil, nil
	case "external_command":
		if !c.Alignment.Enabled {
			return nil, nil
		}
		return &externalalign.Provider{Command: c.Alignment.Command, Args: c.Alignment.Args, ModelName: c.Alignment.Model}, nil
	default:
		return nil, fmt.Errorf("unsupported alignment provider %q", name)
	}
}

func NewTagCurator(name string, c config.ProvidersConfig) (TagCurator, error) {
	switch name {
	case "", "none", "heuristic":
		return nil, nil
	case "openai_chat":
		if !c.TagCurator.Enabled {
			return nil, nil
		}
		if c.TagCurator.Protocol != "" && c.TagCurator.Protocol != "openai_chat" && c.TagCurator.Protocol != "gemini_generate_content" {
			return nil, fmt.Errorf("unsupported tag curator protocol %q", c.TagCurator.Protocol)
		}
		return &tagcurator.Provider{
			Endpoint:  endpoint(c.TagCurator),
			ModelName: c.TagCurator.Model,
			Path:      c.TagCurator.Path,
			Protocol:  c.TagCurator.Protocol,
		}, nil
	case "volc_agent_plan":
		if !c.VolcAgentPlan.Enabled {
			return nil, nil
		}
		return newPlanTagCurator(c.VolcAgentPlan)
	case "volc_coding_plan":
		if !c.VolcCodingPlan.Enabled {
			return nil, nil
		}
		return newPlanTagCurator(c.VolcCodingPlan)
	default:
		return nil, fmt.Errorf("unsupported tag curator provider %q", name)
	}
}

func NewEmbedder(name string, c config.ProvidersConfig) (Embedder, error) {
	switch name {
	case "", "none":
		return nil, nil
	case "openai_embeddings", "gemini_embed_content":
		if !c.Embedding.Enabled {
			return nil, nil
		}
		protocol := c.Embedding.Protocol
		if protocol == "" {
			protocol = name
		}
		if protocol != "openai_embeddings" && protocol != "gemini_embed_content" {
			return nil, fmt.Errorf("unsupported embedding protocol %q", protocol)
		}
		return &embedding.Provider{Endpoint: endpoint(c.Embedding), ModelName: c.Embedding.Model, Path: c.Embedding.Path, Protocol: protocol}, nil
	case "volc_agent_plan_embedding":
		if !c.VolcAgentPlanEmbedding.Enabled {
			return nil, nil
		}
		return newPlanEmbedder(c.VolcAgentPlanEmbedding)
	case "volc_coding_plan_embedding":
		if !c.VolcCodingPlanEmbedding.Enabled {
			return nil, nil
		}
		return newPlanEmbedder(c.VolcCodingPlanEmbedding)
	default:
		return nil, fmt.Errorf("unsupported embedding provider %q", name)
	}
}

func NewRepurposePlanner(name string, c config.ProvidersConfig) (RepurposePlanner, error) {
	switch name {
	case "", "none", "heuristic":
		return nil, nil
	case "openai_chat":
		if !c.Repurpose.Enabled {
			return nil, nil
		}
		if c.Repurpose.Protocol != "" && c.Repurpose.Protocol != "openai_chat" {
			return nil, fmt.Errorf("unsupported repurpose planner protocol %q", c.Repurpose.Protocol)
		}
		return &repurpose.Provider{Endpoint: endpoint(c.Repurpose), ModelName: c.Repurpose.Model, Path: c.Repurpose.Path}, nil
	default:
		return nil, fmt.Errorf("unsupported repurpose planner %q", name)
	}
}

func newPlanTagCurator(c config.ProviderConfig) (TagCurator, error) {
	if c.Protocol != "" && c.Protocol != "openai_chat" {
		return nil, fmt.Errorf("Volcengine plan tag curator requires openai_chat protocol, got %q", c.Protocol)
	}
	return &tagcurator.Provider{Endpoint: endpoint(c), ModelName: c.Model, Path: c.Path, Protocol: "openai_chat"}, nil
}

func newPlanEmbedder(c config.ProviderConfig) (Embedder, error) {
	if c.Protocol != "" && c.Protocol != "openai_embeddings" {
		return nil, fmt.Errorf("Volcengine plan embedder requires openai_embeddings protocol, got %q", c.Protocol)
	}
	return &embedding.Provider{Endpoint: endpoint(c), ModelName: c.Model, Path: c.Path, Protocol: "openai_embeddings"}, nil
}
