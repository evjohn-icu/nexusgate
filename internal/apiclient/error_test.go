package apiclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestApiclientDecodeError(t *testing.T) {
	t.Run("envelope fills every stable field", func(t *testing.T) {
		err := DecodeError(http.StatusServiceUnavailable, "503 Service Unavailable",
			[]byte(`{"error":{"code":"provider_route_exhausted","message":"every configured provider key on this route is failing","retryable":true,"action":"wait_or_change_provider","next_retry_at":"2026-08-09T12:00:00Z"}}`))
		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("DecodeError returned %T, want *Error", err)
		}
		if apiErr.StatusCode != http.StatusServiceUnavailable ||
			apiErr.Code != "provider_route_exhausted" ||
			apiErr.Message != "every configured provider key on this route is failing" ||
			!apiErr.Retryable ||
			apiErr.Action != "wait_or_change_provider" ||
			apiErr.NextRetryAt != "2026-08-09T12:00:00Z" {
			t.Fatalf("envelope fields = %+v", apiErr)
		}
		stable := apiErr.Error()
		for _, want := range []string{
			"HTTP 503 provider_route_exhausted",
			"every configured provider key on this route is failing",
			"retryable",
			"wait_or_change_provider",
			"next_retry_at: 2026-08-09T12:00:00Z",
		} {
			if !strings.Contains(stable, want) {
				t.Fatalf("Error() = %q, missing %q", stable, want)
			}
		}
	})

	t.Run("non-envelope keeps an empty code and bounded status/body message", func(t *testing.T) {
		err := DecodeError(http.StatusInternalServerError, "500 Internal Server Error", []byte("boom"))
		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("DecodeError returned %T, want *Error", err)
		}
		if apiErr.Code != "" || apiErr.StatusCode != http.StatusInternalServerError || !strings.Contains(apiErr.Message, "boom") {
			t.Fatalf("non-envelope error = %+v", apiErr)
		}
		if got := apiErr.Error(); got != "HTTP 500 Internal Server Error: boom" {
			t.Fatalf("non-envelope Error() = %q", got)
		}
	})

	t.Run("long non-envelope body is capped", func(t *testing.T) {
		long := strings.Repeat("x", 5000)
		err := DecodeError(http.StatusBadGateway, "502 Bad Gateway", []byte(long))
		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("DecodeError returned %T, want *Error", err)
		}
		if strings.Count(apiErr.Message, "x") != maxBodyExcerpt {
			t.Fatalf("body excerpt length = %d, want %d", strings.Count(apiErr.Message, "x"), maxBodyExcerpt)
		}
	})

	t.Run("json that is not an envelope stays a bounded body", func(t *testing.T) {
		err := DecodeError(http.StatusConflict, "409 Conflict", []byte(`{"detail":"not an envelope"}`))
		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("DecodeError returned %T, want *Error", err)
		}
		if apiErr.Code != "" || apiErr.Message != `HTTP 409 Conflict: {"detail":"not an envelope"}` {
			t.Fatalf("non-envelope JSON error = %+v", apiErr)
		}
	})

	t.Run("empty body keeps a bare status message", func(t *testing.T) {
		err := DecodeError(http.StatusForbidden, "403 Forbidden", nil)
		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("DecodeError returned %T, want *Error", err)
		}
		if got := apiErr.Error(); got != "HTTP 403 Forbidden" {
			t.Fatalf("empty-body Error() = %q", got)
		}
	})

	t.Run("envelope marshals to stable JSON fields", func(t *testing.T) {
		apiErr := &Error{
			StatusCode:  http.StatusServiceUnavailable,
			Code:        "provider_route_exhausted",
			Message:     "every configured provider key on this route is failing",
			Retryable:   true,
			Action:      "wait_or_change_provider",
			NextRetryAt: "2026-08-09T12:00:00Z",
		}
		raw, err := json.Marshal(apiErr)
		if err != nil {
			t.Fatal(err)
		}
		var emitted map[string]any
		if err := json.Unmarshal(raw, &emitted); err != nil {
			t.Fatal(err)
		}
		if emitted["status_code"] != float64(http.StatusServiceUnavailable) ||
			emitted["code"] != "provider_route_exhausted" ||
			emitted["message"] == "" ||
			emitted["retryable"] != true ||
			emitted["action"] != "wait_or_change_provider" ||
			emitted["next_retry_at"] != "2026-08-09T12:00:00Z" {
			t.Fatalf("marshaled fields = %v", emitted)
		}
	})
}
