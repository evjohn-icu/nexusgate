package providers

import (
	"fmt"
	"strings"

	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/providers/common"
	"github.com/evjohn-icu/nexusslate/internal/providers/embedding"
	"github.com/evjohn-icu/nexusslate/internal/providers/externalalign"
	"github.com/evjohn-icu/nexusslate/internal/providers/gemini"
	"github.com/evjohn-icu/nexusslate/internal/providers/multiframe"
	"github.com/evjohn-icu/nexusslate/internal/providers/openaivideo"
	"github.com/evjohn-icu/nexusslate/internal/providers/qwen"
	"github.com/evjohn-icu/nexusslate/internal/providers/repurpose"
	"github.com/evjohn-icu/nexusslate/internal/providers/shotdetect"
	"github.com/evjohn-icu/nexusslate/internal/providers/stepfun"
	"github.com/evjohn-icu/nexusslate/internal/providers/tagcurator"
	videoproviders "github.com/evjohn-icu/nexusslate/internal/providers/video"
	"github.com/evjohn-icu/nexusslate/internal/providers/volcasr"
)

// openai_multiframe is the protocol for endpoints that understand still
// frames only (llama.cpp, LM Studio, vLLM, SGLang). NexusSlate samples frames
// deterministically and the model describes what it sees.
const ProtocolOpenAIMultiframe = "openai_multiframe"

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
		switch candidate.cfg.Protocol {
		case "", "openai_video":
			if err := registry.Register(&openaivideo.Provider{ProviderName: candidate.name, Endpoint: endpoint(candidate.cfg), ModelName: candidate.cfg.Model, Path: candidate.cfg.Path, MaxInlineBytes: candidate.cfg.MaxInlineVideoBytes}); err != nil {
				return nil, err
			}
		case ProtocolOpenAIMultiframe:
			// The multiframe endpoint never sees a video; NexusSlate samples
			// frames deterministically and the model describes them. The
			// pipeline detects the protocol via MultiframeShotAnalyzer and
			// runs the shot/refinement flow instead of window analysis.
			if err := registry.Register(&multiframe.Provider{ProviderName: candidate.name, Endpoint: endpoint(candidate.cfg), ModelName: candidate.cfg.Model, Path: candidate.cfg.Path}); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("video provider %q requires openai_video or openai_multiframe protocol, got %q", candidate.name, candidate.cfg.Protocol)
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

// NewShotDetector builds the deterministic shot-boundary detector from
// config. A nil detector means the multiframe path must fall back to VLM
// window analysis for its boundaries.
func NewShotDetector(c config.ProvidersConfig) (shotdetect.Detector, error) {
	if !c.ShotDetection.Enabled {
		return nil, nil
	}
	switch c.ShotDetection.Mode {
	case "", config.ShotDetectionModeExternalCommand:
		if c.ShotDetection.Command == "" {
			return nil, fmt.Errorf("shot_detection.command is required in external_command mode")
		}
		return &shotdetect.ExternalCommand{Command: c.ShotDetection.Command, Args: c.ShotDetection.Args}, nil
	case config.ShotDetectionModeFFmpegScene:
		return &shotdetect.FFmpegScene{Threshold: c.ShotDetection.SceneThreshold}, nil
	default:
		return nil, fmt.Errorf("unsupported shot detection mode %q", c.ShotDetection.Mode)
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

// ProviderInfo is the non-secret projection of an enabled provider block:
// how the pipeline addresses it, over what protocol, and with which model.
// Credentials never appear here.
type ProviderInfo struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol,omitempty"`
	Model    string `json:"model,omitempty"`
}

// ProviderCapability is one pipeline capability's provider view: what the
// config selects as primary/fallback, and which blocks are actually enabled.
type ProviderCapability struct {
	Primary    string         `json:"primary"`
	Fallbacks  []string       `json:"fallbacks,omitempty"`
	Configured []ProviderInfo `json:"configured"`
}

// ProviderSummary renders every pipeline capability's provider configuration
// for /api/v1/agent/capabilities and the diagnostics pages. It lets an
// operator see "which providers is this Hub actually running" without poking
// the secret store; a selected-but-disabled provider shows up as a primary
// name with no matching row under configured.
type ProviderSummary struct {
	ASR        ProviderCapability `json:"asr"`
	Vision     ProviderCapability `json:"vision"`
	Repurpose  ProviderCapability `json:"repurpose"`
	TagCurator ProviderCapability `json:"tag_curator"`
	Embedding  ProviderCapability `json:"embedding"`
	Alignment  ProviderCapability `json:"alignment"`
}

// SummarizeProviders builds the provider view from configuration. An empty
// protocol means the block uses its endpoint's native protocol (openai_chat,
// openai_video, ...) — the same meaning the factory applies.
func SummarizeProviders(c config.ProvidersConfig) ProviderSummary {
	type block struct {
		name     string
		protocol string
		model    string
		enabled  bool
	}
	capability := func(primary string, fallbacks []string, blocks ...block) ProviderCapability {
		pc := ProviderCapability{Primary: primary, Fallbacks: fallbacks}
		for _, b := range blocks {
			if b.enabled {
				pc.Configured = append(pc.Configured, ProviderInfo{Name: b.name, Protocol: b.protocol, Model: b.model})
			}
		}
		return pc
	}
	return ProviderSummary{
		ASR: capability(c.ASRPrimary, []string{c.ASRFallback},
			block{"stepfun", c.StepFun.Protocol, c.StepFun.Model, c.StepFun.Enabled},
			block{"qwen", c.Qwen.Protocol, c.Qwen.Model, c.Qwen.Enabled},
			block{"volcengine_asr", "", c.VolcASR.Model, c.VolcASR.Enabled}),
		Vision: capability(c.VisionPrimary, c.VisionFallback,
			block{"gemini", c.Gemini.Protocol, c.Gemini.Model, c.Gemini.Enabled},
			block{"qwen_video", c.QwenVideo.Protocol, c.QwenVideo.Model, c.QwenVideo.Enabled},
			block{"volcengine_video", c.VolcVideo.Protocol, c.VolcVideo.Model, c.VolcVideo.Enabled},
			block{"local_vlm", c.LocalVLM.Protocol, c.LocalVLM.Model, c.LocalVLM.Enabled}),
		Repurpose: capability(c.RepurposePrimary, nil,
			block{"openai_chat", c.Repurpose.Protocol, c.Repurpose.Model, c.Repurpose.Enabled},
			block{"volc_agent_plan", c.VolcAgentPlan.Protocol, c.VolcAgentPlan.Model, c.VolcAgentPlan.Enabled},
			block{"volc_coding_plan", c.VolcCodingPlan.Protocol, c.VolcCodingPlan.Model, c.VolcCodingPlan.Enabled}),
		TagCurator: capability(c.TagCuratorPrimary, nil,
			block{"openai_chat", c.TagCurator.Protocol, c.TagCurator.Model, c.TagCurator.Enabled},
			block{"volc_agent_plan", c.VolcAgentPlan.Protocol, c.VolcAgentPlan.Model, c.VolcAgentPlan.Enabled},
			block{"volc_coding_plan", c.VolcCodingPlan.Protocol, c.VolcCodingPlan.Model, c.VolcCodingPlan.Enabled}),
		Embedding: capability(c.EmbeddingPrimary, nil,
			block{"openai_embeddings", c.Embedding.Protocol, c.Embedding.Model, c.Embedding.Enabled},
			block{"volc_agent_plan_embedding", c.VolcAgentPlanEmbedding.Protocol, c.VolcAgentPlanEmbedding.Model, c.VolcAgentPlanEmbedding.Enabled},
			block{"volc_coding_plan_embedding", c.VolcCodingPlanEmbedding.Protocol, c.VolcCodingPlanEmbedding.Model, c.VolcCodingPlanEmbedding.Enabled}),
		Alignment: capability(c.AlignmentPrimary, nil,
			block{"external_command", "", c.Alignment.Model, c.Alignment.Enabled}),
	}
}

// ValidateProviderConfig is the startup fail-fast for the configured provider
// route. The provider factories defer key use to call time and return a nil
// provider for a disabled block, so without this check a Hub can come up
// "healthy" with an ASR/vision/repurpose route that dies on the first real
// job. config.Load resolves api_key_env into api_key before this runs, so an
// enabled provider whose key env never resolved surfaces here instead of at
// job time. A selected-but-disabled block is deliberately allowed: the shipped
// default config selects the recommended providers while leaving them disabled
// until the operator enables them.
func ValidateProviderConfig(c config.ProvidersConfig) error {
	type check struct {
		found   bool
		enabled bool
		keyed   bool
		keyEnv  string
		apiKey  string
	}
	pc := func(p config.ProviderConfig) (keyed bool, keyEnv, apiKey string) {
		return p.APIKeyEnv != "" || p.AuthHeader != "" || p.AuthScheme != "", p.APIKeyEnv, p.APIKey
	}
	validate := func(role, name string, chk check) error {
		name = strings.TrimSpace(name)
		if name == "" || name == "none" {
			return nil
		}
		if !chk.found {
			return fmt.Errorf("%s provider %q is not supported", role, name)
		}
		if !chk.enabled || !chk.keyed {
			return nil
		}
		if strings.TrimSpace(chk.apiKey) == "" {
			if chk.keyEnv != "" {
				return fmt.Errorf("%s provider %q is enabled but has no API key: environment variable %q is unset", role, name, chk.keyEnv)
			}
			return fmt.Errorf("%s provider %q is enabled but has no API key", role, name)
		}
		return nil
	}

	asrCheck := func(name string) check {
		switch name {
		case "stepfun":
			k, env, key := pc(c.StepFun)
			return check{true, c.StepFun.Enabled, k, env, key}
		case "qwen":
			k, env, key := pc(c.Qwen)
			return check{true, c.Qwen.Enabled, k, env, key}
		case "volcengine_asr":
			return check{true, c.VolcASR.Enabled, c.VolcASR.APIKeyEnv != "" || c.VolcASR.APIKey != "", c.VolcASR.APIKeyEnv, c.VolcASR.APIKey}
		}
		return check{}
	}
	visionCheck := func(name string) check {
		switch name {
		case "gemini":
			k, env, key := pc(c.Gemini)
			return check{true, c.Gemini.Enabled, k, env, key}
		case "qwen_video":
			k, env, key := pc(c.QwenVideo)
			return check{true, c.QwenVideo.Enabled, k, env, key}
		case "volcengine_video":
			k, env, key := pc(c.VolcVideo)
			return check{true, c.VolcVideo.Enabled, k, env, key}
		case "local_vlm":
			k, env, key := pc(c.LocalVLM)
			return check{true, c.LocalVLM.Enabled, k, env, key}
		}
		return check{}
	}
	planCheck := func(name string, enabled bool, p config.ProviderConfig) check {
		k, env, key := pc(p)
		return check{true, enabled, k, env, key}
	}
	alignmentCheck := func(name string) check {
		if name == "external_command" {
			return check{true, c.Alignment.Enabled, false, "", ""}
		}
		return check{}
	}
	keylessSelected := func(name string) (check, bool) {
		switch name {
		case "heuristic":
			return check{found: true, enabled: true, keyed: false}, true
		case "", "none":
			return check{}, true
		}
		return check{}, false
	}

	for _, name := range []string{c.ASRPrimary, c.ASRFallback} {
		if err := validate("asr", name, asrCheck(name)); err != nil {
			return err
		}
	}
	for _, name := range append([]string{c.VisionPrimary}, c.VisionFallback...) {
		if err := validate("vision", name, visionCheck(name)); err != nil {
			return err
		}
	}
	for _, name := range []string{c.AlignmentPrimary} {
		if err := validate("alignment", name, alignmentCheck(name)); err != nil {
			return err
		}
	}
	for _, name := range []string{c.TagCuratorPrimary} {
		chk, ok := keylessSelected(name)
		if !ok {
			switch name {
			case "openai_chat":
				chk = planCheck(name, c.TagCurator.Enabled, c.TagCurator)
			case "volc_agent_plan":
				chk = planCheck(name, c.VolcAgentPlan.Enabled, c.VolcAgentPlan)
			case "volc_coding_plan":
				chk = planCheck(name, c.VolcCodingPlan.Enabled, c.VolcCodingPlan)
			}
		}
		if err := validate("tag curator", name, chk); err != nil {
			return err
		}
	}
	for _, name := range []string{c.EmbeddingPrimary} {
		chk, ok := keylessSelected(name)
		if !ok {
			switch name {
			case "openai_embeddings", "gemini_embed_content":
				chk = planCheck(name, c.Embedding.Enabled, c.Embedding)
			case "volc_agent_plan_embedding":
				chk = planCheck(name, c.VolcAgentPlanEmbedding.Enabled, c.VolcAgentPlanEmbedding)
			case "volc_coding_plan_embedding":
				chk = planCheck(name, c.VolcCodingPlanEmbedding.Enabled, c.VolcCodingPlanEmbedding)
			}
		}
		if err := validate("embedding", name, chk); err != nil {
			return err
		}
	}
	for _, name := range []string{c.RepurposePrimary} {
		chk, ok := keylessSelected(name)
		if !ok {
			switch name {
			case "openai_chat":
				chk = planCheck(name, c.Repurpose.Enabled, c.Repurpose)
			}
		}
		if err := validate("repurpose", name, chk); err != nil {
			return err
		}
	}
	return nil
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
