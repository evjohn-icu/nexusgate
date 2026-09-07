package config

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/media"
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
	//
	// Their defaults authenticate with `Authorization: Bearer <key>`, verified
	// against an Agent Plan account that answers the previously shipped
	// `X-Api-Key`/raw pairing with 401. Header and scheme are stated
	// explicitly rather than left blank because blank has two meanings here:
	// common.Endpoint.NewRequest treats an empty scheme as Bearer, while the
	// Worker JSON proxy sends the key unprefixed. Only the explicit form is
	// correct on both request paths.
	VolcAgentPlan           ProviderConfig      `json:"volc_agent_plan"`
	VolcCodingPlan          ProviderConfig      `json:"volc_coding_plan"`
	VolcAgentPlanEmbedding  ProviderConfig      `json:"volc_agent_plan_embedding"`
	VolcCodingPlanEmbedding ProviderConfig      `json:"volc_coding_plan_embedding"`
	VolcASR                 VolcASRConfig       `json:"volc_asr"`
	Alignment               AlignmentConfig     `json:"alignment"`
	ShotDetection           ShotDetectionConfig `json:"shot_detection"`
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

// ShotDetectionConfig configures deterministic shot-boundary detection for
// the multiframe analysis path. ShotDetectionModeExternalCommand runs an
// external binary through the aligner-style stdin/stdout JSON contract;
// ShotDetectionModeFFmpegScene uses ffmpeg's built-in scene filter and needs
// nothing beyond the ffmpeg binary the pipeline already requires. When
// detection is disabled entirely, the multiframe path falls back to a VLM
// window analysis for its boundaries (two-pass refinement).
type ShotDetectionConfig struct {
	Enabled bool     `json:"enabled"`
	Mode    string   `json:"mode,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// SceneThreshold is the ffmpeg scene score at which a frame starts a new
	// shot (0.3 by default); it is part of the detector identity, so tuning it
	// re-keys analysis.
	SceneThreshold float64 `json:"scene_threshold,omitempty"`
}

const (
	ShotDetectionModeExternalCommand = "external_command"
	ShotDetectionModeFFmpegScene     = "ffmpeg_scene"
)

// SourceStagingConfig controls whether original source files are used in place
// or copied into NexusGate's local cache before media processing. Copy mode is
// intended for mounted NAS/network libraries; it never writes to the source.
type SourceStagingConfig struct {
	Mode string `json:"mode"`
}

// LibrarySupervisorConfig turns `nexusgate serve` into an unattended library:
// the Hub rescans every root on a timer and drains the queue itself, instead of
// waiting for someone to run `root scan` and `pipeline run`.
//
// It lives in config.json rather than in the settings table that holds
// PipelineThrottle, for two reasons. It decides whether a process starts a
// background loop at all, so it is read once at startup like listen_address and
// hub_tls — unlike the throttle, which the pipeline re-reads before every job
// precisely so it can be tightened mid-scan. And turning it on commits the Hub
// to spending Provider quota with nobody watching, which should take the same
// kind of deliberate act as opening a port: an edit on the Hub box, not a
// toggle in a browser page.
//
// Disabled is the only safe default. An install that upgrades into this must
// behave exactly as it did before until someone opts in.
type LibrarySupervisorConfig struct {
	Enabled bool `json:"enabled"`
	// ScanIntervalMinutes is the polling period. Polling, not fsnotify: the
	// libraries this is for live on SMB/NFS shares where inotify is unreliable
	// or absent, and the Docker media bind uses rslave propagation, so a share
	// can appear or disappear underneath the bind while the Hub is running. A
	// dropped event there means footage that is silently never indexed, which
	// is worse than a walk that costs a few stat calls every quarter hour.
	ScanIntervalMinutes int `json:"scan_interval_minutes"`
}

// PipelineConfig carries how a pipeline pass treats work that cannot run yet.
// It is read once at startup rather than re-read per job like the throttle,
// because the deferral only sets a wall-clock park time — there is nothing to
// tighten mid-scan.
type PipelineConfig struct {
	// ProviderRouteDeferralMinutes is how long a job waits after every
	// provider key on its route has failed at once. On the recommended plan
	// that is a spent monthly quota, which clears on its own, so the wait
	// exists to stop the queue from re-asking a dead route in seconds; an
	// operator on a different plan shortens it here instead of rebuilding.
	// Zero or negative is treated as a typo and floored at the point of use,
	// where the busy-retry loop it would cause is actually prevented.
	ProviderRouteDeferralMinutes int `json:"provider_route_deferral_minutes"`
	// MinimumFreeSpaceBytes is the floor of free bytes the pipeline requires
	// on the cache volume before it runs a heavy stage (derive/transcribe/
	// analyze). A full disk makes ffmpeg fail mid-encode with an ordinary
	// retryable error — burning the job's attempts — and cache/sources
	// staging copies can silently fill the volume, so below the floor the job
	// is deferred (reason disk_space_low) instead of running, and no attempt
	// is spent. Zero (the default) disables the preflight entirely: a fresh
	// install must not suddenly start deferring jobs it ran fine yesterday.
	MinimumFreeSpaceBytes int64 `json:"minimum_free_space_bytes"`
}

// defaultProviderRouteDeferralMinutes is the five-hour wait, in the units the
// field above is read in. It matches the failure the deferral exists for: the
// recommended plan's quota is monthly and hard, so a handful of probe calls a
// day still recovers on its own once the account is topped up or the month
// rolls over.
const defaultProviderRouteDeferralMinutes = 5 * 60

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
	// AdminAuth controls whether administrative and mutating routes require
	// the Hub administrator credential. "required" always demands it. The
	// default "trusted_network" waives it for peers inside AdminAuthNetworks,
	// which makes a LAN-only Hub usable without a password while still refusing
	// the internet. "off" never demands it -- every caller that can reach the
	// port is an administrator, including for provider-key writes and paid
	// pipeline runs.
	//
	// The waiver is decided from RemoteAddr alone, exactly like the
	// trusted-read guard, and inherits that guard's documented blind spot:
	// behind a reverse proxy or a published Docker port every peer looks
	// RFC1918. See network_guard.go and the container guard in cmd/nexusgate.
	AdminAuth string `json:"admin_auth,omitempty"`
	// AdminAuthNetworks are the CIDR ranges whose peers skip the admin
	// credential when AdminAuth is "trusted_network". Empty means the same
	// built-in private/loopback/Tailnet set the read guard uses.
	AdminAuthNetworks []string `json:"admin_auth_networks,omitempty"`
}

// AdminAuthMode returns the normalized admin-auth mode. An empty value means
// the documented default trusted_network, so a zero-value HubSecurityConfig
// (for example the browser fixture, which builds a config.Config directly
// without Load()) still answers with the default rather than an empty string.
func (c HubSecurityConfig) AdminAuthMode() string {
	if strings.TrimSpace(c.AdminAuth) == "" {
		return "trusted_network"
	}
	return c.AdminAuth
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

// AdminAuthPrefixes parses AdminAuthNetworks. It is validated at load time so
// a typo fails at startup rather than silently widening or narrowing access
// once the server is already serving.
func (c HubSecurityConfig) AdminAuthPrefixes() ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(c.AdminAuthNetworks))
	for _, raw := range c.AdminAuthNetworks {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return nil, fmt.Errorf("hub_security.admin_auth_networks: %q is not a CIDR range: %w", entry, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

// ValidateContainerAdminAuth fails closed when the Hub runs inside a container
// with the default trusted_network mode and no explicit AdminAuthNetworks. A
// published Docker port makes every peer appear to come from the bridge gateway
// (an RFC1918 address), so the network guard cannot tell a LAN caller from the
// internet; administrative writes would then be open to anyone who can reach
// the port. Requiring an explicit list forces a deliberate answer.
func ValidateContainerAdminAuth(isContainer bool, cfg Config) error {
	if isContainer && cfg.HubSecurity.AdminAuth == "trusted_network" && len(cfg.HubSecurity.AdminAuthNetworks) == 0 {
		return fmt.Errorf("在容器里运行时，hub_security.admin_auth: \"trusted_network\" 必须同时显式配置 hub_security.admin_auth_networks。容器发布端口后，所有请求都来自网桥网关（一个 RFC1918 地址），Hub 无法区分局域网和公网调用者，管理写入会对端口可达的任何人开放。要么列出真实的内网网段，要么改用 admin_auth: \"required\"。")
	}
	return nil
}

type Config struct {
	DataDir           string                  `json:"data_dir"`
	CacheDir          string                  `json:"cache_dir"`
	DatabasePath      string                  `json:"database_path"`
	ListenAddress     string                  `json:"listen_address"`
	HostPlatform      string                  `json:"host_platform"`
	Providers         ProvidersConfig         `json:"providers"`
	Hardware          media.HardwareConfig    `json:"hardware"`
	SourceStaging     SourceStagingConfig     `json:"source_staging"`
	HubTLS            HubTLSConfig            `json:"hub_tls"`
	HubSecurity       HubSecurityConfig       `json:"hub_security"`
	LibrarySupervisor LibrarySupervisorConfig `json:"library_supervisor"`
	Pipeline          PipelineConfig          `json:"pipeline"`
}

func Load() (Config, error) {
	dataDir, err := defaultDataDir()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{DataDir: dataDir, CacheDir: filepath.Join(dataDir, "cache"), DatabasePath: filepath.Join(dataDir, "nexusgate.db"), ListenAddress: "127.0.0.1:8787", Hardware: media.HardwareConfig{Mode: "auto", AllowFallback: true, ProxyBitrateKbps: 1800}, SourceStaging: SourceStagingConfig{Mode: "none"}, HubTLS: HubTLSConfig{Mode: "auto"}, LibrarySupervisor: LibrarySupervisorConfig{Enabled: false, ScanIntervalMinutes: 15}, Pipeline: PipelineConfig{ProviderRouteDeferralMinutes: defaultProviderRouteDeferralMinutes}, Providers: ProvidersConfig{
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
		VolcAgentPlan:           ProviderConfig{Enabled: false, Protocol: "openai_chat", BaseURL: "https://ark.cn-beijing.volces.com/api/plan/v3", Path: "chat/completions", APIKeyEnv: "ARK_AGENT_PLAN_API_KEY", Model: "doubao-seed-2.0-mini", AuthHeader: "Authorization", AuthScheme: "Bearer", TimeoutSeconds: 120},
		VolcCodingPlan:          ProviderConfig{Enabled: false, Protocol: "openai_chat", BaseURL: "https://ark.cn-beijing.volces.com/api/coding/v3", Path: "chat/completions", APIKeyEnv: "ARK_CODING_PLAN_API_KEY", Model: "ark-code-latest", AuthHeader: "Authorization", AuthScheme: "Bearer", TimeoutSeconds: 120},
		VolcAgentPlanEmbedding:  ProviderConfig{Enabled: false, Protocol: "openai_embeddings", BaseURL: "https://ark.cn-beijing.volces.com/api/plan/v3", Path: "embeddings", APIKeyEnv: "ARK_AGENT_PLAN_API_KEY", Model: "doubao-embedding-vision-251215", AuthHeader: "Authorization", AuthScheme: "Bearer", TimeoutSeconds: 120},
		VolcCodingPlanEmbedding: ProviderConfig{Enabled: false, Protocol: "openai_embeddings", BaseURL: "https://ark.cn-beijing.volces.com/api/coding/v3", Path: "embeddings", APIKeyEnv: "ARK_CODING_PLAN_API_KEY", Model: "doubao-embedding-vision-251215", AuthHeader: "Authorization", AuthScheme: "Bearer", TimeoutSeconds: 120},
		VolcASR:                 VolcASRConfig{Enabled: false, URL: "wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_nostream", APIKeyEnv: "ARK_AGENT_PLAN_API_KEY", ResourceID: "volc.seedasr.sauc.duration", RequestModel: "bigmodel", Model: "doubao-seed-asr-2.0", UID: "nexusgate", TimeoutSeconds: 300},
		Alignment:               AlignmentConfig{Enabled: false, Command: "nexusgate-align", Model: "qwen3-forced-aligner"},
		ShotDetection:           ShotDetectionConfig{Enabled: false, Mode: ShotDetectionModeExternalCommand},
	}}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return Config{}, fmt.Errorf("create data directories: %w", err)
	}
	configPath := filepath.Join(dataDir, "config.json")
	raw, err := os.ReadFile(configPath)
	var explicit map[string]json.RawMessage
	if err == nil {
		if err := json.Unmarshal(raw, &explicit); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", configPath, err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", configPath, err)
		}
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_SOURCE_STAGING_MODE")); v != "" {
		cfg.SourceStaging.Mode = v
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_LISTEN_ADDRESS")); v != "" {
		cfg.ListenAddress = v
	}
	// This is a deployment hint rather than a fact LocalHost can prove: the
	// process can detect its container, but only the operator knows which host
	// platform owns remote-share mounts outside it.
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_HOST_PLATFORM")); v != "" {
		cfg.HostPlatform = v
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_TLS_MODE")); v != "" {
		cfg.HubTLS.Mode = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_TLS_CERT_FILE")); v != "" {
		cfg.HubTLS.CertificateFile = v
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_TLS_KEY_FILE")); v != "" {
		cfg.HubTLS.KeyFile = v
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_HUB_ADMIN_TOKEN")); v != "" {
		cfg.HubSecurity.AdminToken = v
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_HUB_ADMIN_AUTH")); v != "" {
		cfg.HubSecurity.AdminAuth = v
	}
	cfg.HubSecurity.AdminAuth = strings.ToLower(strings.TrimSpace(cfg.HubSecurity.AdminAuth))
	if cfg.HubSecurity.AdminAuth == "" {
		cfg.HubSecurity.AdminAuth = "trusted_network"
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_HUB_ADMIN_AUTH_NETWORKS")); v != "" {
		cfg.HubSecurity.AdminAuthNetworks = strings.Split(v, ",")
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_ALLOW_WORKER_PROVIDER_CREDENTIALS")); v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			cfg.HubSecurity.AllowWorkerProviderCredentials = enabled
		}
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_LIBRARY_SUPERVISOR")); v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			cfg.LibrarySupervisor.Enabled = enabled
		}
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_LIBRARY_SUPERVISOR_INTERVAL_MINUTES")); v != "" {
		if minutes, err := strconv.Atoi(v); err == nil {
			cfg.LibrarySupervisor.ScanIntervalMinutes = minutes
		}
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_PIPELINE_PROVIDER_ROUTE_DEFERRAL_MINUTES")); v != "" {
		if minutes, err := strconv.Atoi(v); err == nil {
			cfg.Pipeline.ProviderRouteDeferralMinutes = minutes
		}
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_MINIMUM_FREE_SPACE_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Pipeline.MinimumFreeSpaceBytes = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("NEXUSGATE_TRUSTED_READ_NETWORKS")); v != "" {
		cfg.HubSecurity.TrustedReadNetworks = strings.Split(v, ",")
	}
	if _, err := cfg.HubSecurity.TrustedReadPrefixes(); err != nil {
		return Config{}, err
	}
	if _, err := cfg.HubSecurity.AdminAuthPrefixes(); err != nil {
		return Config{}, err
	}
	resolve := func(p *ProviderConfig) {
		if p.APIKey == "" && p.APIKeyEnv != "" {
			p.APIKey = os.Getenv(p.APIKeyEnv)
		}
		if v := os.Getenv("NEXUSGATE_" + strings.ToUpper(p.APIKeyEnv) + "_BASE_URL"); v != "" {
			p.BaseURL = v
		}
		if v := os.Getenv("NEXUSGATE_PROVIDER_TIMEOUT"); v != "" {
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
	if v := os.Getenv("NEXUSGATE_VOLC_ASR_URL"); v != "" {
		cfg.Providers.VolcASR.URL = v
	}
	if v := os.Getenv("NEXUSGATE_PROVIDER_TIMEOUT"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			cfg.Providers.VolcASR.TimeoutSeconds = n
		}
	}
	if err := validate(cfg, explicit); err != nil {
		return Config{}, err
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return Config{}, fmt.Errorf("create cache directory: %w", err)
	}
	return cfg, nil
}

func validate(cfg Config, explicit map[string]json.RawMessage) error {
	switch strings.ToLower(strings.TrimSpace(cfg.HubTLS.Mode)) {
	case "auto", "files", "off":
	default:
		return fmt.Errorf("hub_tls.mode: unsupported value %q (want auto, files, or off)", cfg.HubTLS.Mode)
	}
	if cfg.LibrarySupervisor.Enabled && cfg.LibrarySupervisor.ScanIntervalMinutes <= 0 {
		return fmt.Errorf("library_supervisor.scan_interval_minutes: must be greater than zero when supervisor is enabled")
	}
	if cfg.Pipeline.ProviderRouteDeferralMinutes <= 0 && explicitField(explicit, "pipeline", "provider_route_deferral_minutes") {
		return fmt.Errorf("pipeline.provider_route_deferral_minutes: explicit value must be greater than zero")
	}
	if cfg.Pipeline.MinimumFreeSpaceBytes < 0 {
		return fmt.Errorf("pipeline.minimum_free_space_bytes: must not be negative")
	}
	switch strings.ToLower(strings.TrimSpace(cfg.HubSecurity.AdminAuth)) {
	case "required", "trusted_network", "off":
	default:
		return fmt.Errorf("hub_security.admin_auth: unsupported value %q (want required, trusted_network, or off)", cfg.HubSecurity.AdminAuth)
	}
	return nil
}

func explicitField(tree map[string]json.RawMessage, parent, field string) bool {
	raw, ok := tree[parent]
	if !ok {
		return false
	}
	var child map[string]json.RawMessage
	if err := json.Unmarshal(raw, &child); err != nil {
		return false
	}
	_, ok = child[field]
	return ok
}
func defaultDataDir() (string, error) {
	if value := os.Getenv("NEXUSGATE_DATA_DIR"); value != "" {
		return filepath.Abs(value)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".nexusgate"), nil
}
