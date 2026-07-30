package config

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ev/timingdex/internal/media"
)

type ProviderConfig struct {
	Enabled        bool              `json:"enabled"`
	Protocol       string            `json:"protocol,omitempty"`
	BaseURL        string            `json:"base_url"`
	Path           string            `json:"path,omitempty"`
	APIKey         string            `json:"api_key,omitempty"`
	APIKeyEnv      string            `json:"api_key_env,omitempty"`
	Model          string            `json:"model"`
	AuthHeader     string            `json:"auth_header,omitempty"`
	AuthScheme     string            `json:"auth_scheme,omitempty"`
	ExtraHeaders   map[string]string `json:"extra_headers,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	// MaxInlineVideoBytes is the largest video this endpoint accepts inside a
	// request body. It is per-endpoint rather than a shared constant because
	// the ceiling varies by more than an order of magnitude between them, and a
	// relay in front of an API can impose its own lower one. Zero means use the
	// conservative built-in default.
	MaxInlineVideoBytes int64 `json:"max_inline_video_bytes,omitempty"`
}

type ProvidersConfig struct {
	ASRPrimary                  string         `json:"asr_primary"`
	ASRFallback                 string         `json:"asr_fallback,omitempty"`
	VisionPrimary               string         `json:"vision_primary"`
	VisionFallback              []string       `json:"vision_fallback,omitempty"`
	AlignmentPrimary            string         `json:"alignment_primary,omitempty"`
	TagCuratorPrimary           string         `json:"tag_curator_primary,omitempty"`
	TagCuratorFallbackHeuristic bool           `json:"tag_curator_fallback_heuristic"`
	EmbeddingPrimary            string         `json:"embedding_primary,omitempty"`
	RepurposePrimary            string         `json:"repurpose_primary,omitempty"`
	RepurposeFallbackHeuristic  bool           `json:"repurpose_fallback_heuristic"`
	StepFun                     ProviderConfig `json:"stepfun"`
	Qwen                        ProviderConfig `json:"qwen"`
	Gemini                      ProviderConfig `json:"gemini"`
	QwenVideo                   ProviderConfig `json:"qwen_video"`
	VolcVideo                   ProviderConfig `json:"volc_video"`
	LocalVLM                    ProviderConfig `json:"local_vlm"`
	TagCurator                  ProviderConfig `json:"tag_curator"`
	Embedding                   ProviderConfig `json:"embedding"`
	Repurpose                   ProviderConfig `json:"repurpose"`
	// VolcAgentPlan and VolcCodingPlan deliberately remain separate accounts.
	// Their API keys and quota pools must never be substituted for each other.
	// Both expose an OpenAI-compatible chat/embeddings surface, so they can be
	// selected for the curator, library summary, or tag embeddings.
	VolcAgentPlan           ProviderConfig  `json:"volc_agent_plan"`
	VolcCodingPlan          ProviderConfig  `json:"volc_coding_plan"`
	VolcAgentPlanEmbedding  ProviderConfig  `json:"volc_agent_plan_embedding"`
	VolcCodingPlanEmbedding ProviderConfig  `json:"volc_coding_plan_embedding"`
	VolcASR                 VolcASRConfig   `json:"volc_asr"`
	Alignment               AlignmentConfig `json:"alignment"`
}

// VolcASRConfig is intentionally not a ProviderConfig. Seed ASR 2.0 uses
// Volcengine's native binary WebSocket protocol, not OpenAI audio endpoints.
type VolcASRConfig struct {
	Enabled    bool   `json:"enabled"`
	URL        string `json:"url"`
	APIKey     string `json:"api_key,omitempty"`
	APIKeyEnv  string `json:"api_key_env,omitempty"`
	ResourceID string `json:"resource_id"`
	// RequestModel is the protocol-level model_name. Seed ASR 2.0's native
	// endpoint currently expects "bigmodel" even though the catalog name is
	// doubao-seed-asr-2.0.
	RequestModel   string `json:"request_model,omitempty"`
	Model          string `json:"model"`
	UID            string `json:"uid,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type AlignmentConfig struct {
	Enabled bool     `json:"enabled"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Model   string   `json:"model,omitempty"`
}

// SourceStagingConfig controls whether original source files are used in place
// or copied into Timingdex's local cache before media processing. Copy mode is
// intended for mounted NAS/network libraries; it never writes to the source.
type SourceStagingConfig struct {
	Mode string `json:"mode"`
}

type HubTLSConfig struct {
	Mode            string `json:"mode"` // auto, files, off
	CertificateFile string `json:"certificate_file,omitempty"`
	KeyFile         string `json:"key_file,omitempty"`
}

type HubSecurityConfig struct {
	// AdminToken is intentionally environment-only. Generated tokens are kept
	// in DATA_DIR/admin-token; neither form belongs in config.json or SQLite.
	AdminToken                     string `json:"-"`
	AllowWorkerProviderCredentials bool   `json:"allow_worker_provider_credentials"`
	// TrustedReadNetworks are the CIDR ranges allowed to reach the read routes
	// that carry no token of their own (browse, search, thumbnails, proxy).
	// Empty means the built-in private/loopback/Tailnet set. Listing
	// "0.0.0.0/0" and "::/0" disables the guard, which serves the whole library
	// to anyone who can reach the port.
	TrustedReadNetworks []string `json:"trusted_read_networks,omitempty"`
}

// TrustedReadPrefixes parses TrustedReadNetworks. It is validated at load time
// so a typo fails at startup rather than silently widening or narrowing access
// once the server is already serving.
func (c HubSecurityConfig) TrustedReadPrefixes() ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(c.TrustedReadNetworks))
	for _, raw := range c.TrustedReadNetworks {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return nil, fmt.Errorf("hub_security.trusted_read_networks: %q is not a CIDR range: %w", entry, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

type Config struct {
	DataDir       string               `json:"data_dir"`
	CacheDir      string               `json:"cache_dir"`
	DatabasePath  string               `json:"database_path"`
	ListenAddress string               `json:"listen_address"`
	Providers     ProvidersConfig      `json:"providers"`
	Hardware      media.HardwareConfig `json:"hardware"`
	SourceStaging SourceStagingConfig  `json:"source_staging"`
	HubTLS        HubTLSConfig         `json:"hub_tls"`
	HubSecurity   HubSecurityConfig    `json:"hub_security"`
}

func Load() (Config, error) {
	dataDir, err := defaultDataDir()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{DataDir: dataDir, CacheDir: filepath.Join(dataDir, "cache"), DatabasePath: filepath.Join(dataDir, "timingdex.db"), ListenAddress: "127.0.0.1:8787", Hardware: media.HardwareConfig{Mode: "auto", AllowFallback: true, ProxyBitrateKbps: 1800}, SourceStaging: SourceStagingConfig{Mode: "none"}, HubTLS: HubTLSConfig{Mode: "auto"}, Providers: ProvidersConfig{
		ASRPrimary: "stepfun", ASRFallback: "qwen", VisionPrimary: "none", AlignmentPrimary: "external_command",
		TagCuratorPrimary: "openai_chat", TagCuratorFallbackHeuristic: true,
		EmbeddingPrimary: "none", RepurposePrimary: "none", RepurposeFallbackHeuristic: true,
		StepFun:                 ProviderConfig{Enabled: false, BaseURL: "https://api.stepfun.com/step_plan/v1", Path: "audio/asr/sse", APIKeyEnv: "STEP_API_KEY", Model: "stepaudio-2.5-asr", TimeoutSeconds: 300},
		Qwen:                    ProviderConfig{Enabled: false, BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Path: "chat/completions", APIKeyEnv: "DASHSCOPE_API_KEY", Model: "qwen3-asr-flash", TimeoutSeconds: 300},
		Gemini:                  ProviderConfig{Enabled: false, Protocol: "gemini_generate_content", BaseURL: "https://generativelanguage.googleapis.com/v1beta", Path: "", APIKeyEnv: "GEMINI_API_KEY", Model: "gemini-2.5-flash", AuthHeader: "x-goog-api-key", AuthScheme: "raw", TimeoutSeconds: 300},
		QwenVideo:               ProviderConfig{Enabled: false, Protocol: "openai_video", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Path: "chat/completions", APIKeyEnv: "DASHSCOPE_API_KEY", Model: "qwen3.6-flash", TimeoutSeconds: 300},
		VolcVideo:               ProviderConfig{Enabled: false, Protocol: "openai_video", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", Path: "chat/completions", APIKeyEnv: "ARK_API_KEY", Model: "doubao-1.5-vision-pro-250428", TimeoutSeconds: 300},
		LocalVLM:                ProviderConfig{Enabled: false, Protocol: "openai_video", BaseURL: "http://127.0.0.1:1234/v1", Path: "chat/completions", Model: "qwen2.5-vl", TimeoutSeconds: 300},
		TagCurator:              ProviderConfig{Enabled: false, Protocol: "openai_chat", BaseURL: "http://127.0.0.1:1234/v1", Path: "chat/completions", Model: "local-small-instruct", TimeoutSeconds: 120},
		Embedding:               ProviderConfig{Enabled: false, Protocol: "openai_embeddings", BaseURL: "http://127.0.0.1:1234/v1", Path: "embeddings", Model: "text-embedding-model", TimeoutSeconds: 120},
		Repurpose:               ProviderConfig{Enabled: false, Protocol: "openai_chat", BaseURL: "http://127.0.0.1:1234/v1", Path: "chat/completions", Model: "local-small-instruct", TimeoutSeconds: 120},
		VolcAgentPlan:           ProviderConfig{Enabled: false, Protocol: "openai_chat", BaseURL: "https://ark.cn-beijing.volces.com/api/plan/v3", Path: "chat/completions", APIKeyEnv: "ARK_AGENT_PLAN_API_KEY", Model: "doubao-seed-2.0-mini", AuthHeader: "X-Api-Key", AuthScheme: "raw", TimeoutSeconds: 120},
		VolcCodingPlan:          ProviderConfig{Enabled: false, Protocol: "openai_chat", BaseURL: "https://ark.cn-beijing.volces.com/api/coding/v3", Path: "chat/completions", APIKeyEnv: "ARK_CODING_PLAN_API_KEY", Model: "ark-code-latest", AuthHeader: "X-Api-Key", AuthScheme: "raw", TimeoutSeconds: 120},
		VolcAgentPlanEmbedding:  ProviderConfig{Enabled: false, Protocol: "openai_embeddings", BaseURL: "https://ark.cn-beijing.volces.com/api/plan/v3", Path: "embeddings", APIKeyEnv: "ARK_AGENT_PLAN_API_KEY", Model: "doubao-embedding-vision-251215", AuthHeader: "X-Api-Key", AuthScheme: "raw", TimeoutSeconds: 120},
		VolcCodingPlanEmbedding: ProviderConfig{Enabled: false, Protocol: "openai_embeddings", BaseURL: "https://ark.cn-beijing.volces.com/api/coding/v3", Path: "embeddings", APIKeyEnv: "ARK_CODING_PLAN_API_KEY", Model: "doubao-embedding-vision-251215", AuthHeader: "X-Api-Key", AuthScheme: "raw", TimeoutSeconds: 120},
		VolcASR:                 VolcASRConfig{Enabled: false, URL: "wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_nostream", APIKeyEnv: "ARK_AGENT_PLAN_API_KEY", ResourceID: "volc.seedasr.sauc.duration", RequestModel: "bigmodel", Model: "doubao-seed-asr-2.0", UID: "timingdex", TimeoutSeconds: 300},
		Alignment:               AlignmentConfig{Enabled: false, Command: "timingdex-align", Model: "qwen3-forced-aligner"},
	}}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return Config{}, fmt.Errorf("create data directories: %w", err)
	}
	configPath := filepath.Join(dataDir, "config.json")
	raw, err := os.ReadFile(configPath)
	if err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", configPath, err)
		}
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if v := strings.TrimSpace(os.Getenv("TIMINGDEX_SOURCE_STAGING_MODE")); v != "" {
		cfg.SourceStaging.Mode = v
	}
	if v := strings.TrimSpace(os.Getenv("TIMINGDEX_LISTEN_ADDRESS")); v != "" {
		cfg.ListenAddress = v
	}
	if v := strings.TrimSpace(os.Getenv("TIMINGDEX_TLS_MODE")); v != "" {
		cfg.HubTLS.Mode = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("TIMINGDEX_TLS_CERT_FILE")); v != "" {
		cfg.HubTLS.CertificateFile = v
	}
	if v := strings.TrimSpace(os.Getenv("TIMINGDEX_TLS_KEY_FILE")); v != "" {
		cfg.HubTLS.KeyFile = v
	}
	if v := strings.TrimSpace(os.Getenv("TIMINGDEX_HUB_ADMIN_TOKEN")); v != "" {
		cfg.HubSecurity.AdminToken = v
	}
	if v := strings.TrimSpace(os.Getenv("TIMINGDEX_ALLOW_WORKER_PROVIDER_CREDENTIALS")); v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			cfg.HubSecurity.AllowWorkerProviderCredentials = enabled
		}
	}
	if v := strings.TrimSpace(os.Getenv("TIMINGDEX_TRUSTED_READ_NETWORKS")); v != "" {
		cfg.HubSecurity.TrustedReadNetworks = strings.Split(v, ",")
	}
	if _, err := cfg.HubSecurity.TrustedReadPrefixes(); err != nil {
		return Config{}, err
	}
	resolve := func(p *ProviderConfig) {
		if p.APIKey == "" && p.APIKeyEnv != "" {
			p.APIKey = os.Getenv(p.APIKeyEnv)
		}
		if v := os.Getenv("TIMINGDEX_" + strings.ToUpper(p.APIKeyEnv) + "_BASE_URL"); v != "" {
			p.BaseURL = v
		}
		if v := os.Getenv("TIMINGDEX_PROVIDER_TIMEOUT"); v != "" {
			if n, e := strconv.Atoi(v); e == nil {
				p.TimeoutSeconds = n
			}
		}
	}
	resolve(&cfg.Providers.StepFun)
	resolve(&cfg.Providers.Qwen)
	resolve(&cfg.Providers.Gemini)
	resolve(&cfg.Providers.QwenVideo)
	resolve(&cfg.Providers.VolcVideo)
	resolve(&cfg.Providers.LocalVLM)
	resolve(&cfg.Providers.TagCurator)
	resolve(&cfg.Providers.Embedding)
	resolve(&cfg.Providers.Repurpose)
	resolve(&cfg.Providers.VolcAgentPlan)
	resolve(&cfg.Providers.VolcCodingPlan)
	resolve(&cfg.Providers.VolcAgentPlanEmbedding)
	resolve(&cfg.Providers.VolcCodingPlanEmbedding)
	if cfg.Providers.VolcASR.APIKey == "" && cfg.Providers.VolcASR.APIKeyEnv != "" {
		cfg.Providers.VolcASR.APIKey = os.Getenv(cfg.Providers.VolcASR.APIKeyEnv)
	}
	if v := os.Getenv("TIMINGDEX_VOLC_ASR_URL"); v != "" {
		cfg.Providers.VolcASR.URL = v
	}
	if v := os.Getenv("TIMINGDEX_PROVIDER_TIMEOUT"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			cfg.Providers.VolcASR.TimeoutSeconds = n
		}
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return Config{}, fmt.Errorf("create cache directory: %w", err)
	}
	return cfg, nil
}
func defaultDataDir() (string, error) {
	if value := os.Getenv("TIMINGDEX_DATA_DIR"); value != "" {
		return filepath.Abs(value)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".timingdex"), nil
}
