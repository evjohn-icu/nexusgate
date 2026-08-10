package common

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type Endpoint struct {
	BaseURL        string            `json:"base_url"`
	APIKey         string            `json:"-"`
	AuthHeader     string            `json:"auth_header,omitempty"`
	AuthScheme     string            `json:"auth_scheme,omitempty"`
	ExtraHeaders   map[string]string `json:"extra_headers,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	HTTPClient     *http.Client      `json:"-"`
}

func (e Endpoint) Client() *http.Client {
	if e.HTTPClient != nil {
		return e.HTTPClient
	}
	timeout := 120 * time.Second
	if e.TimeoutSeconds > 0 {
		timeout = time.Duration(e.TimeoutSeconds) * time.Second
	}
	return &http.Client{Timeout: timeout}
}

func (e Endpoint) URL(path string) string {
	return strings.TrimRight(e.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (e Endpoint) NewRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.URL(path), r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	header := e.AuthHeader
	if header == "" {
		header = "Authorization"
	}
	scheme := e.AuthScheme
	if scheme == "" {
		scheme = "Bearer"
	}
	if e.APIKey != "" {
		value := e.APIKey
		if scheme != "" && scheme != "raw" {
			value = scheme + " " + e.APIKey
		}
		req.Header.Set(header, value)
	}
	for k, v := range e.ExtraHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

// maxErrorBodyBytes bounds the upstream body copied into an error string. That
// string reaches jobs.last_error_message in SQLite and the /progress page, and
// a relay may echo the request headers or body back on failure. Truncating
// keeps an echoed Provider API key from being persisted in full.
const maxErrorBodyBytes = 2048

// maxProviderBodyBytes prevents a successful relay response from turning into
// an unbounded allocation before its diagnostic or raw response is redacted.
const maxProviderBodyBytes = 4 << 20

// StatusError carries the upstream HTTP status alongside the message so the
// pipeline can classify a failure without matching on error text. Most 4xx are
// deterministic setup errors; retrying one only spends paid quota again. The
// exceptions are 401, 402 and 403: those answer about the credential, not the
// request, so another key on the same channel can still succeed. providerpool
// retires that member instead of failing the route — see providerpool.MemberSpent.
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("provider returned HTTP %d: %s", e.StatusCode, e.Body)
}

// HTTPStatusCode exposes the status as a method because StatusCode is a field,
// and a field satisfies no interface. Classifiers outside this package probe
// for a status-bearing error rather than importing it (providerpool holds no
// transport dependency on purpose), and without this method that probe finds
// nothing and silently drops to substring matching on Error(). Nothing fails to
// compile when it is removed; every provider failure is just classified by
// reading digits out of a sentence again.
func (e *StatusError) HTTPStatusCode() int { return e.StatusCode }

func ReadError(resp *http.Response) error {
	b, _ := ReadBody(resp.Body)
	if len(b) > maxErrorBodyBytes+1 {
		b = b[:maxErrorBodyBytes+1]
	}
	body := BoundedString(strings.TrimSpace(string(b)))
	return &StatusError{StatusCode: resp.StatusCode, Body: body}
}

// ReadBody reads a provider response with a finite upper bound. Callers that
// persist or display the result should additionally use BoundedString.
func ReadBody(body io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, maxProviderBodyBytes+1))
}

// ReadErrorWithSecret keeps provider response text useful without allowing an
// echoed request credential to cross into logs or persisted job messages.
func ReadErrorWithSecret(resp *http.Response, secret string) error {
	return RedactError(ReadError(resp), secret)
}

// BoundedString is the persistence bound shared by provider and pipeline errors.
func BoundedString(value string) string {
	if len(value) > maxErrorBodyBytes {
		return value[:maxErrorBodyBytes] + "…(truncated)"
	}
	return value
}

func RedactString(value, secret string) string {
	return BoundedString(redactString(value, secret))
}

func redactString(value, secret string) string {
	if secret == "" {
		return value
	}
	return strings.ReplaceAll(value, secret, "[REDACTED]")
}

// Errorf constructs an error whose text is safe to persist and display.
func Errorf(secret, format string, args ...any) error {
	return RedactError(fmt.Errorf(format, args...), secret)
}

// RedactedError deliberately does not expose the original error through
// Unwrap. Classification is copied through the narrow Is/As surface instead.
type RedactedError struct {
	message string
	cause   error
	status  *StatusError
	netErr  net.Error
}

func (e *RedactedError) Error() string { return e.message }

func (e *RedactedError) Is(target error) bool {
	if e.cause == nil {
		return false
	}
	return errors.Is(e.cause, target)
}

func (e *RedactedError) As(target any) bool {
	if status, ok := target.(**StatusError); ok && e.status != nil {
		*status = e.status
		return true
	}
	if status, ok := target.(*interface{ HTTPStatusCode() int }); ok && e.status != nil {
		*status = e.status
		return true
	}
	if status, ok := target.(*interface{ StatusCode() int }); ok && e.status != nil {
		*status = statusCodeAdapter{code: e.status.StatusCode}
		return true
	}
	if network, ok := target.(*net.Error); ok && e.netErr != nil {
		*network = e.netErr
		return true
	}
	return false
}

type statusCodeAdapter struct{ code int }

func (e statusCodeAdapter) Error() string   { return fmt.Sprintf("provider returned HTTP %d", e.code) }
func (e statusCodeAdapter) StatusCode() int { return e.code }

type redactedNetError struct {
	message string
	cause   net.Error
}

func (e *redactedNetError) Error() string   { return e.message }
func (e *redactedNetError) Timeout() bool   { return e.cause.Timeout() }
func (e *redactedNetError) Temporary() bool { return e.cause.Temporary() }

func (e *RedactedError) Unwrap() error { return nil }

func RedactError(err error, secret string) error {
	if err == nil {
		return nil
	}
	out := &RedactedError{message: RedactString(err.Error(), secret), cause: err}
	var status *StatusError
	if errors.As(err, &status) {
		out.status = &StatusError{StatusCode: status.StatusCode, Body: RedactString(status.Body, secret)}
	}
	var network net.Error
	if errors.As(err, &network) {
		out.netErr = &redactedNetError{message: RedactString(network.Error(), secret), cause: network}
	}
	return out
}
