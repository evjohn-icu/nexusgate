package repurpose

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderParsesStructuredPlan(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"{\"title\":\"城市生活\",\"sections\":[{\"role\":\"opening\",\"query\":\"城市夜景\",\"duration_ms\":5000,\"required\":true}] }"}}]}`)),
			Header:     make(http.Header),
		}, nil
	})}
	provider := &Provider{Endpoint: common.Endpoint{BaseURL: "http://planner.test/v1", HTTPClient: client}, ModelName: "small-model"}
	draft, err := provider.Plan(context.Background(), domain.RepurposeBrief{Brief: "深圳城市生活"})
	if err != nil {
		t.Fatal(err)
	}
	if draft.Title != "城市生活" || len(draft.Sections) != 1 || draft.Sections[0].Query != "城市夜景" {
		t.Fatalf("draft=%+v", draft)
	}
}

func TestProviderRejectsInvalidSection(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"{\"title\":\"bad\",\"sections\":[{\"role\":\"opening\",\"query\":\"\",\"duration_ms\":0}] }"}}]}`)),
			Header:     make(http.Header),
		}, nil
	})}
	provider := &Provider{Endpoint: common.Endpoint{BaseURL: "http://planner.test/v1", HTTPClient: client}}
	if _, err := provider.Plan(context.Background(), domain.RepurposeBrief{Brief: "test"}); err == nil {
		t.Fatal("expected invalid planner section error")
	}
}
