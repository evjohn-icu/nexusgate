// Package remote: inference transport.
//
// The Hub and a Worker exchange the same domain types, so this file only adds
// what is specific to a leased call: a capability descriptor, structured
// failure codes, a progress response that can order cancellation, and the
// lease-bound input descriptor for query images.
package remote

import (
	"fmt"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// InferenceFailureCode is the closed set of machine-readable failure reasons.
// Classification everywhere in the Hub happens on this value, never on error
// prose, so rewording a message cannot change a retry decision.
type InferenceFailureCode string

const (
	FailureModelUnavailable     InferenceFailureCode = "model_unavailable"
	FailureUnsupportedOperation InferenceFailureCode = "unsupported_operation"
	FailureUnsupportedInput     InferenceFailureCode = "unsupported_input"
	FailureInvalidOutput        InferenceFailureCode = "invalid_output"
	FailureResourceExhausted    InferenceFailureCode = "resource_exhausted"
	FailureDeadlineExceeded     InferenceFailureCode = "deadline_exceeded"
	FailureCancelled            InferenceFailureCode = "cancelled"
)

// Valid reports whether c is a declared failure code.
func (c InferenceFailureCode) Valid() bool {
	switch c {
	case FailureModelUnavailable, FailureUnsupportedOperation,
		FailureUnsupportedInput, FailureInvalidOutput,
		FailureResourceExhausted, FailureDeadlineExceeded, FailureCancelled:
		return true
	}
	return false
}

// Retryable reports whether the same request may succeed on a later attempt
// without operator action. A missing model or an unsupported input is stable;
// a resource or deadline failure may clear.
func (c InferenceFailureCode) Retryable() bool {
	switch c {
	case FailureResourceExhausted, FailureDeadlineExceeded:
		return true
	}
	return false
}

// InferenceFailure is the typed error an executor returns. It implements error
// so callers can pass it through the ordinary pipeline, while classification
// reads the code.
type InferenceFailure struct {
	Code    InferenceFailureCode `json:"code"`
	Message string               `json:"message,omitempty"`
}

// Error renders a stable, non-secret description.
func (f *InferenceFailure) Error() string {
	if strings.TrimSpace(f.Message) == "" {
		return string(f.Code)
	}
	return fmt.Sprintf("%s: %s", f.Code, f.Message)
}

// InferenceCapability describes one operation a Worker can actually execute.
// It carries no credential and no filesystem path.
type InferenceCapability struct {
	Operation       domain.InferenceOperation `json:"operation"`
	SchemaVersion   string                    `json:"schema_version"`
	ModelID         string                    `json:"model_id"`
	ModelRevision   string                    `json:"model_revision"`
	ConfigHash      string                    `json:"config_hash"`
	SpaceID         string                    `json:"space_id,omitempty"`
	Dimension       int                       `json:"dimension,omitempty"`
	Backend         string                    `json:"backend"`
	Available       bool                      `json:"available"`
	UnavailableCode InferenceFailureCode      `json:"unavailable_code,omitempty"`
	MaxBatch        int                       `json:"max_batch,omitempty"`
	VRAMBudgetMiB   int                       `json:"vram_budget_mib,omitempty"`
	// ModelManagement is true only when the Worker was granted install/probe
	// permission at enrollment. A heartbeat can never set it.
	ModelManagement bool `json:"model_management,omitempty"`
}

// Validate checks a capability for the fields the lease selector relies on.
func (c InferenceCapability) Validate() error {
	if !c.Operation.Valid() {
		return fmt.Errorf("capability names unknown operation %q", c.Operation)
	}
	if c.SchemaVersion != domain.InferenceSchemaVersion {
		return fmt.Errorf("capability schema %q is not %q", c.SchemaVersion, domain.InferenceSchemaVersion)
	}
	if strings.TrimSpace(c.ModelID) == "" || strings.TrimSpace(c.ModelRevision) == "" {
		return fmt.Errorf("capability %q lacks a model identity", c.Operation)
	}
	if strings.TrimSpace(c.Backend) == "" {
		return fmt.Errorf("capability %q lacks a backend", c.Operation)
	}
	if c.Operation.NeedsSpace() && strings.TrimSpace(c.SpaceID) == "" {
		return fmt.Errorf("capability %q lacks a space id", c.Operation)
	}
	if c.Dimension < 0 {
		return fmt.Errorf("capability %q has a negative dimension", c.Operation)
	}
	if !c.Available && !c.UnavailableCode.Valid() {
		return fmt.Errorf("unavailable capability %q must say why", c.Operation)
	}
	return nil
}

// Matches reports whether this capability can execute req.
func (c InferenceCapability) Matches(req domain.InferenceRequest) bool {
	return c.Available &&
		c.Operation == req.Operation &&
		c.SchemaVersion == req.SchemaVersion &&
		c.ModelID == req.ModelID &&
		c.ModelRevision == req.ModelRevision &&
		c.ConfigHash == req.ConfigHash &&
		(!req.Operation.NeedsSpace() || c.SpaceID == req.SpaceID)
}

// InferenceResultSubmission is the body of the lease-bound result endpoint.
// The digest makes a lost-response retry safe: the Hub compares it with the
// bytes it already committed and returns the committed outcome instead of
// writing twice.
type InferenceResultSubmission struct {
	OperationID string                 `json:"operation_id"`
	Digest      string                 `json:"digest"`
	Result      domain.InferenceResult `json:"result"`
}

// InferenceInputDescriptor describes the exact bytes a Worker may fetch for
// the query image bound to its lease. It never contains a path.
type InferenceInputDescriptor struct {
	Kind      domain.InputKind `json:"kind"`
	MediaType string           `json:"media_type"`
	Digest    string           `json:"digest"`
	SizeBytes int64            `json:"size_bytes"`
}

// InferenceProgress is the response to a Worker progress call. Legacy derive
// clients receive 204; inference clients need the lease and cancellation
// state to stop a subprocess before it produces a result nobody will accept.
type InferenceProgress struct {
	CancelRequested bool   `json:"cancel_requested"`
	LeaseExpiresAt  string `json:"lease_expires_at,omitempty"`
	LeaseValid      bool   `json:"lease_valid"`
}

// ModelManagementTask is one queued install/probe unit. It is a maintenance
// job, not an inference.v1 operation: it moves bytes and verifies hashes, it
// never runs a model.
type ModelManagementTask struct {
	JobID           string `json:"job_id"`
	TargetWorkerID  string `json:"target_worker_id"`
	CatalogID       string `json:"catalog_id"`
	ManifestDigest  string `json:"manifest_digest"`
	Action          string `json:"action"`
	LicenseAccepted bool   `json:"license_accepted"`
}
