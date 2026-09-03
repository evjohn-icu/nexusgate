package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSparseConfigPreservesDefaults(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("NEXUSSLATE_DATA_DIR", dataDir)
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"providers":{"stepfun":{"enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LibrarySupervisor.ScanIntervalMinutes != 15 || cfg.HubTLS.Mode != "auto" || cfg.Pipeline.ProviderRouteDeferralMinutes != defaultProviderRouteDeferralMinutes {
		t.Fatalf("sparse config lost defaults: supervisor=%d tls=%q deferral=%d", cfg.LibrarySupervisor.ScanIntervalMinutes, cfg.HubTLS.Mode, cfg.Pipeline.ProviderRouteDeferralMinutes)
	}
}

func TestLoadRejectsExplicitZeroSupervisorIntervalWhenEnabled(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("NEXUSSLATE_DATA_DIR", dataDir)
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"library_supervisor":{"enabled":true,"scan_interval_minutes":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("explicit zero supervisor interval should be rejected")
	}
}

func TestLoadRejectsInvalidTLSMode(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("NEXUSSLATE_DATA_DIR", dataDir)
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"hub_tls":{"mode":""}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("explicit empty TLS mode should be rejected")
	}
}

// The Ark plan endpoints authenticate with a bearer token. Shipping any other
// header/scheme pairing here is not a cosmetic default: the provider answers
// 401, and because the failure happens against the operator's own key it reads
// as a bad key rather than a bad default. Both fields are asserted explicitly
// because leaving them blank is only correct on the in-process request path —
// the Worker JSON proxy sends an unprefixed key when the scheme is empty.
func TestVolcPlanProvidersDefaultToBearerAuth(t *testing.T) {
	t.Setenv("NEXUSSLATE_DATA_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for name, provider := range map[string]ProviderConfig{
		"volc_agent_plan":            cfg.Providers.VolcAgentPlan,
		"volc_coding_plan":           cfg.Providers.VolcCodingPlan,
		"volc_agent_plan_embedding":  cfg.Providers.VolcAgentPlanEmbedding,
		"volc_coding_plan_embedding": cfg.Providers.VolcCodingPlanEmbedding,
	} {
		if provider.AuthHeader != "Authorization" || provider.AuthScheme != "Bearer" {
			t.Fatalf("%s auth_header=%q auth_scheme=%q want Authorization/Bearer", name, provider.AuthHeader, provider.AuthScheme)
		}
	}
}

func TestLoadReadsSourceStagingModeFromEnvironment(t *testing.T) {
	t.Setenv("NEXUSSLATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSSLATE_SOURCE_STAGING_MODE", "copy")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SourceStaging.Mode != "copy" {
		t.Fatalf("source staging mode=%q want copy", cfg.SourceStaging.Mode)
	}
}

// A malformed range must stop startup. Accepting it and skipping the entry
// would either lock the operator out of their own library or, worse, silently
// widen what the guard admits.
func TestLoadRejectsMalformedTrustedReadNetwork(t *testing.T) {
	t.Setenv("NEXUSSLATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSSLATE_TRUSTED_READ_NETWORKS", "192.168.1.0/16, not-a-cidr")
	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject a malformed CIDR range")
	}
}

func TestTrustedReadPrefixesParsesConfiguredRanges(t *testing.T) {
	t.Setenv("NEXUSSLATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSSLATE_TRUSTED_READ_NETWORKS", "10.9.0.0/16, fd00::/8")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	prefixes, err := cfg.HubSecurity.TrustedReadPrefixes()
	if err != nil || len(prefixes) != 2 {
		t.Fatalf("prefixes=%v err=%v", prefixes, err)
	}
	if prefixes[0].String() != "10.9.0.0/16" || prefixes[1].String() != "fd00::/8" {
		t.Fatalf("prefixes=%v", prefixes)
	}
}
