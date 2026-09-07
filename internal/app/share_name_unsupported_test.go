package app

import (
	"context"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/mount"
)

func newShareNameTestService(t *testing.T, container bool) *Service {
	t.Helper()
	service, err := NewService(nil, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	service.hostOverride = &mount.Host{OS: "linux", UID: 1000, GID: 1000, Container: container}
	return service
}

func warningCodes(inspection RootInspection) map[string]int {
	counts := map[string]int{}
	for _, detail := range inspection.WarningDetails {
		counts[detail.Code]++
	}
	return counts
}

// A share name carrying a space is a real share, and until this landed the Hub
// said otherwise: ParseShare refused it, the wizard classified it as a local
// path, and the operator was answered "cannot read /nas/My Footage" — a path
// they never typed, their "//" silently folded to "/", and the share they did
// type absent from the reply. The commands still cannot be generated, but the
// refusal now names the real reason.
func TestInspectRootPathNamesTheShareItCannotGenerateCommandsFor(t *testing.T) {
	service := newShareNameTestService(t, true)
	inspection := service.InspectRootPath(context.Background(), "//nas/My Footage", "", HostHint{})

	if !inspection.IsShare {
		t.Fatal("//nas/My Footage must be recognised as a share; that recognition is the whole point")
	}
	if inspection.Share == nil || inspection.Share.Name != "My Footage" {
		t.Fatalf("Share = %+v, want the name preserved as typed", inspection.Share)
	}
	if inspection.Path != "//nas/My Footage" {
		t.Errorf("Path = %q, want the share address rather than a local path", inspection.Path)
	}
	if inspection.Guidance != nil {
		t.Errorf("guidance must be withheld for a name that cannot be interpolated, got %+v", inspection.Guidance)
	}
	if inspection.ComposeVolume != nil {
		t.Errorf("the compose stanza must be withheld too: its device: scalar would carry the raw name, got %+v", inspection.ComposeVolume)
	}

	counts := warningCodes(inspection)
	if counts["root.share_name_unsupported"] != 1 {
		t.Fatalf("want exactly one root.share_name_unsupported, got %v", counts)
	}
	var message string
	for _, detail := range inspection.WarningDetails {
		if detail.Code == "root.share_name_unsupported" {
			message = detail.Message
			if detail.Params["path"] != "//nas/My Footage" {
				t.Errorf("params[path] = %q, want the share address", detail.Params["path"])
			}
		}
	}
	if !strings.Contains(message, "//nas/My Footage") {
		t.Errorf("the refusal must name the share the operator typed, got %q", message)
	}
	// The English fallback reaches jobs and the CLI, so it must also say what
	// to do rather than only what was refused.
	if !strings.Contains(message, "rename") || !strings.Contains(message, "mount it yourself") {
		t.Errorf("the refusal must carry both ways forward, got %q", message)
	}
	if !containsString(inspection.Warnings, message) {
		t.Error("the Message projection must reach Warnings, which is what the CLI and doctor print")
	}
}

// The old answer was not merely unhelpful, it was false. Nothing in the reply
// may present the input as a local filesystem path.
func TestInspectRootPathDoesNotCallASpacedShareAMissingLocalPath(t *testing.T) {
	service := newShareNameTestService(t, false)
	inspection := service.InspectRootPath(context.Background(), "//nas/My Footage", "", HostHint{})
	if inspection.Exists {
		t.Error("nothing on the local filesystem was inspected; Exists must stay false")
	}
	for _, warning := range inspection.Warnings {
		if strings.Contains(warning, "/nas/My Footage") && !strings.Contains(warning, "//nas/My Footage") {
			t.Errorf("a warning names the folded local path rather than the share: %q", warning)
		}
	}
}

// Positive control. Without it every assertion above is satisfied by guidance
// simply never being produced for any share at all.
func TestInspectRootPathStillGeneratesCommandsForAnOrdinaryShare(t *testing.T) {
	service := newShareNameTestService(t, false)
	inspection := service.InspectRootPath(context.Background(), "//nas/Video", "", HostHint{})
	if inspection.Guidance == nil {
		t.Fatal("an ordinary share must still get its mount commands")
	}
	commands := 0
	for _, step := range inspection.Guidance.Steps {
		commands += len(step.Commands)
	}
	if commands == 0 {
		t.Fatal("the guidance for an ordinary share must carry commands")
	}
	if warningCodes(inspection)["root.share_name_unsupported"] != 0 {
		t.Error("an ordinary share must not be refused")
	}
}

// The two refusals are independent facts about two different inputs, so both
// are reported when both apply — and neither may be reported twice.
func TestInspectRootPathReportsShareNameAndMountpointRefusalsIndependently(t *testing.T) {
	service := newShareNameTestService(t, false)
	inspection := service.InspectRootPath(context.Background(), "//nas/My Footage", "/mnt/bad;id", HostHint{})
	counts := warningCodes(inspection)
	if counts["root.share_name_unsupported"] != 1 || counts["root.mountpoint_invalid"] != 1 {
		t.Fatalf("want one of each refusal, got %v", counts)
	}
	if inspection.Guidance != nil {
		t.Error("guidance must stay withheld when either input is refused")
	}
}

func containsString(haystack []string, needle string) bool {
	for _, value := range haystack {
		if value == needle {
			return true
		}
	}
	return false
}
