package config

import "testing"

func TestLoadReadsSourceStagingModeFromEnvironment(t *testing.T) {
	t.Setenv("TIMINGDEX_DATA_DIR", t.TempDir())
	t.Setenv("TIMINGDEX_SOURCE_STAGING_MODE", "copy")
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
	t.Setenv("TIMINGDEX_DATA_DIR", t.TempDir())
	t.Setenv("TIMINGDEX_TRUSTED_READ_NETWORKS", "192.168.1.0/16, not-a-cidr")
	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject a malformed CIDR range")
	}
}

func TestTrustedReadPrefixesParsesConfiguredRanges(t *testing.T) {
	t.Setenv("TIMINGDEX_DATA_DIR", t.TempDir())
	t.Setenv("TIMINGDEX_TRUSTED_READ_NETWORKS", "10.9.0.0/16, fd00::/8")
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
