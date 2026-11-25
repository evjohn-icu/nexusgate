package common

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestSameOriginClassification(t *testing.T) {
	tests := []struct {
		name   string
		first  string
		second string
		want   bool
	}{
		{"identical origin ignores path and query", "https://provider.example/v1/x", "https://provider.example/other?x=1", true},
		{"case-insensitive scheme and host", "HTTPS://PROVIDER.Example", "https://provider.example", true},
		{"default port matches explicit", "https://provider.example", "https://provider.example:443", true},
		{"http default port matches explicit", "http://provider.example", "http://provider.example:80", true},
		{"zero-padded port normalizes", "http://provider.example:0080", "http://provider.example:80", true},
		{"different ports differ", "https://provider.example:8443", "https://provider.example", false},
		{"scheme differs", "https://provider.example", "http://provider.example", false},
		{"host differs", "https://provider.example", "https://other.example", false},
		{"userinfo ignored", "https://user:pass@provider.example", "https://provider.example", true},
		{"ftp is never same origin", "ftp://provider.example", "ftp://provider.example", false},
		{"data urls are never same origin", "data:text/plain,x", "data:text/plain,x", false},
		{"missing host is never same origin", "https:///path", "https:///path", false},
		{"out-of-range port is never valid", "https://provider.example:70000", "https://provider.example:70000", false},
		{"zero port is never valid", "http://provider.example:0", "http://provider.example:0", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameOrigin(mustParseURL(t, tc.first), mustParseURL(t, tc.second)); got != tc.want {
				t.Fatalf("SameOrigin(%q, %q) = %t, want %t", tc.first, tc.second, got, tc.want)
			}
		})
	}
	if SameOrigin(nil, mustParseURL(t, "https://provider.example")) {
		t.Fatal("nil URL must never be same origin")
	}
	if SameOrigin(mustParseURL(t, "https://provider.example"), nil) {
		t.Fatal("nil URL must never be same origin")
	}
}

// A credentialed client must never deliver its authentication to a redirect
// target on another origin: the policy returns the redirect response to the
// caller instead of replaying the request with its headers.
func TestWithSameOriginRedirectsRefusesCrossOriginHop(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("cross-origin target received Authorization header")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			w.Header().Set("Location", other.URL+"/target")
			w.WriteHeader(http.StatusFound)
		case "/local":
			w.Header().Set("Location", "/local/target")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer origin.Close()

	client := WithSameOriginRedirects(&http.Client{Timeout: 5 * time.Second})
	req, err := http.NewRequest(http.MethodGet, origin.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-test-credential-42")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("cross-origin redirect status = %d, want the unfollowed 302", resp.StatusCode)
	}

	// Same origin keeps following.
	req, err = http.NewRequest(http.MethodGet, origin.URL+"/local", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-test-credential-42")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("Do same-origin: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same-origin redirect status = %d, want 200", resp.StatusCode)
	}
}

func TestWithSameOriginRedirectsDelegatesExistingPolicyAndKeepsDefaults(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/loop")
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()

	// A client without its own policy keeps the standard 10-hop limit.
	plain := WithSameOriginRedirects(&http.Client{})
	resp, err := plain.Get(origin.URL + "/loop")
	if err == nil {
		resp.Body.Close()
		t.Fatal("self-redirect loop must stop")
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("error = %v, want the net/http default limit", err)
	}

	// An existing policy is delegated after the origin check.
	base := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return errors.New("caller policy consulted")
	}}
	wrapped := WithSameOriginRedirects(base)
	resp, err = wrapped.Get(origin.URL + "/loop")
	if err == nil {
		resp.Body.Close()
		t.Fatal("redirect must reach the caller policy")
	}
	if !strings.Contains(err.Error(), "caller policy consulted") {
		t.Fatalf("error = %v, want delegation to the caller policy", err)
	}
	if base.CheckRedirect == nil {
		t.Fatal("the wrapped policy must not mutate the original client")
	}
}

func TestWithSameOriginRedirectsPreservesClientSettings(t *testing.T) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	base := &http.Client{Transport: transport, Timeout: 7 * time.Second}
	wrapped := WithSameOriginRedirects(base)
	if wrapped.Timeout != 7*time.Second {
		t.Fatalf("timeout = %v, want preserved", wrapped.Timeout)
	}
	if wrapped.Transport != transport {
		t.Fatal("transport must be preserved")
	}
	if wrapped == base {
		t.Fatal("the original client must not be mutated")
	}
}

func TestEndpointClientAppliesSameOriginPolicy(t *testing.T) {
	if (Endpoint{}).Client().CheckRedirect == nil {
		t.Fatal("default endpoint client must carry the same-origin policy")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	supplied := &http.Client{Transport: transport}
	wrapped := (Endpoint{HTTPClient: supplied}).Client()
	if wrapped.CheckRedirect == nil {
		t.Fatal("caller-supplied client must also carry the same-origin policy")
	}
	if wrapped.Transport != transport {
		t.Fatal("caller-supplied transport must be preserved")
	}
	if supplied.CheckRedirect != nil {
		t.Fatal("the original supplied client must not be mutated")
	}
}
