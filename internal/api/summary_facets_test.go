package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// TestProcessingSummaryAgreesWithCardListForFacetQueries is the assertion that
// would have caught the divergence: the count strip above the library list and
// the cards below it must describe the same set of assets for the same query
// string. Before this change processing-summary ignored the facet parameters,
// so a facet-applied query showed a full-library total above a narrowed list.
func TestProcessingSummaryAgreesWithCardListForFacetQueries(t *testing.T) {
	server := NewServer("", newFacetTestService(t))
	queries := []string{
		"",
		"?asset_type=b_roll",
		"?asset_type=talking_to_camera",
		"?shot_size=wide",
		"?asset_type=b_roll&shot_size=wide",
	}
	for _, query := range queries {
		cards := httptest.NewRecorder()
		server.Handler().ServeHTTP(cards, lanRequest(http.MethodGet, "/api/v1/assets"+query, nil))
		if cards.Code != http.StatusOK {
			t.Fatalf("query=%q cards status=%d body=%s", query, cards.Code, cards.Body.String())
		}
		var got []domain.AssetCard
		if err := json.NewDecoder(cards.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		summary := httptest.NewRecorder()
		server.Handler().ServeHTTP(summary, lanRequest(http.MethodGet, "/api/v1/library/processing-summary"+query, nil))
		if summary.Code != http.StatusOK {
			t.Fatalf("query=%q summary status=%d body=%s", query, summary.Code, summary.Body.String())
		}
		var gotSummary domain.AssetProcessingSummary
		if err := json.NewDecoder(summary.Body).Decode(&gotSummary); err != nil {
			t.Fatal(err)
		}
		if gotSummary.Total != len(got) {
			t.Fatalf("query=%q: summary total=%d but card list has %d cards — the two endpoints disagree", query, gotSummary.Total, len(got))
		}
	}
}

// TestProcessingSummaryDateFromParsing pins L3's decision: an unparseable date
// is the same failure class as an invalid facet value — both would silently
// compile into a WHERE clause that matches nothing and read as "you have no
// footage" — so it must 400, while an empty value stays unset (the browser's
// date input either sends nothing or a valid "2006-01-02" value).
func TestProcessingSummaryDateFromParsing(t *testing.T) {
	server := NewServer("", newFacetTestService(t))
	for _, query := range []string{"?date_from=not-a-date", "?date_from=2026-13-01", "?date_to=never"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/library/processing-summary"+query, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("query=%q status=%d body=%s, want 400", query, response.Code, response.Body.String())
		}
	}
	for _, query := range []string{"", "?date_from=", "?date_to=", "?date_from=2026-07-01"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/library/processing-summary"+query, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("query=%q status=%d body=%s, want 200", query, response.Code, response.Body.String())
		}
	}
}
