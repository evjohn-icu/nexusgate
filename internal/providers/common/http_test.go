package common

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
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

// The message format is relied upon by existing provider tests, and it is what
// reaches jobs.last_error_message and the /progress page. Nothing classifies on
// it any more — retry decisions read the status through errors.As and the
// domain.ErrPermanentFailure marker — so this test pins what an operator reads,
// not what the code branches on.
func TestReadErrorPreservesMessageFormat(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("  invalid api key  "))}

	err := ReadError(resp)
	if got, want := err.Error(), "provider returned HTTP 401: invalid api key"; got != want {
		t.Fatalf("error text = %q, want %q", got, want)
	}
}

// Callers outside this package classify a failure by probing a wrapped error
// for a status-bearing interface, because providerpool deliberately depends on
// no transport package. StatusCode is a field, so the method is the only thing
// that probe can find; deleting it still compiles and silently sends every
// provider failure back to substring matching on the message.
// APIKey must be excluded from JSON serialisation to avoid leaking a provider
// key through logs, debug output or configuration export. The field is populated
// at runtime from secretstore, never from a JSON config block.
func TestEndpointAPIKeyNotExposedInJSON(t *testing.T) {
	ep := Endpoint{
		BaseURL: "https://api.example.com",
		APIKey:  "sk-secret-key-that-must-not-leak",
	}
	data, err := json.Marshal(ep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-secret") || strings.Contains(string(data), "api_key") {
		t.Fatalf("APIKey must not appear in JSON output, got: %s", data)
	}
}

func TestStatusErrorExposesStatusThroughAnInterface(t *testing.T) {
	wrapped := fmt.Errorf("analyze: %w", &StatusError{StatusCode: http.StatusPaymentRequired, Body: "insufficient balance"})

	var probe interface{ HTTPStatusCode() int }
	if !errors.As(wrapped, &probe) {
		t.Fatal("a wrapped *StatusError is invisible to a status probe")
	}
	if got := probe.HTTPStatusCode(); got != http.StatusPaymentRequired {
		t.Fatalf("HTTPStatusCode() = %d, want 402", got)
	}
}

func TestEndpointURLJoinsBaseAndPath(t *testing.T) {
	ep := Endpoint{BaseURL: "https://nas:8787/"}
	for _, tc := range []struct{ path, want string }{
		{"api/v1/scan", "https://nas:8787/api/v1/scan"},
		{"/api/v1/scan", "https://nas:8787/api/v1/scan"},
		{"", "https://nas:8787/"},
	} {
		if got := ep.URL(tc.path); got != tc.want {
			t.Fatalf("URL(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestEndpointClientTimeout(t *testing.T) {
	if got := (Endpoint{}).Client().Timeout; got != 120*time.Second {
		t.Fatalf("default client timeout = %v, want 120s", got)
	}
	if got := (Endpoint{TimeoutSeconds: 5}).Client().Timeout; got != 5*time.Second {
		t.Fatalf("custom client timeout = %v, want 5s", got)
	}
}

func TestEndpointNewRequestAuthAndHeaders(t *testing.T) {
	ctx := context.Background()
	t.Run("default bearer auth", func(t *testing.T) {
		req, err := (Endpoint{BaseURL: "https://h", APIKey: "k"}).NewRequest(ctx, "POST", "p", map[string]string{"a": "b"})
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer k" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer k")
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		if req.Body == nil {
			t.Fatal("POST with body must set req.Body")
		}
	})
	t.Run("raw scheme sends key verbatim", func(t *testing.T) {
		req, err := (Endpoint{BaseURL: "https://h", APIKey: "k", AuthScheme: "raw"}).NewRequest(ctx, "GET", "p", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "k" {
			t.Fatalf("Authorization = %q, want %q", got, "k")
		}
		if req.Body != nil {
			t.Fatal("GET with nil body must not set req.Body")
		}
	})
	t.Run("custom header and extra headers", func(t *testing.T) {
		req, err := (Endpoint{BaseURL: "https://h", APIKey: "k", AuthHeader: "X-Key", ExtraHeaders: map[string]string{"X-Foo": "bar"}}).NewRequest(ctx, "GET", "p", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("X-Key"); got != "Bearer k" {
			t.Fatalf("X-Key = %q, want %q", got, "Bearer k")
		}
		if got := req.Header.Get("X-Foo"); got != "bar" {
			t.Fatalf("X-Foo = %q, want bar", got)
		}
	})
}

func TestReadErrorSmallBodyUntruncated(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader("boom"))}
	err := ReadError(resp)
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("ReadError() = %T, want *StatusError", err)
	}
	if se.StatusCode != http.StatusBadGateway || se.Body != "boom" {
		t.Fatalf("StatusError = %d/%q, want 502/boom", se.StatusCode, se.Body)
	}
	if strings.Contains(se.Error(), "boom") == false {
		t.Fatalf("Error() = %q, want it to carry the body", se.Error())
	}
}

func TestReadErrorWithSecretStatusMatrix(t *testing.T) {
	const secret = "sk-echoed-key"
	for _, code := range []int{400, 401, 402, 403, 404, 408, 429, 500, 502, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			body := strings.Repeat("x", maxErrorBodyBytes) + secret + strings.Repeat("y", maxErrorBodyBytes)
			err := ReadErrorWithSecret(&http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}, secret)
			var status *StatusError
			if !errors.As(err, &status) || status.StatusCode != code {
				t.Fatalf("status = %+v, err=%v", status, err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(status.Body, secret) {
				t.Fatalf("secret leaked: %v / %q", err, status.Body)
			}
			if len(err.Error()) > maxErrorBodyBytes+128 {
				t.Fatalf("error was not bounded: %d", len(err.Error()))
			}
		})
	}
}

type testNetError struct{}

func (testNetError) Error() string   { return "network secret" }
func (testNetError) Timeout() bool   { return true }
func (testNetError) Temporary() bool { return true }

func TestRedactErrorPreservesClassification(t *testing.T) {
	secret := "secret"
	for _, tc := range []struct {
		name string
		in   error
		is   error
		as   func(error) bool
	}{
		{"cancel", context.Canceled, context.Canceled, nil},
		{"deadline", context.DeadlineExceeded, context.DeadlineExceeded, nil},
		{"eof", io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, nil},
		{"network", testNetError{}, nil, func(err error) bool { var n net.Error; return errors.As(err, &n) && n.Timeout() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RedactError(fmt.Errorf("wrapped: %w", tc.in), secret)
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("errors.Is(%v) = false", tc.is)
			}
			if tc.as != nil && !tc.as(err) {
				t.Fatal("errors.As classification was lost")
			}
			if errors.Unwrap(err) != nil {
				t.Fatal("redacted error exposes its cause")
			}
		})
	}
}
