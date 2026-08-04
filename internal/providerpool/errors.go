package providerpool

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
)

// FailureClass determines whether a completed operation should cool its
// member before it is selected again.
type FailureClass string

const (
	Retryable    FailureClass = "retryable"
	NonRetryable FailureClass = "non_retryable"
	// MemberSpent means the credential failed, not the call. Waiting cannot fix
	// it and the next member can, so the pool stops selecting this one rather
	// than cooling it or letting one dead key end the route. The name is about
	// what the pool does with the member; the statuses that produce it are
	// ClassifyFailure's business alone.
	MemberSpent FailureClass = "member_spent"
)

// HTTPError is a transport-independent status-bearing error useful to callers
// that need to report an HTTP response without coupling this package to an
// HTTP client.
type HTTPError struct {
	Code int
	Err  error
}

func (e HTTPError) Error() string {
	if e.Err == nil {
		return "provider request failed with status " + strconv.Itoa(e.Code)
	}
	return e.Err.Error()
}

func (e HTTPError) Unwrap() error { return e.Err }

func (e HTTPError) StatusCode() int { return e.Code }

// ClassifyFailure classifies status, timeout, network, configuration, and
// schema failures without depending on a particular provider SDK.
func ClassifyFailure(err error) FailureClass {
	if err == nil {
		return NonRetryable
	}
	if code := statusCode(err); code != 0 {
		switch {
		case code == 408 || code == 429 || code >= 500 && code <= 599:
			return Retryable
		// 401, 402 and 403 describe the key: revoked, out of credit, or not
		// entitled to this model. An operator is encouraged to pool several
		// plan keys in one channel, so the same request on the next member can
		// still succeed. Every other 4xx describes the request, which no other
		// key would answer differently — sending it again is a paid call spent
		// on a certain refusal.
		case code == 401 || code == 402 || code == 403:
			return MemberSpent
		case code >= 400 && code <= 499:
			return NonRetryable
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return Retryable
	}
	if errors.Is(err, context.Canceled) {
		return NonRetryable
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return Retryable
	}

	// Below here nothing carries a status. What is left is adapters that never
	// spoke HTTP (the WebSocket ASR path, a local aligner, a capability that is
	// not configured) plus anything that lost its status on the way up, so the
	// text is all there is. It deliberately never yields MemberSpent: retiring a
	// member is a durable decision about a key, and "unauthorized" in a sentence
	// is as likely to be a Hub-side misconfiguration as a dead credential.
	// Guessing wrong here removes a working key from the route; guessing wrong
	// the other way only fails one request.
	//
	// Matching is by vocabulary only, never by a bare three-digit run. This
	// block used to also treat any text containing "500".."599" (and a few
	// other codes) as though it named an HTTP status, on the theory that a
	// status could show up in prose instead of a field. Nothing in this
	// codebase does that: every adapter that speaks HTTP reports its status
	// through *common.StatusError, via common.ReadError, and the probe above
	// already classifies that before this code runs (see statusCode). What
	// the digit check actually saw in production was internal/providers/
	// volcasr's WebSocket ASR path, which has no status to carry and forwards
	// the provider's raw payload verbatim into the error text -- and a
	// Volcengine error code such as 45000002 contains "500" as a plain
	// substring, with no relationship to HTTP semantics. A permanent
	// provider rejection landing on that substring burned the full backoff
	// for nothing. Removing the digit check costs nothing real: no adapter
	// in this repository relies on a status number appearing only in prose.
	message := strings.ToLower(err.Error())
	if hasAny(message, "configuration", "configured", "config error", "schema", "unauthorized", "forbidden", "bad request", "unprocessable", "invalid request") {
		return NonRetryable
	}
	if hasAny(message, "timeout", "timed out", "deadline exceeded", "network", "connection reset", "connection refused", "connection aborted", "temporary", "temporarily unavailable", "too many requests", "service unavailable", "server error", "5xx") {
		return Retryable
	}
	return NonRetryable
}

// IsRetryable reports whether err should trigger member cooldown.
func IsRetryable(err error) bool { return ClassifyFailure(err) == Retryable }

func statusCode(err error) int {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	var httpStatus interface{ HTTPStatusCode() int }
	if errors.As(err, &httpStatus) {
		return httpStatus.HTTPStatusCode()
	}
	return 0
}

func hasAny(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}
