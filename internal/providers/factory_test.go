package providers

import (
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/config"
	videoproviders "github.com/evjohn-icu/nexusgate/internal/providers/video"
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

func TestVideoFactoryAcceptsMultiframeProtocol(t *testing.T) {
	cfg := config.ProvidersConfig{
		LocalVLM: config.ProviderConfig{Enabled: true, Protocol: ProtocolOpenAIMultiframe, BaseURL: "http://127.0.0.1:1234/v1", Model: "Qwen3-VL-4B-Instruct"},
	}
	provider, err := NewVideoUnderstandingProvider("local_vlm", nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "local_vlm" || provider.Model() != "Qwen3-VL-4B-Instruct" {
		t.Fatalf("identity: %s/%s", provider.Name(), provider.Model())
	}
	if analyzer := provider.(interface {
		MultiframeAnalyzer() videoproviders.MultiframeShotAnalyzer
	}).MultiframeAnalyzer(); analyzer == nil {
		t.Fatal("multiframe protocol did not surface a MultiframeShotAnalyzer through the router")
	}
}

func TestVideoFactoryRejectsUnknownProtocol(t *testing.T) {
	cfg := config.ProvidersConfig{
		LocalVLM: config.ProviderConfig{Enabled: true, Protocol: "openai_frames", BaseURL: "http://127.0.0.1:1234/v1", Model: "m"},
	}
	if _, err := NewVideoUnderstandingProvider("local_vlm", nil, cfg); err == nil {
		t.Fatal("expected rejection of an unknown protocol")
	}
}

func TestNewShotDetectorModes(t *testing.T) {
	cfg := config.ProvidersConfig{ShotDetection: config.ShotDetectionConfig{Enabled: true, Mode: config.ShotDetectionModeFFmpegScene}}
	detector, err := NewShotDetector(cfg)
	if err != nil || detector == nil {
		t.Fatalf("ffmpeg_scene detector: %v err=%v", detector, err)
	}
	if detector.Name() == "" {
		t.Fatal("ffmpeg_scene detector has empty identity")
	}
	cfg.ShotDetection = config.ShotDetectionConfig{Enabled: true, Mode: config.ShotDetectionModeExternalCommand, Command: "pyscenedetect.sh"}
	detector, err = NewShotDetector(cfg)
	if err != nil || detector == nil {
		t.Fatalf("external_command detector: %v err=%v", detector, err)
	}
	cfg.ShotDetection = config.ShotDetectionConfig{Enabled: true, Mode: config.ShotDetectionModeExternalCommand}
	if _, err := NewShotDetector(cfg); err == nil {
		t.Fatal("external_command without a command must fail")
	}
	cfg.ShotDetection = config.ShotDetectionConfig{Enabled: true, Mode: "bogus"}
	if _, err := NewShotDetector(cfg); err == nil {
		t.Fatal("unknown mode must fail")
	}
	cfg.ShotDetection = config.ShotDetectionConfig{Enabled: false}
	detector, err = NewShotDetector(cfg)
	if err != nil || detector != nil {
		t.Fatalf("disabled detection must be nil: %v err=%v", detector, err)
	}
}

func TestValidateProviderConfigAcceptsDefaultAndKeyedConfigs(t *testing.T) {
	// The shipped default selects stepfun/qwen but leaves them disabled: that
	// must not fail, or every fresh install would refuse to start.
	if err := ValidateProviderConfig(config.ProvidersConfig{ASRPrimary: "stepfun", ASRFallback: "qwen"}); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
	// An enabled keyed provider with a resolved key passes.
	if err := ValidateProviderConfig(config.ProvidersConfig{
		ASRPrimary: "stepfun",
		StepFun:    config.ProviderConfig{Enabled: true, APIKeyEnv: "STEP_API_KEY", APIKey: "sk-test"},
	}); err != nil {
		t.Fatalf("keyed config rejected: %v", err)
	}
	// Keyless local endpoints (LM Studio, a relay) pass without any key.
	if err := ValidateProviderConfig(config.ProvidersConfig{
		VisionPrimary: "local_vlm",
		LocalVLM:      config.ProviderConfig{Enabled: true, BaseURL: "http://127.0.0.1:1234/v1", Protocol: ProtocolOpenAIMultiframe},
	}); err != nil {
		t.Fatalf("keyless local_vlm rejected: %v", err)
	}
	// Heuristic repurpose needs no provider.
	if err := ValidateProviderConfig(config.ProvidersConfig{RepurposePrimary: "heuristic"}); err != nil {
		t.Fatalf("heuristic repurpose rejected: %v", err)
	}
}

func TestValidateProviderConfigRejectsEnabledProviderWithUnsetKey(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.ProvidersConfig
		want string
	}{
		{"asr", config.ProvidersConfig{
			ASRPrimary: "stepfun",
			StepFun:    config.ProviderConfig{Enabled: true, APIKeyEnv: "STEP_API_KEY"},
		}, "STEP_API_KEY"},
		{"vision", config.ProvidersConfig{
			VisionPrimary: "qwen_video",
			QwenVideo:     config.ProviderConfig{Enabled: true, APIKeyEnv: "DASHSCOPE_API_KEY"},
		}, "DASHSCOPE_API_KEY"},
		{"repurpose", config.ProvidersConfig{
			RepurposePrimary: "openai_chat",
			Repurpose:        config.ProviderConfig{Enabled: true, APIKeyEnv: "ARK_API_KEY"},
		}, "ARK_API_KEY"},
	}
	for _, tc := range cases {
		err := ValidateProviderConfig(tc.cfg)
		if err == nil {
			t.Fatalf("%s: expected failure for enabled provider with unset key", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %q does not name %q", tc.name, err, tc.want)
		}
	}
}

func TestValidateProviderConfigRejectsUnknownProvider(t *testing.T) {
	err := ValidateProviderConfig(config.ProvidersConfig{ASRPrimary: "bogus", ASRFallback: "qwen"})
	if err == nil || !strings.Contains(err.Error(), `asr provider "bogus"`) {
		t.Fatalf("unknown asr provider: err=%v", err)
	}
}

func TestSummarizeProvidersReportsEnabledBlocksWithoutSecrets(t *testing.T) {
	cfg := config.ProvidersConfig{
		ASRPrimary:    "stepfun",
		StepFun:       config.ProviderConfig{Enabled: true, Model: "stepaudio-2.5-asr", APIKey: "sk-secret"},
		Qwen:          config.ProviderConfig{Enabled: false, Model: "qwen3-asr-flash"},
		VisionPrimary: "local_vlm",
		LocalVLM:      config.ProviderConfig{Enabled: true, Protocol: ProtocolOpenAIMultiframe, Model: "doubao-seed-2.0-lite"},
	}
	s := SummarizeProviders(cfg)
	if s.ASR.Primary != "stepfun" {
		t.Fatalf("asr primary=%q", s.ASR.Primary)
	}
	if len(s.ASR.Configured) != 1 || s.ASR.Configured[0].Name != "stepfun" || s.ASR.Configured[0].Model != "stepaudio-2.5-asr" {
		t.Fatalf("asr configured=%+v", s.ASR.Configured)
	}
	for _, p := range append(s.ASR.Configured, s.Vision.Configured...) {
		if strings.Contains(p.Name, "secret") || strings.Contains(p.Model, "sk-") {
			t.Fatalf("provider info leaked a secret: %+v", p)
		}
	}
	if len(s.Vision.Configured) != 1 || s.Vision.Configured[0].Protocol != ProtocolOpenAIMultiframe {
		t.Fatalf("vision configured=%+v", s.Vision.Configured)
	}
}
