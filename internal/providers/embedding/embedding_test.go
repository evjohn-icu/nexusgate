package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ev/timingdex/internal/providers/common"
)

func TestOpenAICompatibleEmbeddingFixture(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization=%q", got)
		}
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "embed-test" || len(body.Input) != 2 {
			t.Fatalf("body=%+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[0.9,0.1]}]}`))
	}))
	defer server.Close()
	provider := &Provider{Endpoint: common.Endpoint{BaseURL: server.URL + "/v1", APIKey: "test-key"}, ModelName: "embed-test"}
	vectors, err := provider.Embed(context.Background(), []string{"night city", "urban night"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 2 || len(vectors[1]) != 2 {
		t.Fatalf("vectors=%v", vectors)
	}
}
