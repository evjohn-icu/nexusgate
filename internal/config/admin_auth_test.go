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
	t.Setenv("NEXUSSLATE_DATA_DIR", dataDir)
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
	t.Setenv("NEXUSSLATE_DATA_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HubSecurity.AdminAuth != "trusted_network" {
		t.Fatalf("admin_auth=%q want trusted_network", cfg.HubSecurity.AdminAuth)
	}
}

// TestLoadReadsAdminAuthNetworksFromEnvironment pins that the container
// deployment variable NEXUSSLATE_HUB_ADMIN_AUTH_NETWORKS lands in
// HubSecurity.AdminAuthNetworks after a comma split, alongside the mode
// override, so a Compose/Unraid operator can deliberately select
// trusted_network and name the real client CIDRs in one place.
func TestLoadReadsAdminAuthNetworksFromEnvironment(t *testing.T) {
	t.Setenv("NEXUSSLATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSSLATE_HUB_ADMIN_AUTH", "trusted_network")
	t.Setenv("NEXUSSLATE_HUB_ADMIN_AUTH_NETWORKS", "10.9.0.0/16, fd00::/8")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HubSecurity.AdminAuth != "trusted_network" {
		t.Fatalf("admin_auth=%q want trusted_network", cfg.HubSecurity.AdminAuth)
	}
	if len(cfg.HubSecurity.AdminAuthNetworks) != 2 {
		t.Fatalf("admin_auth_networks=%v want 2 entries", cfg.HubSecurity.AdminAuthNetworks)
	}
	prefixes, err := cfg.HubSecurity.AdminAuthPrefixes()
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixes) != 2 || prefixes[0].String() != "10.9.0.0/16" || prefixes[1].String() != "fd00::/8" {
		t.Fatalf("prefixes=%v", prefixes)
	}
}

// TestLoadRejectsMalformedAdminAuthNetworksFromEnvironment pins that a typo
// in the container variable fails startup instead of silently widening or
// narrowing the admin waiver.
func TestLoadRejectsMalformedAdminAuthNetworksFromEnvironment(t *testing.T) {
	t.Setenv("NEXUSSLATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSSLATE_HUB_ADMIN_AUTH_NETWORKS", "192.168.1.0/16, not-a-cidr")
	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject a malformed admin_auth_networks CIDR")
	}
}

// TestDeploymentContainerDefaultsPassGuard pins the repository deployment
// defaults: both shipped NAT/container entry points (docker-compose.yml and
// the Unraid template) default NEXUSSLATE_HUB_ADMIN_AUTH to "required", which
// loads and clears the container guard with no networks configured. The
// bare-metal default trusted_network with an empty networks list still fails
// closed inside a container, so an operator who wants the waiver must name
// the real CIDRs explicitly.
func TestDeploymentContainerDefaultsPassGuard(t *testing.T) {
	t.Setenv("NEXUSSLATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSSLATE_HUB_ADMIN_AUTH", "required")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateContainerAdminAuth(true, cfg); err != nil {
		t.Fatalf("required container default must pass the guard: %v", err)
	}

	t.Setenv("NEXUSSLATE_HUB_ADMIN_AUTH", "trusted_network")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateContainerAdminAuth(true, cfg); err == nil {
		t.Fatal("expected trusted_network container default with an empty networks list to fail the guard")
	}
}
