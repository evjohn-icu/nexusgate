package config

import "testing"

// The deferral predates this field, so an install that never sets it must
// keep waiting exactly five hours after a route exhausts. The flooring of a
// mistyped value is tested at the point of use (NewPipeline in internal/app),
// where the busy-retry loop it prevents would actually be entered.
func TestLoadDefaultsProviderRouteDeferralToFiveHours(t *testing.T) {
	t.Setenv("NEXUSGATE_DATA_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipeline.ProviderRouteDeferralMinutes != 300 {
		t.Fatalf("provider route deferral minutes=%d, want 300 (5h)", cfg.Pipeline.ProviderRouteDeferralMinutes)
	}
}

func TestLoadReadsProviderRouteDeferralFromEnvironment(t *testing.T) {
	t.Setenv("NEXUSGATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSGATE_PIPELINE_PROVIDER_ROUTE_DEFERRAL_MINUTES", "45")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipeline.ProviderRouteDeferralMinutes != 45 {
		t.Fatalf("provider route deferral minutes=%d, want 45", cfg.Pipeline.ProviderRouteDeferralMinutes)
	}
}
