// Package capture contains vendor-neutral capture metadata and grouping rules.
// It has no knowledge of media containers, metadata tools, storage, or UI.
package capture

import "time"

// SourceKind describes where a capture observation came from. It is kept
// deliberately generic so callers can map camera- or tool-specific sources to
// these stable categories.
type SourceKind string

const (
	SourceUnknown  SourceKind = "unknown"
	SourceOriginal SourceKind = "original"
	SourceEmbedded SourceKind = "embedded"
	SourceSidecar  SourceKind = "sidecar"
	SourceProxy    SourceKind = "proxy"
	SourceDerived  SourceKind = "derived"
	SourceManual   SourceKind = "manual"
)

// SourcePriority is the strength of an observation within a field family.
// Merge applies a field-family-specific order instead of treating all fields
// as if they had one global winner.
type SourcePriority uint8

const (
	PriorityUnknown SourcePriority = iota
	PriorityProxy
	PriorityDerived
	PriorityFilename
	PrioritySidecar
	PriorityEmbedded
	PriorityOriginal
	PriorityManual
)

// Short aliases keep call sites readable while retaining SourcePriority as
// the public type name.
const (
	PriorityGenerated = PriorityDerived
	PriorityCamera    = PriorityOriginal
)

// FieldFamily identifies the semantic group to which a field belongs.
type FieldFamily string

const (
	FieldFamilyIdentity FieldFamily = "identity"
	FieldFamilyContext  FieldFamily = "context"
	FieldFamilyMarker   FieldFamily = "marker"
	FieldFamilySource   FieldFamily = "source"
)

const (
	FieldIdentityDeviceID     = "identity.device_id"
	FieldIdentityDeviceSerial = "identity.device_serial"
	FieldIdentityManufacturer = "identity.manufacturer"
	FieldIdentityModel        = "identity.model"
	FieldIdentityLensModel    = "identity.lens_model"

	FieldContextCapturedAt    = "context.captured_at"
	FieldContextDuration      = "context.duration"
	FieldContextFilename      = "context.filename"
	FieldContextSourceID      = "context.source_id"
	FieldContextSessionMarker = "context.session_marker"
	FieldContextReelMarker    = "context.reel_marker"
	FieldContextFlightMarker  = "context.flight_marker"

	FieldSource = "source"
)

// Exported field names are stable keys for provenance and conflict lookup.
const (
	FieldDeviceID      = FieldIdentityDeviceID
	FieldDeviceSerial  = FieldIdentityDeviceSerial
	FieldManufacturer  = FieldIdentityManufacturer
	FieldModel         = FieldIdentityModel
	FieldLensModel     = FieldIdentityLensModel
	FieldCapturedAt    = FieldContextCapturedAt
	FieldDuration      = FieldContextDuration
	FieldFilename      = FieldContextFilename
	FieldSourceID      = FieldContextSourceID
	FieldSessionMarker = FieldContextSessionMarker
	FieldReelMarker    = FieldContextReelMarker
	FieldFlightMarker  = FieldContextFlightMarker
)

// CaptureIdentity contains stable, vendor-neutral device identity. DeviceID
// and DeviceSerial are the only fields used for automatic shoot-session joins.
type CaptureIdentity struct {
	DeviceID     string
	DeviceSerial string
	Manufacturer string
	Model        string
	LensModel    string
}

// CaptureContext contains capture-time context and explicit grouping markers.
// CapturedAt is the capture timestamp, not an ingestion or file-system time.
type CaptureContext struct {
	CapturedAt    time.Time
	Duration      time.Duration
	Filename      string
	SourceID      string
	SessionMarker string
	ReelMarker    string
	FlightMarker  string
}

// Evidence identifies the source and strength of one observation. Locator is
// an opaque caller-provided reference such as a metadata field path; this
// package never reads it.
type Evidence struct {
	Source     string
	Priority   SourcePriority
	Locator    string
	Note       string
	ObservedAt time.Time
}

// FieldProvenance records every evidence item observed for one field and the
// evidence supporting the selected value.
type FieldProvenance struct {
	Selected   []Evidence
	Alternates []Evidence
	Observed   []Evidence
}

// Provenance is keyed by the exported field constants above.
type Provenance struct {
	Fields map[string]FieldProvenance
}

// ConflictValue is one distinct observed value for a field.
type ConflictValue struct {
	Value    string
	Evidence []Evidence
}

// Conflict preserves disagreement instead of silently discarding the losing
// observations. Selected is the canonical value chosen by Merge.
type Conflict struct {
	Field    string
	Family   FieldFamily
	Selected string
	Values   []ConflictValue
}

// Capture is an observation of one media item. A Capture can be a source
// observation, a sidecar observation, or a derived/proxy observation. Merge
// combines observations without reading or extracting anything from them.
type Capture struct {
	ID            string
	Identity      CaptureIdentity
	Context       CaptureContext
	Source        SourceKind
	Evidence      []Evidence
	FieldEvidence map[string][]Evidence

	Provenance Provenance
	Conflicts  []Conflict
}

// CaptureObservation and MergedCapture are descriptive aliases for callers
// that want to make the input/output role explicit.
type CaptureObservation = Capture
type MergedCapture = Capture
