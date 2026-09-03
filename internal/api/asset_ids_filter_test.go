package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/app"
	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/media"
	"github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
)

// newAssetIDsTestService seeds n plain assets (no analysis needed — these
// tests exercise the ids= id-list filter, not facets) and returns the
// service alongside their real asset ids in creation order, so a test can
// build an ids= query value against ids that actually exist.
func newAssetIDsTestService(t *testing.T, n int) (*app.Service, []string) {
	t.Helper()
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "asset-ids-handler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootPath := t.TempDir()
	root, err := repo.CreateLibraryRoot(ctx, rootPath)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, n)
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		relPath := "clip-" + strconv.Itoa(i) + ".mp4"
		absPath := filepath.Join(rootPath, relPath)
		if err := os.WriteFile(absPath, []byte("fixture-"+relPath), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(absPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.UpsertScannedFile(ctx, root, relPath, absPath, info, "fp-"+relPath); err != nil {
			t.Fatal(err)
		}
		assets, err := repo.ListAssets(ctx, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range assets {
			if !seen[a.ID] {
				seen[a.ID] = true
				ids = append(ids, a.ID)
				break
			}
		}
	}
	if len(ids) != n {
		t.Fatalf("seeded %d assets, want %d", len(ids), n)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service, ids
}

// TestListAssetCardsHandlerFiltersByIDs is the O2 wiring proof: a
// /api/v1/search hit list can be rendered by asking for exactly those ids
// instead of pulling a capped card listing and intersecting client-side.
func TestListAssetCardsHandlerFiltersByIDs(t *testing.T) {
	service, ids := newAssetIDsTestService(t, 3)
	server := NewServer("", service)
	response := httptest.NewRecorder()
	target := "/api/v1/assets?ids=" + ids[0] + "," + ids[2]
	server.Handler().ServeHTTP(response, lanRequest(http.MethodGet, target, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var cards []domain.AssetCard
	if err := json.NewDecoder(response.Body).Decode(&cards); err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("cards=%+v, want exactly the 2 requested ids", cards)
	}
	got := map[string]bool{}
	for _, c := range cards {
		got[c.ID] = true
	}
	if !got[ids[0]] || !got[ids[2]] {
		t.Fatalf("cards=%+v, want ids %s and %s", cards, ids[0], ids[2])
	}
	if got[ids[1]] {
		t.Fatalf("cards=%+v, must not include the id that was not requested", cards)
	}
}

// TestListAssetCardsHandlerRejectsTooManyIDs pins the "bound it and say so"
// requirement: an over-cap id list is a 400 naming the limit, not a
// truncation that silently drops the tail — a silently dropped id is the
// exact failure this endpoint exists to remove.
func TestListAssetCardsHandlerRejectsTooManyIDs(t *testing.T) {
	service, _ := newAssetIDsTestService(t, 1)
	server := NewServer("", service)
	many := make([]string, 201)
	for i := range many {
		many[i] = "id-" + strconv.Itoa(i)
	}
	response := httptest.NewRecorder()
	target := "/api/v1/assets?ids=" + strings.Join(many, ",")
	server.Handler().ServeHTTP(response, lanRequest(http.MethodGet, target, nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "200") {
		t.Fatalf("body=%q, want it to name the 200 id limit", response.Body.String())
	}
}

// TestListAssetCardsHandlerTreatsEmptyIDsAsUnset: an empty ids= value means
// unset (match every asset, same as omitting the parameter), not "match
// nothing" — a client that computed zero search hits is expected to skip
// calling this endpoint rather than send ids= empty.
func TestListAssetCardsHandlerTreatsEmptyIDsAsUnset(t *testing.T) {
	service, ids := newAssetIDsTestService(t, 2)
	server := NewServer("", service)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/assets?ids=", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var cards []domain.AssetCard
	if err := json.NewDecoder(response.Body).Decode(&cards); err != nil {
		t.Fatal(err)
	}
	if len(cards) != len(ids) {
		t.Fatalf("cards=%d, want all %d seeded assets (empty ids means unset)", len(cards), len(ids))
	}
}

// TestSearchHandlerRejectsInvalidFacetValue: /api/v1/search now calls
// parseFacetFilter exactly like its three siblings (search/shots,
// search/shots/hybrid, similar), so a bad facet value 400s naming the field
// instead of silently searching unfiltered.
func TestSearchHandlerRejectsInvalidFacetValue(t *testing.T) {
	server := NewServer("", newFacetTestService(t))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/search?q=test&shot_size=nonsense", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "shot_size") {
		t.Fatalf("body=%q, want it to name the shot_size field", response.Body.String())
	}
}

// TestSearchHandlerAppliesFacetFilter is the behavioral half: a facet that
// matches only one of newFacetTestService's two fixtures must narrow
// /api/v1/search's ids to that one asset, proving the handler actually wires
// facets into Service.SearchFiltered rather than only validating and
// discarding them.
func TestSearchHandlerAppliesFacetFilter(t *testing.T) {
	server := NewServer("", newFacetTestService(t))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, lanRequest(http.MethodGet, "/api/v1/search?q=handler+facet+fixture&shot_size=wide", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var ids []string
	if err := json.NewDecoder(response.Body).Decode(&ids); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("ids=%v, want exactly the wide/b_roll fixture", ids)
	}
}
