package providerchannels

import (
	"fmt"
	"strings"

	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/providerpool"
)

// FromLegacyConfig converts the primary/fallback provider selectors from the
// pre-channel config into deterministic, ordered channel metadata. API keys
// and other secret values are ignored; members receive only stable secret
// references.
func FromLegacyConfig(providers config.ProvidersConfig) ([]Channel, error) {
	var channels []Channel

	addRoute := func(capability Capability, names ...string) error {
		seen := make(map[string]struct{}, len(names))
		for routeOrder, name := range names {
			name = strings.TrimSpace(name)
			if name == "" || name == "none" || name == "heuristic" {
				continue
			}
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}

			spec, err := legacyProvider(capability, name, providers)
			if err != nil {
				return err
			}
			channels = append(channels, makeLegacyChannel(capability, name, routeOrder, spec))
		}
		return nil
	}

	if err := addRoute(CapabilityASR, providers.ASRPrimary, providers.ASRFallback); err != nil {
		return nil, err
	}
	if err := addRoute(CapabilityVideoAnalysis, append([]string{providers.VisionPrimary}, providers.VisionFallback...)...); err != nil {
		return nil, err
	}
	if err := addRoute(CapabilityAlignment, providers.AlignmentPrimary); err != nil {
		return nil, err
	}
	if err := addRoute(CapabilityTagCurator, providers.TagCuratorPrimary); err != nil {
		return nil, err
	}
	if err := addRoute(CapabilityEmbedding, providers.EmbeddingPrimary); err != nil {
		return nil, err
	}
	if err := addRoute(CapabilityRepurpose, providers.RepurposePrimary); err != nil {
		return nil, err
	}
	return channels, nil
}

// ChannelsFromLegacyConfig is a descriptive alias for FromLegacyConfig.
func ChannelsFromLegacyConfig(providers config.ProvidersConfig) ([]Channel, error) {
	return FromLegacyConfig(providers)
}

// NewExecutorFromLegacyConfig converts the legacy selectors and immediately
// creates an executor for them.
func NewExecutorFromLegacyConfig(providers config.ProvidersConfig, options ...providerpool.Options) (*Executor, error) {
	channels, err := FromLegacyConfig(providers)
	if err != nil {
		return nil, err
	}
	return NewExecutor(channels, options...)
}

type legacyProviderSpec struct {
	enabled          bool
	protocol         string
	endpoint         string
	path             string
	model            string
	authHeader       string
	authScheme       string
	timeoutSeconds   int
	secretConfigured bool
	secretRef        string
}

func legacyProvider(capability Capability, name string, providers config.ProvidersConfig) (legacyProviderSpec, error) {
	provider := func(c config.ProviderConfig) legacyProviderSpec {
		return legacyProviderSpec{
			enabled: c.Enabled, protocol: c.Protocol, endpoint: c.BaseURL, path: c.Path, model: c.Model,
			authHeader: c.AuthHeader, authScheme: c.AuthScheme, timeoutSeconds: c.TimeoutSeconds,
			secretConfigured: strings.TrimSpace(c.APIKey) != "" || strings.TrimSpace(c.APIKeyEnv) != "",
			secretRef:        "provider/" + name,
		}
	}

	switch capability {
	case CapabilityASR:
		switch name {
		case "stepfun":
			return provider(providers.StepFun), nil
		case "qwen":
			return provider(providers.Qwen), nil
		case "volcengine_asr":
			return legacyProviderSpec{enabled: providers.VolcASR.Enabled, protocol: "volcengine_asr", endpoint: providers.VolcASR.URL, model: providers.VolcASR.Model, secretConfigured: strings.TrimSpace(providers.VolcASR.APIKey) != "" || strings.TrimSpace(providers.VolcASR.APIKeyEnv) != "", secretRef: "provider/" + name}, nil
		}
	case CapabilityVideoAnalysis:
		switch name {
		case "gemini":
			return provider(providers.Gemini), nil
		case "qwen_video":
			return provider(providers.QwenVideo), nil
		case "volcengine_video":
			return provider(providers.VolcVideo), nil
		case "local_vlm":
			return provider(providers.LocalVLM), nil
		}
	case CapabilityAlignment:
		if name == "external_command" {
			return legacyProviderSpec{enabled: providers.Alignment.Enabled, protocol: name, model: providers.Alignment.Model}, nil
		}
	case CapabilityTagCurator:
		switch name {
		case "openai_chat":
			return provider(providers.TagCurator), nil
		case "volc_agent_plan":
			return provider(providers.VolcAgentPlan), nil
		case "volc_coding_plan":
			return provider(providers.VolcCodingPlan), nil
		}
	case CapabilityEmbedding:
		switch name {
		case "openai_embeddings", "gemini_embed_content":
			return provider(providers.Embedding), nil
		case "volc_agent_plan_embedding":
			return provider(providers.VolcAgentPlanEmbedding), nil
		case "volc_coding_plan_embedding":
			return provider(providers.VolcCodingPlanEmbedding), nil
		}
	case CapabilityRepurpose:
		if name == "openai_chat" {
			return provider(providers.Repurpose), nil
		}
	}
	return legacyProviderSpec{}, fmt.Errorf("providerchannels: unsupported %s provider %q", capability, name)
}

func makeLegacyChannel(capability Capability, name string, routeOrder int, spec legacyProviderSpec) Channel {
	id := "legacy/" + string(capability) + "/" + name
	memberID := id + "/default"
	member := Member{
		ID: memberID, ChannelID: id, Provider: name, ProviderName: name, Label: "default",
		SecretRef: spec.secretRef, SecretConfigured: spec.secretConfigured, Enabled: spec.enabled,
		Weight: 1, MaxInflight: 1, Capabilities: []Capability{capability},
	}
	if spec.secretRef == "" {
		member.SecretConfigured = false
	}
	return Channel{
		ID: id, Capability: capability, Capabilities: []Capability{capability}, Label: name,
		Provider: name, ProviderName: name, Protocol: spec.protocol, Endpoint: spec.endpoint, Path: spec.path,
		Model: spec.model, AuthHeader: spec.authHeader, AuthScheme: spec.authScheme, TimeoutSeconds: spec.timeoutSeconds,
		Enabled: spec.enabled, RouteOrder: routeOrder, Members: []Member{member},
	}
}
