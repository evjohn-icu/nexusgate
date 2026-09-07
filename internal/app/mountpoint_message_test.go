package app

import (
	"context"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/mount"
)

// The refusal message quotes a character set back to the operator, and an
// operator who follows it expects the next attempt to be accepted. That only
// holds while the sentence and mount.safeForCommand agree, and they have
// already disagreed once: the backslash stayed in the advertised set after it
// was removed from the policy, with every test green. Nothing else in the tree
// compares the two — the message is a string literal and the policy is a switch
// statement in another package — so this is the only place the drift can be
// caught.
func TestMountpointInvalidMessageAdvertisesOnlyAcceptedCharacters(t *testing.T) {
	if !strings.Contains(mountpointInvalidMessage, advertisedMountpointChars) {
		t.Fatalf("the message no longer quotes advertisedMountpointChars verbatim, so the set below\nis no longer what the operator is told:\n%s", mountpointInvalidMessage)
	}
	host := mount.Host{OS: "linux", UID: 1000, GID: 1000}
	for _, r := range advertisedMountpointChars {
		if r == ' ' {
			// The constant is spaced out for legibility; the spaces are
			// separators in the prose, not members of the set. A space is
			// itself refused, which the positive control below proves.
			continue
		}
		candidate := "/mnt/nexusgate/video" + string(r)
		if !mount.ValidMountpoint(candidate, host) {
			t.Errorf("the message tells the operator %q is allowed in a mount point, but\nmount.ValidMountpoint(%q) refuses it — the advice would send them in a circle", string(r), candidate)
		}
	}

	// Positive controls. Without them a ValidMountpoint that returned true for
	// everything would satisfy every assertion above, and so would an
	// advertisedMountpointChars that had been emptied.
	if advertisedMountpointChars == "" {
		t.Fatal("advertisedMountpointChars is empty, so the loop above asserted nothing")
	}
	for _, refused := range []string{"/mnt/My Footage", `/mnt/nexusgate\video`, "/mnt/nexusgate/$(id)"} {
		if mount.ValidMountpoint(refused, host) {
			t.Errorf("mount.ValidMountpoint(%q) accepted a mount point the message says is refused;\neither the policy widened or this test is no longer measuring it", refused)
		}
	}
}

// The constant is only worth pinning if it is what actually reaches the
// operator. InspectRootPath is the one producer, and it is reached from the
// /api/v1/roots/inspect handler whose admin credential is waived for LAN peers
// under the default trusted_network — so this path is not hypothetical.
func TestInspectRootPathCarriesTheMountpointRefusalItAdvertises(t *testing.T) {
	service := newComposeTestService(t)
	service.hostOverride = &mount.Host{OS: "linux", UID: 1000, GID: 1000}
	inspection := service.InspectRootPath(context.Background(), "//192.0.2.10/Video", "/mnt/nexusgate/Video; id > /tmp/pwned", HostHint{})

	var found *RootWarningDetail
	for i := range inspection.WarningDetails {
		if inspection.WarningDetails[i].Code == "root.mountpoint_invalid" {
			found = &inspection.WarningDetails[i]
		}
	}
	if found == nil {
		t.Fatalf("a mount point carrying a semicolon must be refused with root.mountpoint_invalid, got %+v", inspection.WarningDetails)
	}
	if found.Message != mountpointInvalidMessage {
		t.Errorf("the emitted message is not the constant this file pins:\n got: %s\nwant: %s", found.Message, mountpointInvalidMessage)
	}
	// The offending text must not come back on the page that will render it.
	if strings.Contains(found.Message, "pwned") || strings.Contains(strings.Join(inspection.Warnings, "\n"), "pwned") {
		t.Error("the rejected mount point was echoed back into a response the browser renders")
	}
	// A refused mount point must also withhold the guidance, not merely warn
	// about it: the steps are what the operator pastes into a shell.
	if inspection.Guidance != nil {
		t.Errorf("a refused mount point must yield no copy-paste guidance, got %+v", inspection.Guidance)
	}
}
