package app

import (
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/mount"
)

// HubWSL must read mount.Host.WSL specifically — not Container, and not a
// hardcoded false — because discoverDiagnostics on the library-roots page
// branches on it to explain an empty LAN scan under WSL's default NAT
// networking. A wiring mistake here (reading the wrong field, or never
// reading hostOverride/mount.LocalHost at all) would silently fall through
// to the generic "nothing on your LAN answered" message for every WSL Hub.
func TestHubWSLReflectsHostOverride(t *testing.T) {
	wsl := mount.Host{OS: "linux", WSL: true}
	service := &Service{hostOverride: &wsl}
	if got := service.HubWSL(); !got {
		t.Fatalf("HubWSL()=%v, want true when hostOverride.WSL is true", got)
	}

	notWSL := mount.Host{OS: "linux", WSL: false}
	service = &Service{hostOverride: &notWSL}
	if got := service.HubWSL(); got {
		t.Fatalf("HubWSL()=%v, want false when hostOverride.WSL is false", got)
	}

	// A containerised, non-WSL host must report false: HubWSL and
	// HubContainerised are two different facts about two different
	// deployment shapes, and discoverDiagnostics's ladder depends on being
	// able to tell them apart.
	containerOnly := mount.Host{OS: "linux", Container: true, WSL: false}
	service = &Service{hostOverride: &containerOnly}
	if got := service.HubWSL(); got {
		t.Fatalf("HubWSL()=%v, want false for a containerised-but-not-WSL host", got)
	}
	if got := service.HubContainerised(); !got {
		t.Fatalf("HubContainerised()=%v, want true for a containerised-but-not-WSL host", got)
	}
}
