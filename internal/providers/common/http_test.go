package common

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// ReadError's text ends up in jobs.last_error_message and on the /progress
// page. A relay that echoes the request back on failure must not be able to
// park a full Provider key in SQLite, so the body stays bounded.
func TestReadErrorTruncatesUpstreamBody(t *testing.T) {
	body := strings.Repeat("a", 8192)
	resp := &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(body))}

	err := ReadError(resp)
	status, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("ReadError should return *StatusError, got %T", err)
	}
	if status.StatusCode != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", status.StatusCode)
	}
	if len(status.Body) > maxErrorBodyBytes+len("…(truncated)") {
		t.Fatalf("body should be truncated, got %d bytes", len(status.Body))
	}
	if !strings.HasSuffix(status.Body, "…(truncated)") {
		t.Fatal("truncated body should be marked as truncated")
	}
}

// The message format is relied upon by existing provider tests and by the
// substring classification that remains in isRetryableJobError.
func TestReadErrorPreservesMessageFormat(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("  invalid api key  "))}

	err := ReadError(resp)
	if got, want := err.Error(), "provider returned HTTP 401: invalid api key"; got != want {
		t.Fatalf("error text = %q, want %q", got, want)
	}
}
