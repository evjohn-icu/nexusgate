package capture

import (
	"testing"
	"time"
)

func TestAggregateSessionsRequiresStableDeviceAndAtMostThirtyMinutes(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	captures := []Capture{
		fixtureCapture("a", SourceOriginal, "device-a", "SERIAL-A", base, ""),
		fixtureCapture("b", SourceOriginal, "device-a", "SERIAL-A", base.Add(30*time.Minute), ""),
		fixtureCapture("c", SourceOriginal, "device-a", "SERIAL-A", base.Add(61*time.Minute), ""),
		fixtureCapture("d", SourceOriginal, "device-b", "SERIAL-B", base.Add(10*time.Minute), ""),
	}

	sessions := AggregateSessions(captures)
	if len(sessions) != 3 {
		t.Fatalf("sessions=%d, want 3; sessions=%#v", len(sessions), sessions)
	}
	if got := len(sessions[0].Captures); got != 2 {
		t.Fatalf("first session captures=%d, want boundary pair of 2: %#v", got, sessions[0])
	}
	if sessions[0].Captures[0].ID != "a" || sessions[0].Captures[1].ID != "b" {
		t.Fatalf("first session order=%q,%q, want a,b", sessions[0].Captures[0].ID, sessions[0].Captures[1].ID)
	}
}

func TestAggregateSessionsAllowsAnExplicitCommonMarkerAcrossDeviceAndTime(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	for _, marker := range []struct {
		name string
		set  func(*CaptureContext)
	}{
		{name: "session", set: func(context *CaptureContext) { context.SessionMarker = "session-1" }},
		{name: "reel", set: func(context *CaptureContext) { context.ReelMarker = "reel-1" }},
		{name: "flight", set: func(context *CaptureContext) { context.FlightMarker = "flight-1" }},
	} {
		t.Run(marker.name, func(t *testing.T) {
			first := fixtureCapture("first", SourceOriginal, "device-a", "SERIAL-A", base, "")
			second := fixtureCapture("second", SourceOriginal, "device-b", "SERIAL-B", base.Add(2*time.Hour), "")
			marker.set(&first.Context)
			marker.set(&second.Context)

			sessions := AggregateSessions([]Capture{first, second})
			if len(sessions) != 1 || len(sessions[0].Captures) != 2 {
				t.Fatalf("common %s marker should join captures: %#v", marker.name, sessions)
			}
		})
	}
}

func TestAggregateSessionsExcludesProxiesAndSidecars(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	original := fixtureCapture("original", SourceOriginal, "device-a", "SERIAL-A", base, "session-1")
	proxy := fixtureCapture("proxy", SourceProxy, "device-a", "SERIAL-A", base.Add(1*time.Minute), "session-1")
	sidecar := fixtureCapture("sidecar", SourceSidecar, "device-a", "SERIAL-A", base.Add(2*time.Minute), "session-1")

	sessions := AggregateSessions([]Capture{original, proxy, sidecar})
	if len(sessions) != 1 {
		t.Fatalf("sessions=%d, want one source session: %#v", len(sessions), sessions)
	}
	if len(sessions[0].Captures) != 1 || sessions[0].Captures[0].ID != "original" {
		t.Fatalf("derived observations must not form or join sessions: %#v", sessions[0].Captures)
	}
}

// TestAggregateSessionsDeduplicatesSameAsset pins the "one file is one
// capture" invariant: the repository builds the capture list from a location
// join, so the same asset ID can arrive more than once (a file mirrored
// under a root). Each asset must appear in exactly one session, once.
func TestAggregateSessionsDeduplicatesSameAsset(t *testing.T) {
	base := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	dupe := fixtureCapture("mirrored", SourceOriginal, "device-a", "SERIAL-A", base, "")
	captures := []Capture{
		dupe,
		{ID: "mirrored", Source: SourceOriginal, Identity: dupe.Identity, Context: dupe.Context},
		{ID: "other", Source: SourceOriginal, Identity: dupe.Identity, Context: CaptureContext{CapturedAt: base.Add(5 * time.Minute)}},
	}

	sessions := AggregateSessions(captures)
	total := 0
	for _, session := range sessions {
		seen := make(map[string]struct{}, len(session.Captures))
		for _, capture := range session.Captures {
			if _, dup := seen[capture.ID]; dup {
				t.Fatalf("session %s contains capture %q twice", session.ID, capture.ID)
			}
			seen[capture.ID] = struct{}{}
			total++
		}
	}
	if total != 2 {
		t.Fatalf("distinct captures=%d, want 2 (mirrored deduplicated)", total)
	}
}

func fixtureCapture(id string, source SourceKind, deviceID, serial string, capturedAt time.Time, marker string) Capture {
	return Capture{
		ID:       id,
		Source:   source,
		Identity: CaptureIdentity{DeviceID: deviceID, DeviceSerial: serial},
		Context:  CaptureContext{CapturedAt: capturedAt, SessionMarker: marker},
	}
}
