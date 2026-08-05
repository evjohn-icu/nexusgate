package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHardeningRepurposeCreatePlanRejectsOversizedBody(t *testing.T) {
	service := newHardeningTestService(t)
	handler := NewServer("", service).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans", oversizedBody()))

	if rec.Code == http.StatusOK || rec.Code == http.StatusCreated {
		t.Fatalf("expected rejection for oversized body, got status=%d", rec.Code)
	}
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 400 or 413 for oversized body, got status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHardeningRepurposeRevisePlanRejectsOversizedBody(t *testing.T) {
	service := newHardeningTestService(t)
	handler := NewServer("", service).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans/any-id/revisions", oversizedBody()))

	if rec.Code == http.StatusOK || rec.Code == http.StatusCreated {
		t.Fatalf("expected rejection for oversized body, got status=%d", rec.Code)
	}
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 400 or 413 for oversized body, got status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHardeningRepurposeCreatePlanRejectsOversizedBodyAgent(t *testing.T) {
	service := newHardeningTestService(t)
	if service.AgentToken() == "" {
		t.Skip("agent token not available")
	}
	handler := NewServer("", service).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAgentRequest(service, http.MethodPost, "/api/v1/repurpose/plans", oversizedBody()))

	if rec.Code == http.StatusOK || rec.Code == http.StatusCreated {
		t.Fatalf("expected rejection for oversized body via agent token, got status=%d", rec.Code)
	}
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 400 or 413 for oversized body via agent token, got status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHardeningRepurposeRevisePlanRejectsOversizedBodyAgent(t *testing.T) {
	service := newHardeningTestService(t)
	if service.AgentToken() == "" {
		t.Skip("agent token not available")
	}
	handler := NewServer("", service).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAgentRequest(service, http.MethodPost, "/api/v1/repurpose/plans/any-id/revisions", oversizedBody()))

	if rec.Code == http.StatusOK || rec.Code == http.StatusCreated {
		t.Fatalf("expected rejection for oversized body via agent token, got status=%d", rec.Code)
	}
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 400 or 413 for oversized body via agent token, got status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHardeningRepurposeCreatePlanAcceptsValidBody(t *testing.T) {
	service := newHardeningTestService(t)
	handler := NewServer("", service).Handler()

	body := strings.NewReader(`{"brief":"深圳城市宣传片"}`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hubAdminRequest(service, http.MethodPost, "/api/v1/repurpose/plans", body))

	// The plan may fail to create (no assets, no provider), but it must not
	// be rejected as a body-size error. 400 is the normal failure here, 201
	// would mean the plan was actually generated.
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("valid body was rejected as too large, got status=%d body=%s", rec.Code, rec.Body.String())
	}
}
