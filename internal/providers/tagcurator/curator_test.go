package tagcurator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/providers/common"
)

func TestCurateValidatesOpenAICompatibleDraft(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization=%q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "tiny-model" {
			t.Fatalf("model=%v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"groups\":[{\"canonical_name\":\"urban_night\",\"aliases\":[\"城市夜景\",\"night_city\",\"invented_tag\"],\"category\":\"scene\",\"confidence\":0.91,\"reason\":\"same scene\"}]}"}}]}`))
	}))
	defer server.Close()

	provider := &Provider{
		Endpoint:  common.Endpoint{BaseURL: server.URL + "/v1", APIKey: "test-key"},
		ModelName: "tiny-model",
	}
	proposals, err := provider.Curate(context.Background(), []domain.UnresolvedTag{
		{NormalizedTag: "城市夜景", AssetCount: 2},
		{NormalizedTag: "night_city", AssetCount: 1},
	}, []domain.CanonicalTag{{ID: "tag-1", CanonicalName: "urban_night"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 {
		t.Fatalf("proposals=%d", len(proposals))
	}
	got := proposals[0]
	if got.ProposalType != "add_aliases" || got.AffectedAssets != 3 {
		t.Fatalf("proposal=%+v", got)
	}
	aliases, ok := got.Payload["aliases"].([]string)
	if !ok {
		t.Fatalf("aliases type=%T", got.Payload["aliases"])
	}
	if len(aliases) != 2 || aliases[0] != "night_city" || aliases[1] != "城市夜景" {
		t.Fatalf("aliases=%v", aliases)
	}
}

func TestSummarizeLibraryFixture(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"素材以城市夜景为主。\",\"themes\":[\"城市夜景\"],\"suitable_for\":[\"城市氛围 B-roll\"]}"}}]}`))
	}))
	defer server.Close()
	provider := &Provider{Endpoint: common.Endpoint{BaseURL: server.URL + "/v1"}, ModelName: "tiny-model"}
	draft, err := provider.SummarizeLibrary(context.Background(), domain.LibrarySummaryInput{AssetCount: 10, AnalyzedCount: 8, TopTags: []domain.LibraryTagStat{{CanonicalName: "urban_night", AssetCount: 5}}})
	if err != nil {
		t.Fatal(err)
	}
	if draft.Summary == "" || len(draft.Themes) != 1 || len(draft.SuitableFor) != 1 {
		t.Fatalf("draft=%+v", draft)
	}
}
