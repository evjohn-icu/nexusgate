package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/mount"
)

func newComposeTestService(t *testing.T) *Service {
	t.Helper()
	service, err := NewService(nil, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// On a bare-metal host the mount commands InspectRootPath already generates
// are simply the right answer, and a compose stanza alongside them would be
// noise nobody asked for — so a share inspected from a non-containerised Hub
// must not carry one, matching what a normal `go test` invocation (never
// itself running inside a container) actually reports today.
func TestInspectRootPathOmitsComposeVolumeWhenNotContainerised(t *testing.T) {
	service := newComposeTestService(t)
	service.hostOverride = &mount.Host{OS: "linux", UID: 1000, GID: 1000, Container: false}
	inspection := service.InspectRootPath(context.Background(), "//192.0.2.10/Video", "", HostHint{})
	if !inspection.IsShare {
		t.Fatal("//192.0.2.10/Video should parse as a share")
	}
	if inspection.ComposeVolume != nil {
		t.Fatalf("a non-containerised host must not get a compose-volume suggestion, got %+v", inspection.ComposeVolume)
	}
}

// A containerised Hub (mount.Host.Container) genuinely cannot mount the share
// for itself — see the container-cannot-mount note mount.Guidance already
// attaches — so this is the one case where the compose stanza is the actual
// next step. hostOverride exists (see Service.hostOverride's doc) precisely
// so this branch can be exercised without the test binary itself running
// inside a container.
func TestInspectRootPathAddsComposeVolumeWhenContainerised(t *testing.T) {
	service := newComposeTestService(t)
	service.hostOverride = &mount.Host{OS: "linux", UID: 1000, GID: 1000, Container: true}

	nfs := service.InspectRootPath(context.Background(), "192.0.2.10:/volume1/Video", "", HostHint{})
	if !nfs.IsShare {
		t.Fatal("192.0.2.10:/volume1/Video should parse as a share")
	}
	if nfs.ComposeVolume == nil {
		t.Fatal("a containerised host must get a compose-volume suggestion for an NFS share")
	}
	if nfs.ComposeVolume.Warning != "" || nfs.ComposeVolume.WarningKey != "" {
		t.Errorf("the NFS form carries no credentials to warn about, got Warning=%q WarningKey=%q", nfs.ComposeVolume.Warning, nfs.ComposeVolume.WarningKey)
	}
	if !strings.Contains(nfs.ComposeVolume.YAML, `type: "nfs"`) {
		t.Errorf("NFS compose YAML missing the nfs driver type, got:\n%s", nfs.ComposeVolume.YAML)
	}

	smb := service.InspectRootPath(context.Background(), "//192.0.2.10/Video", "", HostHint{})
	if !smb.IsShare {
		t.Fatal("//192.0.2.10/Video should parse as a share")
	}
	if smb.ComposeVolume == nil {
		t.Fatal("a containerised host must get a compose-volume suggestion for an SMB share")
	}
	if smb.ComposeVolume.Warning == "" {
		t.Fatal("the SMB form must warn that the password ends up in cleartext")
	}
	if smb.ComposeVolume.WarningKey == "" {
		t.Fatal("a non-empty Warning must carry a WarningKey so the wizard can translate it")
	}
}

// Share.User never carries a password (see mount.Share's doc and ParseShare's
// userinfo handling), so the SMB compose YAML always uses the
// YOUR_NAS_PASSWORD placeholder — but that guarantee lives in internal/mount.
// This checks it holds all the way through the JSON this Hub actually sends
// to a browser: a password pasted into the wizard must not reach the new
// compose_volume.yaml field any more than it reaches path or share.
func TestInspectRootPathComposeVolumeNeverEchoesAPastedPassword(t *testing.T) {
	service := newComposeTestService(t)
	service.hostOverride = &mount.Host{OS: "linux", UID: 1000, GID: 1000, Container: true}

	inspection := service.InspectRootPath(context.Background(), "smb://ev:hunter2@192.0.2.10/Video", "", HostHint{})
	if inspection.ComposeVolume == nil {
		t.Fatal("a containerised host must get a compose-volume suggestion for this SMB share")
	}
	encoded, err := json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "hunter2") {
		t.Fatalf("the pasted password reached the JSON response: %s", encoded)
	}
	if strings.Contains(inspection.ComposeVolume.YAML, "hunter2") {
		t.Fatalf("the pasted password reached compose_volume.yaml specifically: %s", inspection.ComposeVolume.YAML)
	}
}

// mount.Step distinguishes a line to run from a line to append to a file, but
// MountGuideStep is a hand-written projection: a field added to mount.Step and
// not copied here reaches nothing. That is not a hypothetical — Kind and File
// were added upstream and silently stopped at this boundary, leaving the
// browser rendering an /etc/fstab entry as a runnable command exactly as
// before, with every Go-side test still green.
func TestMountGuideStepProjectionCarriesKindAndFile(t *testing.T) {
	service := newComposeTestService(t)
	service.hostOverride = &mount.Host{OS: "linux", UID: 1000, GID: 1000, Container: false}
	inspection := service.InspectRootPath(context.Background(), "//192.0.2.10/Video", "/mnt/video", HostHint{})
	if inspection.Guidance == nil {
		t.Fatal("a share must carry mount guidance")
	}
	// Positive control: the upstream package really does produce a file-line
	// step for this input, so a missing projection cannot look like "there was
	// nothing to project".
	var upstream int
	for _, step := range mount.Guidance(mount.Share{Protocol: mount.ProtocolSMB, Host: "192.0.2.10", Name: "Video"}, "/mnt/video", mount.Host{OS: "linux", UID: 1000, GID: 1000}).Steps {
		if step.Kind == "file-line" {
			upstream++
		}
	}
	if upstream == 0 {
		t.Fatal("mount.Guidance produced no file-line step — this guard is watching the wrong input")
	}
	var projected int
	for _, step := range inspection.Guidance.Steps {
		if step.Kind != "file-line" {
			continue
		}
		projected++
		if step.File == "" {
			t.Errorf("step %q is a file-line with no File, so the browser cannot name the file", step.Key)
		}
	}
	if projected != upstream {
		t.Fatalf("projected %d file-line steps, upstream produced %d — MountGuideStep is dropping fields", projected, upstream)
	}
}

// The hint carries the two things this process cannot observe about its own
// deployment. Everything else in mount.Host is proven locally, and a caller
// able to claim Container or MediaBind would be choosing which paths the
// wizard tells an operator to mount — instructions the operator follows.
func TestHostHintCannotOverrideProvenHostFacts(t *testing.T) {
	service := newComposeTestService(t)
	proven := mount.Host{OS: "linux", UID: 1000, GID: 1000, Container: true, MediaBind: mount.Bind{Target: "/media/library", Source: "/mnt/remotes"}}
	service.hostOverride = &proven
	applied := HostHint{Platform: "unraid", Service: true}.apply(proven)
	if applied.Platform != "unraid" || !applied.Service {
		t.Fatalf("the hint must set the two facts it owns, got %+v", applied)
	}
	if applied.Container != proven.Container || applied.UID != proven.UID || applied.GID != proven.GID || applied.MediaBind != proven.MediaBind || applied.OS != proven.OS || applied.WSL != proven.WSL {
		t.Fatalf("the hint changed a locally proven fact: %+v vs %+v", applied, proven)
	}
	// Service is one-way: detection proves it, and a caller must not be able to
	// suppress a warning that detection established.
	provenService := proven
	provenService.Service = true
	if !(HostHint{Service: false}.apply(provenService)).Service {
		t.Fatal("a hint must not be able to turn Service off once it was detected")
	}
}

// Platform selects which instructions an administrator is shown, so a label
// outside the closed set is dropped rather than carried through to guidance.
func TestUnknownPlatformHintIsDroppedNotCarried(t *testing.T) {
	service := newComposeTestService(t)
	service.hostOverride = &mount.Host{OS: "linux", UID: 1000, GID: 1000, Container: true, MediaBind: mount.Bind{Target: "/media/library", Source: "/mnt/remotes"}}
	unraid := service.InspectRootPath(context.Background(), "//192.0.2.10/Video", "", HostHint{Platform: "unraid"})
	bogus := service.InspectRootPath(context.Background(), "//192.0.2.10/Video", "", HostHint{Platform: "unraid-but-not-really"})
	keys := func(inspection RootInspection) string {
		if inspection.Guidance == nil {
			t.Fatal("a share must carry guidance")
		}
		var out string
		for _, step := range inspection.Guidance.Steps {
			out += step.Key + " "
		}
		return out
	}
	unraidKeys, bogusKeys := keys(unraid), keys(bogus)
	// Positive control: the accepted label really does reach a different branch,
	// so the inequality below is evidence and not an accident of formatting.
	if !strings.Contains(unraidKeys, "unraid-") {
		t.Fatalf("platform=unraid did not reach the Unraid branch: %s", unraidKeys)
	}
	if strings.Contains(bogusKeys, "unraid-") {
		t.Fatalf("an unrecognised platform label reached the Unraid branch: %s", bogusKeys)
	}
}
