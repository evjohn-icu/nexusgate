package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeStrictJSON(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		limit  int64
		status int
		code   string
	}{
		{"valid", `{"ok":true}`, 32, 0, ""},
		{"whitespace", "  {\"ok\":true}\n \t", 32, 0, ""},
		{"malformed", `{"ok":`, 32, http.StatusBadRequest, "invalid_request"},
		{"trailing byte", `{"ok":true}x`, 32, http.StatusBadRequest, "invalid_request"},
		{"second value", `{"ok":true}{}`, 32, http.StatusBadRequest, "invalid_request"},
		{"trailing crosses limit", `{"ok":true}xxxxxxxxxxxxxxxxxxxxxxxx`, 12, http.StatusRequestEntityTooLarge, "request_body_too_large"},
		{"oversized first value", `{"long":"xxxxxxxxxxxxxxxx"}`, 8, http.StatusRequestEntityTooLarge, "request_body_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var value map[string]any
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			ok := decodeStrictJSON(w, r, &value, tt.limit)
			if tt.status == 0 {
				if !ok || w.Code != http.StatusOK {
					t.Fatalf("ok=%v status=%d body=%s", ok, w.Code, w.Body.String())
				}
				return
			}
			if ok || w.Code != tt.status {
				t.Fatalf("ok=%v status=%d body=%s, want %d", ok, w.Code, w.Body.String(), tt.status)
			}
			var envelope errorEnvelopeBody
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != tt.code || envelope.Error.Message != "invalid request body" && envelope.Error.Message != "request body too large" {
				t.Fatalf("envelope=%+v", envelope.Error)
			}
		})
	}
}

func TestDecodeOptionalStrictJSONEmptyBody(t *testing.T) {
	var value map[string]any
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(nil))
	w := httptest.NewRecorder()
	present, ok := decodeOptionalStrictJSON(w, r, &value, 8)
	if present || !ok || w.Code != http.StatusOK {
		t.Fatalf("present=%v ok=%v status=%d", present, ok, w.Code)
	}
}

func TestDecodeOptionalStrictJSONTrailingLimit(t *testing.T) {
	var value map[string]any
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"ok":true}xxxxxxxx`))
	w := httptest.NewRecorder()
	_, ok := decodeOptionalStrictJSON(w, r, &value, 12)
	if ok || w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("ok=%v status=%d body=%s, want 413", ok, w.Code, w.Body.String())
	}
	var envelope errorEnvelopeBody
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "request_body_too_large" {
		t.Fatalf("envelope=%+v", envelope.Error)
	}
}

func TestDecodeFailureWritesOneEnvelope(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "single-envelope")
	handler := NewServer("admin-token", service).Handler()
	r := lanRequest(http.MethodPost, "/api/v1/collections", strings.NewReader(`{"name":`))
	r.Header.Set("Authorization", "Bearer "+service.AdminToken())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var envelope errorEnvelopeBody
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "invalid_request" || strings.Count(w.Body.String(), `"error"`) != 1 {
		t.Fatalf("body=%s", w.Body.String())
	}
}
