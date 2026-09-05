package embedding

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/providers/common"
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

func TestOpenAIEmbeddingRejectsInvalidResponses(t *testing.T) {
	cases := map[string]string{
		"duplicate index": `{"data":[{"index":0,"embedding":[1,0]},{"index":0,"embedding":[0,1]}]}`,
		"missing index":   `{"data":[{"index":0,"embedding":[1,0]},{"index":2,"embedding":[0,1]}]}`,
		"negative index":  `{"data":[{"index":-1,"embedding":[1,0]},{"index":1,"embedding":[0,1]}]}`,
		"zero vector":     `{"data":[{"index":0,"embedding":[0,0]},{"index":1,"embedding":[0,0]}]}`,
		"nan vector":      `{"data":[{"index":0,"embedding":[1,NaN]},{"index":1,"embedding":[0,1]}]}`,
		"infinite vector": `{"data":[{"index":0,"embedding":[1,Infinity]},{"index":1,"embedding":[0,1]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(w, strings.NewReader(body))
			}))
			defer server.Close()
			provider := &Provider{Endpoint: common.Endpoint{BaseURL: server.URL}, ModelName: "test"}
			if _, err := provider.Embed(context.Background(), []string{"a", "b"}); err == nil {
				t.Fatal("invalid response must be rejected")
			}
		})
	}
}
