package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/nleexport"
	"github.com/evjohn-icu/timingdex/internal/providerchannels"
	"github.com/evjohn-icu/timingdex/internal/search"
)

// APIError is the machine-readable error body every API failure answers with.
// The code field is a stable identifier an agent or MCP tool can switch on —
// it never depends on message wording — while message is for a human and
// action, when present, tells the caller what to do next. retryable and
// next_retry_at exist because the pipeline's retry decision is a property of
// the failure, and a caller that cannot see it will guess.
type APIError struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Retryable   bool   `json:"retryable,omitempty"`
	Action      string `json:"action,omitempty"`
	NextRetryAt string `json:"next_retry_at,omitempty"`
}

// errorEnvelope is the wire shape: the error is deliberately nested under an
// "error" key so the response stays a single JSON object even when a future
// endpoint has something besides the failure to report.
type errorEnvelope struct {
	Error APIError `json:"error"`
}

// maxAPIErrorMessageBytes bounds the error text copied into an envelope's
// message field. That text reaches browser pages, the Worker, and any agent
// or MCP tool that passes the response back upstream, and the audit that
// bounded common.ReadError (internal/providers/common/http.go) found the same
// unbounded-copy pattern persisting Provider keys: a relay can echo a request
// body into an error string. The bound there is 2048; the message field here
// is one field of a JSON body rather than a whole error, so it is tighter.
const maxAPIErrorMessageBytes = 300

// writeAPIError answers a request with the machine-readable error envelope.
// The body is JSON — {"error":{code,message,retryable,action,next_retry_at}}
// — because API failures are consumed twice: an agent or MCP tool needs a
// stable code to switch on and a retryable flag to decide whether to retry,
// and the browser pages decode the same body to show the operator the message
// and, when present, the action. The old ad hoc text bodies were a
// user-facing copy layer with no structure: prose that nothing could switch
// on, so every consumer re-invented the classification by reading words out
// of a sentence — the exact trap the sentinel-based classifiers in this
// repository exist to close. The status code stays the caller's choice so a
// handler keeps answering 404/409/… exactly as it did before.
func writeAPIError(w http.ResponseWriter, status int, e APIError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: e})
}

// apiErrorFromError maps an error to the HTTP status and envelope it deserves,
// with errors.Is (and errors.As for value sentinels like ErrShareNotMounted)
// against the sentinels the lower layers return — never by reading the
// message text, for the reason every sentinel's doc comment gives: rewording
// an error is ordinary maintenance and must not reclassify a response. Where
// a handler already knows better than this table (a call site that today
// answers 400 for a parseFacetFilter failure, which carries no sentinel and
// so cannot be recognized structurally), the handler keeps its explicit
// status and uses writeAPIError directly.
//
// The default is deliberately a generic 500: an error this classifier does
// not know is not proven to be the caller's fault, and its text must not
// reach the response — the detail belongs in the log, which the caller (or
// writeErrorEnvelope) already wrote.
func apiErrorFromError(err error) (int, APIError) {
	var shareErr app.ErrShareNotMounted
	if errors.As(err, &shareErr) {
		// The message carries the share name — the operator's own input, safe
		// to echo and exactly what makes the response actionable.
		return http.StatusUnprocessableEntity, APIError{
			Code:    "share_not_mounted",
			Message: truncateMessage(shareErr.Error(), maxAPIErrorMessageBytes),
			Action:  "mount_the_share",
		}
	}
	switch {
	case errors.Is(err, search.ErrSearchPaginationWindow):
		return http.StatusBadRequest, APIError{Code: "invalid_request", Message: truncateMessage(err.Error(), maxAPIErrorMessageBytes)}
	case errors.Is(err, domain.ErrJobLeaseLost), errors.Is(err, app.ErrWorkerArtifactLease):
		return http.StatusConflict, APIError{Code: "job_lease_lost", Message: "job lease no longer held", Retryable: true}
	case errors.Is(err, domain.ErrPermanentFailure):
		return http.StatusUnprocessableEntity, APIError{Code: "permanent_failure", Message: "the operation cannot succeed as requested"}
	case errors.Is(err, providerchannels.ErrRouteExhausted):
		return http.StatusServiceUnavailable, APIError{Code: "provider_route_exhausted", Message: "every configured provider key on this route is failing", Retryable: true, Action: "wait_or_change_provider"}
	case errors.Is(err, providerchannels.ErrNoRoute):
		return http.StatusServiceUnavailable, APIError{Code: "provider_no_route", Message: "no provider route is available for this capability", Retryable: true, Action: "configure_or_enable_a_provider_channel"}
	case errors.Is(err, domain.ErrPlanNotFound),
		errors.Is(err, domain.ErrPlanRevisionNotFound),
		errors.Is(err, domain.ErrShotVectorNotFound),
		errors.Is(err, domain.ErrShotNotFound),
		errors.Is(err, app.ErrCollectionNotFound),
		errors.Is(err, app.ErrWebDAVSpaceNotFound):
		return http.StatusNotFound, APIError{Code: "not_found", Message: "resource not found", Action: "check_the_identifier"}
	case errors.Is(err, domain.ErrReorderInvalid):
		return http.StatusConflict, APIError{Code: "conflict", Message: "the shot list no longer matches the collection's current pins", Retryable: true, Action: "refresh_the_basket"}
	case errors.Is(err, domain.ErrCollectionExists), errors.Is(err, app.ErrWebDAVAccountExists):
		return http.StatusConflict, APIError{Code: "already_exists", Message: "resource already exists"}
	case errors.Is(err, domain.ErrPlanImmutable):
		return http.StatusConflict, APIError{Code: "plan_immutable", Message: "the plan is already approved and cannot be changed"}
	case errors.Is(err, domain.ErrPlanRevisionNotDraft), errors.Is(err, domain.ErrPlanRevisionNotLatest):
		return http.StatusConflict, APIError{Code: "plan_revision_conflict", Message: "the revision is no longer the latest draft"}
	case errors.Is(err, app.ErrPlanNotApproved):
		return http.StatusConflict, APIError{Code: "plan_not_approved", Message: "the plan must be approved before it can be exported"}
	case errors.Is(err, app.ErrPlanNotExportable), errors.Is(err, nleexport.ErrInvalidTimeline):
		return http.StatusUnprocessableEntity, APIError{Code: "plan_not_exportable", Message: "the plan cannot be rendered as an export"}
	case errors.Is(err, domain.ErrJobNotAssignable):
		return http.StatusConflict, APIError{Code: "job_not_assignable", Message: "the job is running and cannot be reassigned"}
	case errors.Is(err, app.ErrInvalidRepurposeRevision),
		errors.Is(err, app.ErrProviderChannelValidation),
		errors.Is(err, app.ErrInvalidWorkerArtifact),
		errors.Is(err, app.ErrWebDAVAccountInvalid),
		errors.Is(err, app.ErrWebDAVLinkKindInvalid),
		errors.Is(err, domain.ErrInvalidAssignment):
		// A validation refusal is the one class where the error text is
		// genuinely informative — it names the rejected value — so it is
		// carried into the message, truncated to the same bound that keeps a
		// Provider key echoed by a relay from being persisted.
		return http.StatusBadRequest, APIError{Code: "invalid_request", Message: truncateMessage(err.Error(), maxAPIErrorMessageBytes)}
	case errors.Is(err, app.ErrPairingTokenInvalid):
		return http.StatusUnauthorized, APIError{Code: "pairing_token_invalid", Message: "worker enrollment rejected"}
	case errors.Is(err, app.ErrWorkerProviderCredentialDeliveryDisabled):
		return http.StatusForbidden, APIError{Code: "worker_credential_delivery_disabled", Message: "worker provider credential delivery is disabled"}
	case errors.Is(err, app.ErrWorkerProviderConfiguredAsChannelOnly):
		return http.StatusForbidden, APIError{Code: "worker_provider_channel_only", Message: "this capability is configured as a provider channel, which worker access does not read"}
	case errors.Is(err, app.ErrWorkerProviderNotConfigured):
		return http.StatusServiceUnavailable, APIError{Code: "worker_provider_not_configured", Message: "no provider is configured for this capability"}
	case errors.Is(err, app.ErrProviderProxyRequest):
		return http.StatusBadGateway, APIError{Code: "provider_proxy_failed", Message: "provider proxy request failed"}
	}
	return http.StatusInternalServerError, APIError{Code: "internal_error", Message: "internal server error"}
}

// writeErrorEnvelope is the drop-in replacement for the flat text writeError:
// it logs the full error for the operator and answers with the envelope that
// apiErrorFromError classifies. The status follows the classification — a 500
// stays a 500, a recognized sentinel answers with the status its handler
// already used.
func writeErrorEnvelope(w http.ResponseWriter, err error) {
	slog.Error("request failed", "error", err)
	status, e := apiErrorFromError(err)
	writeAPIError(w, status, e)
}

// truncateMessage caps error text at max bytes, cutting on a rune boundary so
// a multi-byte (e.g. Chinese) message is never split mid-character, and marks
// the cut the same way common.ReadError does. The marker is allowed to push
// the total past the bound by its own length, matching that function's
// contract.
func truncateMessage(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…(truncated)"
}
