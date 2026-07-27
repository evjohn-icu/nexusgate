package capture

import (
	"reflect"
	"testing"
	"time"
)

func TestMergeUsesFieldFamilyPrecedenceAndPreservesConflicts(t *testing.T) {
	capturedAt := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	original := Capture{
		Identity: CaptureIdentity{
			DeviceID:     "camera-device-1",
			DeviceSerial: "SERIAL-1",
			Model:        "Camera One",
		},
		Context: CaptureContext{
			CapturedAt:    capturedAt,
			SessionMarker: "camera-session",
		},
		Source: SourceOriginal,
		Evidence: []Evidence{{
			Source:   "original-media",
			Priority: PriorityOriginal,
			Locator:  "container.metadata",
		}},
	}
	sidecar := Capture{
		Identity: CaptureIdentity{
			DeviceID:     "sidecar-device-claim",
			DeviceSerial: "SERIAL-1",
			Model:        "Camera One (sidecar)",
		},
		Context: CaptureContext{
			CapturedAt:    capturedAt.Add(2 * time.Minute),
			SessionMarker: "editor-session",
			ReelMarker:    "reel-03",
		},
		Source: SourceSidecar,
		Evidence: []Evidence{{
			Source:   "sidecar-xmp",
			Priority: PrioritySidecar,
			Locator:  "xmp.CameraSerial",
		}},
	}

	merged := Merge(original, sidecar)
	if merged.Identity.DeviceID != original.Identity.DeviceID {
		t.Fatalf("identity device ID=%q, want original value %q", merged.Identity.DeviceID, original.Identity.DeviceID)
	}
	if merged.Identity.DeviceSerial != original.Identity.DeviceSerial {
		t.Fatalf("identity serial=%q, want %q", merged.Identity.DeviceSerial, original.Identity.DeviceSerial)
	}
	if !merged.Context.CapturedAt.Equal(capturedAt) {
		t.Fatalf("capture time=%s, want %s", merged.Context.CapturedAt, capturedAt)
	}
	if merged.Context.SessionMarker != "editor-session" {
		t.Fatalf("session marker=%q, want sidecar marker", merged.Context.SessionMarker)
	}
	if merged.Context.ReelMarker != "reel-03" {
		t.Fatalf("reel marker=%q, want sidecar marker", merged.Context.ReelMarker)
	}

	if got := merged.Provenance.Fields[FieldIdentityDeviceID].Selected[0].Source; got != "original-media" {
		t.Fatalf("selected device provenance=%q, want original-media", got)
	}
	conflict, ok := findConflict(merged.Conflicts, FieldIdentityDeviceID)
	if !ok {
		t.Fatalf("device ID conflict missing: %#v", merged.Conflicts)
	}
	if len(conflict.Values) != 2 {
		t.Fatalf("device ID conflict values=%d, want 2: %#v", len(conflict.Values), conflict)
	}
	if conflict.Selected != original.Identity.DeviceID {
		t.Fatalf("selected conflict value=%q, want %q", conflict.Selected, original.Identity.DeviceID)
	}

	if !reflect.DeepEqual(merged, Merge(sidecar, original)) {
		t.Fatalf("merge must be independent of input order:\nforward=%#v\nreverse=%#v", merged, Merge(sidecar, original))
	}
}

func findConflict(conflicts []Conflict, field string) (Conflict, bool) {
	for _, conflict := range conflicts {
		if conflict.Field == field {
			return conflict, true
		}
	}
	return Conflict{}, false
}
