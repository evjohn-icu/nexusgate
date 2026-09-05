package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/mount"
	"github.com/evjohn-icu/nexusgate/internal/nleexport"
	"github.com/evjohn-icu/nexusgate/internal/providerchannels"
)

// TestWriteAPIErrorEnvelopeShape pins the wire contract: one JSON object with
// an "error" key holding exactly the five fields, no extra keys, and the
// application/json content type. A browser page decodes message+action and an
// agent switches on code, so both depend on this shape staying stable.
func TestWriteAPIErrorEnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAPIError(rec, http.StatusUnprocessableEntity, APIError{
		Code:        "share_not_mounted",
		Message:     "//nas/video is a network share and is not mounted here",
		Retryable:   true,
		Action:      "mount_the_share",
		NextRetryAt: "2026-08-09T12:00:00Z",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var got struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, rec.Body.String())
	}
	if got.Error.Code != "share_not_mounted" || got.Error.Message == "" ||
		!got.Error.Retryable || got.Error.Action != "mount_the_share" ||
		got.Error.NextRetryAt != "2026-08-09T12:00:00Z" {
		t.Fatalf("envelope fields mismatch: %+v", got.Error)
	}
	var envelope map[string]map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body is not a JSON object: %v", err)
	}
	errorField, ok := envelope["error"]
	if !ok {
		t.Fatalf("body has no top-level \"error\" key: %s", rec.Body.String())
	}
	if len(errorField) != 5 {
		t.Fatalf("error object has %d keys, want exactly 5 (code, message, retryable, action, next_retry_at): %s", len(errorField), rec.Body.String())
	}
}

// TestAPIErrorFromErrorSentinelClasses is the central classifier's contract:
// each sentinel class maps to the status and code the handlers already used
// before the envelope existed. Every case wraps the sentinel (with %w) to
// prove classification survives wrapping — flattening a cause with %v has
// silently broken errors.Is classification in this repository before.
func TestAPIErrorFromErrorSentinelClasses(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantCode  string
		wantRetry bool
		wantErr   bool
	}{
		{"plan not found -> 404", fmt.Errorf("lookup: %w", domain.ErrPlanNotFound), "not_found", false, false},
		{"plan revision not found -> 404", fmt.Errorf("lookup: %w", domain.ErrPlanRevisionNotFound), "not_found", false, false},
		{"shot vector not found -> 404", fmt.Errorf("lookup: %w", domain.ErrShotVectorNotFound), "not_found", false, false},
		{"webdav space not found -> 404", fmt.Errorf("lookup: %w", app.ErrWebDAVSpaceNotFound), "not_found", false, false},
		{"collection exists -> 409 already_exists", fmt.Errorf("save: %w", domain.ErrCollectionExists), "already_exists", false, false},
		{"webdav account exists -> 409 already_exists", fmt.Errorf("create: %w", app.ErrWebDAVAccountExists), "already_exists", false, false},
		{"route exhausted -> 503 retryable", fmt.Errorf("%w: %w", providerchannels.ErrRouteExhausted, errors.New("last non-terminal failure")), "provider_route_exhausted", true, true},
		{"no route -> 503 retryable", fmt.Errorf("%w: %w: capability %q", providerchannels.ErrNoRoute, errors.New("no available"), "asr"), "provider_no_route", true, true},
		{"job lease lost -> 409 retryable", fmt.Errorf("complete: %w", domain.ErrJobLeaseLost), "job_lease_lost", true, false},
		{"worker artifact lease -> 409 retryable", fmt.Errorf("upload: %w", app.ErrWorkerArtifactLease), "job_lease_lost", true, false},
		{"lease lost through Permanent -> 409", fmt.Errorf("run: %w", domain.Permanent(fmt.Errorf("write: %w", domain.ErrJobLeaseLost))), "job_lease_lost", true, false},
		{"permanent failure -> 422", domain.Permanent(fmt.Errorf("analysis failed validation: %w", errors.New("shot description is required"))), "permanent_failure", false, false},
		{"share not mounted -> 422 with action", app.ErrShareNotMounted{Share: mount.Share{Protocol: mount.ProtocolSMB, Host: "nas", Name: "video"}}, "share_not_mounted", false, false},
		{"plan immutable -> 409", fmt.Errorf("revise: %w", domain.ErrPlanImmutable), "plan_immutable", false, false},
		{"plan revision not draft -> 409", fmt.Errorf("approve: %w", domain.ErrPlanRevisionNotDraft), "plan_revision_conflict", false, false},
		{"plan revision not latest -> 409", fmt.Errorf("approve: %w", domain.ErrPlanRevisionNotLatest), "plan_revision_conflict", false, false},
		{"plan not approved -> 409", fmt.Errorf("export: %w", app.ErrPlanNotApproved), "plan_not_approved", false, false},
		{"plan not exportable -> 422", fmt.Errorf("export: %w", app.ErrPlanNotExportable), "plan_not_exportable", false, false},
		{"invalid timeline -> 422", fmt.Errorf("serialize: %w", nleexport.ErrInvalidTimeline), "plan_not_exportable", false, false},
		{"job not assignable -> 409", fmt.Errorf("assign: %w", domain.ErrJobNotAssignable), "job_not_assignable", false, false},
		{"invalid repurpose revision -> 400", fmt.Errorf("revise: %w: section 3", app.ErrInvalidRepurposeRevision), "invalid_request", false, false},
		{"provider channel validation -> 400", fmt.Errorf("%w: duplicate label", app.ErrProviderChannelValidation), "invalid_request", false, false},
		{"invalid worker artifact -> 400", fmt.Errorf("%w: unsupported type %q", app.ErrInvalidWorkerArtifact, "vtt"), "invalid_request", false, false},
		{"webdav account invalid -> 400", fmt.Errorf("create: %w", app.ErrWebDAVAccountInvalid), "invalid_request", false, false},
		{"webdav link kind invalid -> 400", fmt.Errorf("link: %w", app.ErrWebDAVLinkKindInvalid), "invalid_request", false, false},
		{"invalid assignment -> 400", fmt.Errorf("assign: %w", domain.ErrInvalidAssignment), "invalid_request", false, false},
		{"pairing token invalid -> 401", fmt.Errorf("enroll: %w", app.ErrPairingTokenInvalid), "pairing_token_invalid", false, false},
		{"worker credential delivery disabled -> 403", fmt.Errorf("issue: %w", app.ErrWorkerProviderCredentialDeliveryDisabled), "worker_credential_delivery_disabled", false, false},
		{"worker provider channel only -> 403", fmt.Errorf("issue: %w", app.ErrWorkerProviderConfiguredAsChannelOnly), "worker_provider_channel_only", false, false},
		{"worker provider not configured -> 503", fmt.Errorf("issue: %w", app.ErrWorkerProviderNotConfigured), "worker_provider_not_configured", false, true},
		{"provider proxy failed -> 502", fmt.Errorf("proxy: %w", app.ErrProviderProxyRequest), "provider_proxy_failed", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, e := apiErrorFromError(tt.err)
			if (status >= 500) != tt.wantErr {
				t.Errorf("status = %d, wantErr %v", status, tt.wantErr)
			}
			if e.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", e.Code, tt.wantCode)
			}
			if e.Retryable != tt.wantRetry {
				t.Errorf("retryable = %v, want %v", e.Retryable, tt.wantRetry)
			}
		})
	}
}

// TestAPIErrorFromErrorStatusCodes pins the exact HTTP statuses separately
// from the codes, so a code/status drift fails loudly instead of being
// absorbed by the previous test's 5xx blanket.
func TestAPIErrorFromErrorStatusCodes(t *testing.T) {
	tests := []struct {
		err    error
		status int
	}{
		{domain.ErrPlanNotFound, http.StatusNotFound},
		{domain.ErrShotVectorNotFound, http.StatusNotFound},
		{domain.ErrCollectionExists, http.StatusConflict},
		{providerchannels.ErrRouteExhausted, http.StatusServiceUnavailable},
		{domain.ErrJobLeaseLost, http.StatusConflict},
		{app.ErrWorkerArtifactLease, http.StatusConflict},
		{domain.Permanent(errors.New("structural")), http.StatusUnprocessableEntity},
		{app.ErrShareNotMounted{Share: mount.Share{Protocol: mount.ProtocolNFS, Host: "nas", Name: "/video"}}, http.StatusUnprocessableEntity},
		{app.ErrInvalidRepurposeRevision, http.StatusBadRequest},
		{app.ErrWebDAVSpaceNotFound, http.StatusNotFound},
		{app.ErrWorkerProviderNotConfigured, http.StatusServiceUnavailable},
		{app.ErrWorkerProviderConfiguredAsChannelOnly, http.StatusForbidden},
		{app.ErrPairingTokenInvalid, http.StatusUnauthorized},
		{app.ErrProviderProxyRequest, http.StatusBadGateway},
		{nleexport.ErrInvalidTimeline, http.StatusUnprocessableEntity},
		{app.ErrPlanNotApproved, http.StatusConflict},
		{domain.ErrJobNotAssignable, http.StatusConflict},
	}
	for _, tt := range tests {
		status, _ := apiErrorFromError(tt.err)
		if status != tt.status {
			t.Errorf("%v: status = %d, want %d", tt.err, status, tt.status)
		}
	}
}

// TestAPIErrorFromErrorUnknownIsGeneric500 pins the safety rule: an error the
// classifier does not know is a 500 whose message never reveals the
// underlying error's text. The detail belongs in the log (writeErrorEnvelope
// writes it), not in a response that an agent or Worker may echo further.
func TestAPIErrorFromErrorUnknownIsGeneric500(t *testing.T) {
	secret := "sk-test-KEYVALUE-" + strings.Repeat("do-not-leak-", 20)
	status, e := apiErrorFromError(fmt.Errorf("repository explode: %w", errors.New(secret)))
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", status, http.StatusInternalServerError)
	}
	if e.Code != "internal_error" {
		t.Fatalf("code = %q, want internal_error", e.Code)
	}
	if e.Message != "internal server error" {
		t.Fatalf("message = %q, want generic text", e.Message)
	}
	if strings.Contains(e.Message, secret) || strings.Contains(e.Message, "repository explode") {
		t.Fatalf("message leaks underlying error text: %q", e.Message)
	}
}

// TestAPIErrorFromErrorValidationMessageTruncated pins the 300-byte bound on
// the one class that carries error text: a validation refusal naming a long
// rejected value must not grow without limit (the audit that bounded
// common.ReadError found unbounded error copies can persist Provider keys).
func TestAPIErrorFromErrorValidationMessageTruncated(t *testing.T) {
	long := strings.Repeat("x", 2000)
	_, e := apiErrorFromError(fmt.Errorf("%w: invalid asset_type value: %q", app.ErrInvalidRepurposeRevision, long))
	if e.Code != "invalid_request" {
		t.Fatalf("code = %q, want invalid_request", e.Code)
	}
	if len(e.Message) > maxAPIErrorMessageBytes+len("…(truncated)") {
		t.Fatalf("message %d bytes, want at most %d + truncation marker", len(e.Message), maxAPIErrorMessageBytes)
	}
	if !strings.HasSuffix(e.Message, "…(truncated)") {
		t.Fatalf("truncated message not marked: %q", e.Message)
	}
	if strings.Contains(e.Message, long) {
		t.Fatalf("message carries the full untruncated value")
	}
}

// TestTruncateMessageKeepsRuneBoundary pins that a message cut mid-way through
// a multi-byte character is backed off to a rune boundary instead of emitting
// a split UTF-8 sequence (Chinese product copy reaches this field).
func TestTruncateMessageKeepsRuneBoundary(t *testing.T) {
	msg := strings.Repeat("摄", 200)
	got := truncateMessage(msg, 300)
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Fatalf("truncated message not marked: %q", got)
	}
	content := strings.TrimSuffix(got, "…(truncated)")
	if !strings.HasPrefix(msg, content) {
		t.Fatalf("truncated content is not a prefix of the original: %q vs %q", content, msg)
	}
	if strings.Contains(content, "\uFFFD") {
		t.Fatalf("truncation split a multi-byte rune: %q", content)
	}
	status, e := apiErrorFromError(fmt.Errorf("%w: %s", app.ErrProviderChannelValidation, msg))
	if status != http.StatusBadRequest || !strings.HasSuffix(e.Message, "…(truncated)") {
		t.Fatalf("classifier did not truncate multi-byte validation message: %d %q", status, e.Message)
	}
}

// TestWriteErrorEnvelopeLogsAndAnswers pins writeErrorEnvelope's contract: it
// logs the full error (so the operator still has the detail an unknown 500
// refuses to echo) and answers with the classified envelope.
func TestWriteErrorEnvelopeLogsAndAnswers(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErrorEnvelope(rec, fmt.Errorf("boom: %w", domain.ErrCollectionExists))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	var got struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got.Error.Code != "already_exists" {
		t.Fatalf("code = %q, want already_exists", got.Error.Code)
	}
}
