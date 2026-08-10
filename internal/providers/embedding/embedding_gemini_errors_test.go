package embedding

import (
	"context"
	"errors"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiEmbeddingErrorFixture(t *testing.T) {
	const key = "embedding-gemini-key"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte(strings.Repeat("failure ", 500) + key))
	}))
	defer s.Close()
	_, err := (&Provider{Endpoint: common.Endpoint{BaseURL: s.URL, APIKey: key}, Protocol: "gemini_embed_content"}).Embed(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("expected error")
	}
	var se *common.StatusError
	if !errors.As(err, &se) || se.HTTPStatusCode() != 502 {
		t.Fatalf("expected status error, got %v", err)
	}
	if strings.Contains(se.Body, key) || strings.Contains(se.Error(), key) || len(se.Body) > 2048+len("…(truncated)") {
		t.Fatalf("leaked or unbounded status body: %q", se.Body)
	}
}
