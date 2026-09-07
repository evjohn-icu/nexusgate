package app

import (
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/mount"
)

// The test-only host replacement supplies the proven local facts, the
// configured platform supplies the deployment default, and the request hint
// is applied by InspectRootPath after both. Unknown values at either hint
// boundary must not erase a known platform.
func TestHostPlatformPrecedence(t *testing.T) {
	base := mount.Host{OS: "linux", Platform: "test-override"}
	service := &Service{
		cfg:          config.Config{HostPlatform: "unraid"},
		hostOverride: &base,
	}
	if got := service.mountHost().Platform; got != "unraid" {
		t.Fatalf("configured platform=%q want unraid over hostOverride", got)
	}

	configured := service.mountHost()
	if got := (HostHint{Platform: "unraid"}).apply(configured).Platform; got != "unraid" {
		t.Fatalf("request platform=%q want unraid over configured value", got)
	}
	if got := (HostHint{Platform: "not-a-platform"}).apply(configured).Platform; got != "unraid" {
		t.Fatalf("unknown request platform cleared configured value: got %q", got)
	}

	// With no configured hint, a known hostOverride platform remains the base
	// value; an unknown configured value is likewise ignored by the one
	// application point that validates platform labels.
	service.cfg.HostPlatform = ""
	base.Platform = "unraid"
	if got := service.mountHost().Platform; got != "unraid" {
		t.Fatalf("hostOverride platform=%q want unraid when config is empty", got)
	}
	service.cfg.HostPlatform = "not-a-platform"
	if got := service.mountHost().Platform; got != "unraid" {
		t.Fatalf("unknown configured platform cleared hostOverride: got %q", got)
	}
}
