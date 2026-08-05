package common

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	body := strings.TrimSpace(string(b))
	if len(body) > maxErrorBodyBytes {
		body = body[:maxErrorBodyBytes] + "…(truncated)"
	}
	return &StatusError{StatusCode: resp.StatusCode, Body: body}
}
