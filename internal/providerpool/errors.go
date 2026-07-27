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

	message := strings.ToLower(err.Error())
	if hasAny(message, "configuration", "configured", "config error", "schema", "unauthorized", "forbidden", "bad request", "unprocessable", "invalid request") || hasStatusText(message, 400, 401, 403, 422) {
		return NonRetryable
	}
	if hasAny(message, "timeout", "timed out", "deadline exceeded", "network", "connection reset", "connection refused", "connection aborted", "temporary", "temporarily unavailable", "too many requests", "service unavailable", "server error", "5xx") || hasStatusText(message, 408, 429) || statusTextIs5xx(message) {
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

func hasStatusText(value string, codes ...int) bool {
	for _, code := range codes {
		if strings.Contains(value, strconv.Itoa(code)) {
			return true
		}
	}
	return false
}

func statusTextIs5xx(value string) bool {
	for code := 500; code <= 599; code++ {
		if strings.Contains(value, strconv.Itoa(code)) {
			return true
		}
	}
	return false
}
