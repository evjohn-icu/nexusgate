package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestValidateContainerAdminAuth pins the container guard's decision table. The
// guard only fires when a containerised Hub runs the default trusted_network
// mode without an explicit admin_auth_networks allowlist — the one combination
// where a published Docker port would waive admin writes for the whole
// internet. Every other combination is allowed.
func TestValidateContainerAdminAuth(t *testing.T) {
	cases := []struct {
		name        string
		isContainer bool
		adminAuth   string
		networks    []string
		wantErr     bool
	}{
		{
			name:        "container trusted_network without networks fails",
			isContainer: true,
			adminAuth:   "trusted_network",
			wantErr:     true,
		},
		{
			name:        "container trusted_network with networks passes",
			isContainer: true,
			adminAuth:   "trusted_network",
			networks:    []string{"10.0.0.0/8"},
			wantErr:     false,
		},
		{
			name:        "container required without networks passes",
			isContainer: true,
			adminAuth:   "required",
			wantErr:     false,
		},
		{
			name:        "container off without networks passes",
			isContainer: true,
			adminAuth:   "off",
			wantErr:     false,
		},
		{
			name:        "bare metal trusted_network without networks passes",
			isContainer: false,
			adminAuth:   "trusted_network",
			wantErr:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{}
			cfg.HubSecurity.AdminAuth = tc.adminAuth
			cfg.HubSecurity.AdminAuthNetworks = tc.networks
			err := ValidateContainerAdminAuth(tc.isContainer, cfg)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tc.wantErr && err.Error() == "" {
				t.Fatal("expected an actionable error message, got empty")
			}
		})
	}
}

// TestAdminAuthPrefixesParsesConfiguredRanges mirrors the trusted-read prefix
// parsing: each CIDR is parsed and masked, and the result is returned in order.
func TestAdminAuthPrefixesParsesConfiguredRanges(t *testing.T) {
	cfg := HubSecurityConfig{AdminAuthNetworks: []string{"10.9.0.0/16", "fd00::/8"}}
	prefixes, err := cfg.AdminAuthPrefixes()
	if err != nil || len(prefixes) != 2 {
		t.Fatalf("prefixes=%v err=%v", prefixes, err)
	}
	if prefixes[0].String() != "10.9.0.0/16" || prefixes[1].String() != "fd00::/8" {
		t.Fatalf("prefixes=%v", prefixes)
	}
}

// TestAdminAuthPrefixesEmptyReturnsEmpty pins that an unset allowlist yields an
// empty result (the API layer then falls back to the built-in private ranges).
func TestAdminAuthPrefixesEmptyReturnsEmpty(t *testing.T) {
	prefixes, err := (HubSecurityConfig{}).AdminAuthPrefixes()
	if err != nil || len(prefixes) != 0 {
		t.Fatalf("prefixes=%v err=%v", prefixes, err)
	}
}

// TestAdminAuthPrefixesRejectsMalformedCIDR pins that a bad range fails rather
// than being silently skipped, which would widen or narrow the waiver.
func TestAdminAuthPrefixesRejectsMalformedCIDR(t *testing.T) {
	cfg := HubSecurityConfig{AdminAuthNetworks: []string{"not-a-cidr"}}
	if _, err := cfg.AdminAuthPrefixes(); err == nil {
		t.Fatal("expected a malformed CIDR to error")
	}
}

// TestLoadRejectsInvalidAdminAuth pins that an admin_auth value outside the
// three modes fails startup instead of silently falling back to a default.
func TestLoadRejectsInvalidAdminAuth(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("TIMINGDEX_DATA_DIR", dataDir)
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"hub_security":{"admin_auth":"yes"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject an invalid admin_auth value")
	}
}

// TestLoadNormalizesEmptyAdminAuthToTrustedNetwork pins the default: a config
// that never mentions admin_auth (or leaves it blank) must load as
// trusted_network, not fail validation.
func TestLoadNormalizesEmptyAdminAuthToTrustedNetwork(t *testing.T) {
	t.Setenv("TIMINGDEX_DATA_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HubSecurity.AdminAuth != "trusted_network" {
		t.Fatalf("admin_auth=%q want trusted_network", cfg.HubSecurity.AdminAuth)
	}
}

// TestAdminAuthModeNormalizesEmptyToTrustedNetwork pins the access-point
// default: a zero-value HubSecurityConfig (which never went through Load)
// must answer trusted_network, and an explicit mode passes through unchanged.
func TestAdminAuthModeNormalizesEmptyToTrustedNetwork(t *testing.T) {
	if got := (HubSecurityConfig{}).AdminAuthMode(); got != "trusted_network" {
		t.Fatalf("AdminAuthMode()=%q want trusted_network", got)
	}
	if got := (HubSecurityConfig{AdminAuth: "off"}).AdminAuthMode(); got != "off" {
		t.Fatalf("AdminAuthMode()=%q want off", got)
	}
}
