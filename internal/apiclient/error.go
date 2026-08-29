// Package apiclient provides shared client-side decoding of Timingdex Hub
// HTTP error envelopes for API consumers that do not live inside the Hub
// process: the MCP client and the Worker runtime. The Hub answers failures
// with a stable JSON envelope —
//
//	{"error":{"code":...,"message":...,"retryable":...,"action":...,"next_retry_at":...}}
//
// — so clients can switch on Code, respect Retryable, and follow Action
// instead of parsing free-form prose. Servers that do not speak the envelope
// still produce a bounded, deterministic error.
package apiclient

import (
	"encoding/json"
	"fmt"
	"strings"
)

// maxBodyExcerpt bounds how much of a non-envelope response body is copied
// into the decoded error. A misbehaving server must not be able to make the
// error text allocate without bound.
const maxBodyExcerpt = 300

// Error is a decoded Timingdex Hub API error. It mirrors the server-side
// envelope so a caller can switch on Code, respect Retryable, and follow
// Action, while Message stays human-readable and NextRetryAt carries the
// server's suggested retry instant (RFC 3339) when one exists.
type Error struct {
	StatusCode  int    `json:"status_code"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	Retryable   bool   `json:"retryable"`
	Action      string `json:"action,omitempty"`
	NextRetryAt string `json:"next_retry_at,omitempty"`
}

// Error returns a stable, human-readable rendering of the decoded failure.
// For an envelope it reads "HTTP <status> <code>: <message> (retryable)"
// followed by any action and next_retry_at; for a non-envelope body it
// returns the bounded status/body message verbatim.
func (e *Error) Error() string {
	if e.Code == "" {
		if e.Message == "" {
			return fmt.Sprintf("HTTP %d", e.StatusCode)
		}
		return e.Message
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d %s", e.StatusCode, e.Code)
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Retryable {
		b.WriteString(" (retryable)")
	}
	if e.Action != "" {
		b.WriteString(" action: ")
		b.WriteString(e.Action)
	}
	if e.NextRetryAt != "" {
		b.WriteString(" next_retry_at: ")
		b.WriteString(e.NextRetryAt)
	}
	return b.String()
}

// DecodeError turns an unsuccessful Hub response into an *Error. When the
// body is the Hub's JSON envelope it fills the fields from it; otherwise it
// returns an *Error with an empty Code and a bounded status/body message
// ("HTTP <status> <statusText>: <first 300 bytes of body>").
//
// The envelope is decoded from the FULL body — callers pass already-bounded
// bodies (the MCP client caps at 1 MiB, the Worker at 4096 bytes) — because
// the Hub can emit an envelope whose serialized form exceeds 300 bytes (a
// message clipped to the 300-byte ceiling plus code/retryable/action/
// next_retry_at), and truncating before unmarshalling would drop the stable
// fields for exactly the long-message failures a client must classify. Only
// the non-envelope fallback message is capped at maxBodyExcerpt, so a
// misbehaving non-envelope server still cannot make the error text allocate
// without bound.
func DecodeError(statusCode int, statusText string, body []byte) error {
	var envelope struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			Retryable   bool   `json:"retryable"`
			Action      string `json:"action"`
			NextRetryAt string `json:"next_retry_at"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Code != "" {
		return &Error{
			StatusCode:  statusCode,
			Code:        envelope.Error.Code,
			Message:     envelope.Error.Message,
			Retryable:   envelope.Error.Retryable,
			Action:      envelope.Error.Action,
			NextRetryAt: envelope.Error.NextRetryAt,
		}
	}
	excerpt := body
	if len(excerpt) > maxBodyExcerpt {
		excerpt = excerpt[:maxBodyExcerpt]
	}
	var message string
	if strings.TrimSpace(statusText) == "" {
		message = fmt.Sprintf("HTTP %d", statusCode)
	} else {
		message = fmt.Sprintf("HTTP %s", statusText)
	}
	if trimmed := strings.TrimSpace(string(excerpt)); trimmed != "" {
		message += ": " + trimmed
	}
	return &Error{StatusCode: statusCode, Message: message}
}
