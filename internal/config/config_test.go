package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSparseConfigPreservesDefaults(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("NEXUSGATE_DATA_DIR", dataDir)
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
	t.Setenv("NEXUSGATE_DATA_DIR", dataDir)
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"library_supervisor":{"enabled":true,"scan_interval_minutes":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("explicit zero supervisor interval should be rejected")
	}
}

func TestLoadRejectsInvalidTLSMode(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("NEXUSGATE_DATA_DIR", dataDir)
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
	t.Setenv("NEXUSGATE_DATA_DIR", t.TempDir())
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
	t.Setenv("NEXUSGATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSGATE_SOURCE_STAGING_MODE", "copy")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SourceStaging.Mode != "copy" {
		t.Fatalf("source staging mode=%q want copy", cfg.SourceStaging.Mode)
	}
}

func TestLoadReadsSourceStagingMaxBytesFromConfigFile(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("NEXUSGATE_DATA_DIR", dataDir)
	t.Setenv("NEXUSGATE_SOURCE_STAGING_MODE", "")
	t.Setenv("NEXUSGATE_SOURCE_STAGING_MAX_BYTES", "")
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"source_staging":{"mode":"copy","max_bytes":123456}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SourceStaging.MaxBytes != 123456 {
		t.Fatalf("source staging max bytes=%d want 123456", cfg.SourceStaging.MaxBytes)
	}
}

func TestLoadSourceStagingMaxBytesEnvironmentOverridesConfigFile(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("NEXUSGATE_DATA_DIR", dataDir)
	t.Setenv("NEXUSGATE_SOURCE_STAGING_MODE", "")
	t.Setenv("NEXUSGATE_SOURCE_STAGING_MAX_BYTES", "654321")
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"source_staging":{"mode":"copy","max_bytes":123456}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SourceStaging.MaxBytes != 654321 {
		t.Fatalf("source staging max bytes=%d want environment value 654321", cfg.SourceStaging.MaxBytes)
	}
}

func TestLoadRejectsNonNumericSourceStagingMaxBytesEnvironment(t *testing.T) {
	t.Setenv("NEXUSGATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSGATE_SOURCE_STAGING_MAX_BYTES", "not-a-number")
	_, err := Load()
	if err == nil {
		t.Fatal("expected non-numeric source staging max bytes to be rejected")
	}
	if !strings.Contains(err.Error(), "NEXUSGATE_SOURCE_STAGING_MAX_BYTES") {
		t.Fatalf("error=%q does not name NEXUSGATE_SOURCE_STAGING_MAX_BYTES", err)
	}
}

func TestLoadRejectsNegativeSourceStagingMaxBytes(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("NEXUSGATE_DATA_DIR", dataDir)
	t.Setenv("NEXUSGATE_SOURCE_STAGING_MODE", "")
	t.Setenv("NEXUSGATE_SOURCE_STAGING_MAX_BYTES", "")
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"source_staging":{"mode":"copy","max_bytes":-1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("expected negative source staging max bytes to be rejected")
	}
	if !strings.Contains(err.Error(), "source_staging.max_bytes") {
		t.Fatalf("error=%q does not name source_staging.max_bytes", err)
	}
}

func TestLoadAcceptsExplicitZeroSourceStagingMaxBytes(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("NEXUSGATE_DATA_DIR", dataDir)
	t.Setenv("NEXUSGATE_SOURCE_STAGING_MODE", "")
	t.Setenv("NEXUSGATE_SOURCE_STAGING_MAX_BYTES", "")
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"source_staging":{"mode":"copy","max_bytes":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SourceStaging.MaxBytes != 0 {
		t.Fatalf("source staging max bytes=%d; explicit 0 means unbounded and must not be turned into a default", cfg.SourceStaging.MaxBytes)
	}
}

func TestLoadMissingSourceStagingMaxBytesDefaultsToZero(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("NEXUSGATE_DATA_DIR", dataDir)
	t.Setenv("NEXUSGATE_SOURCE_STAGING_MODE", "")
	t.Setenv("NEXUSGATE_SOURCE_STAGING_MAX_BYTES", "")
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"source_staging":{"mode":"copy"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SourceStaging.Mode != "copy" {
		t.Fatalf("source staging mode=%q want copy", cfg.SourceStaging.Mode)
	}
	if cfg.SourceStaging.MaxBytes != 0 {
		t.Fatalf("missing source staging max bytes=%d want 0", cfg.SourceStaging.MaxBytes)
	}
}

// A malformed range must stop startup. Accepting it and skipping the entry
// would either lock the operator out of their own library or, worse, silently
// widen what the guard admits.
func TestLoadRejectsMalformedTrustedReadNetwork(t *testing.T) {
	t.Setenv("NEXUSGATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSGATE_TRUSTED_READ_NETWORKS", "192.168.1.0/16, not-a-cidr")
	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject a malformed CIDR range")
	}
}

func TestTrustedReadPrefixesParsesConfiguredRanges(t *testing.T) {
	t.Setenv("NEXUSGATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSGATE_TRUSTED_READ_NETWORKS", "10.9.0.0/16, fd00::/8")
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

func TestLoadReadsHostPlatformFromEnvironment(t *testing.T) {
	for _, raw := range []string{"  unraid  ", "Unraid", "UNRAID", "  Unraid  "} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("NEXUSGATE_DATA_DIR", t.TempDir())
			t.Setenv("NEXUSGATE_HOST_PLATFORM", raw)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.HostPlatform != "unraid" {
				t.Fatalf("host platform=%q want unraid", cfg.HostPlatform)
			}
		})
	}
}

func TestLoadEmptyHostPlatformEnvironmentPreservesDefault(t *testing.T) {
	t.Setenv("NEXUSGATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSGATE_HOST_PLATFORM", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostPlatform != "" {
		t.Fatalf("empty host platform environment changed default to %q", cfg.HostPlatform)
	}

	if err := os.Unsetenv("NEXUSGATE_HOST_PLATFORM"); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostPlatform != "" {
		t.Fatalf("unset host platform environment changed default to %q", cfg.HostPlatform)
	}
}
