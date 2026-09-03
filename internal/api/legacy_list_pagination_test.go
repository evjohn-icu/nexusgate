package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/app"
	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/media"
	"github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
)

// newLegacyListService builds a migrated real-SQLite service for the paged
// list endpoint tests.
func newLegacyListService(t *testing.T, name string) (*app.Service, *sqlite.Repository) {
	t.Helper()
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), name+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service, repo
}

// insertAPITestAsset seeds the minimal assets row FK-referencing test tables
// need, using the repository's own timestamp layout.
func insertAPITestAsset(t *testing.T, repo *sqlite.Repository, id string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, id, id+"-fp", now, now); err != nil {
		t.Fatal(err)
	}
}

// getJSONList performs a LAN trusted-read GET and returns the recorder.
func getJSONList(handler http.Handler, target string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodGet, target, nil))
	return response
}

// assertListPaginationHeaders pins the three X-NexusSlate-* response headers on
// a paged list response.
func assertListPaginationHeaders(t *testing.T, response *httptest.ResponseRecorder, wantLimit, wantOffset int, wantHasMore bool) {
	t.Helper()
	if got := response.Header().Get("X-NexusSlate-Limit"); got != fmt.Sprintf("%d", wantLimit) {
		t.Fatalf("X-NexusSlate-Limit=%q, want %d", got, wantLimit)
	}
	if got := response.Header().Get("X-NexusSlate-Offset"); got != fmt.Sprintf("%d", wantOffset) {
		t.Fatalf("X-NexusSlate-Offset=%q, want %d", got, wantOffset)
	}
	if got := response.Header().Get("X-NexusSlate-Has-More"); got != fmt.Sprintf("%t", wantHasMore) {
		t.Fatalf("X-NexusSlate-Has-More=%q, want %t", got, wantHasMore)
	}
}

func TestLegacyListPagination(t *testing.T) {
	service, repo := newLegacyListService(t, "legacy-paging")
	handler := NewServer("", service).Handler()
	ctx := context.Background()

	// Seed more jobs than one page can hold so pagination is observable.
	const jobTotal = 7
	for i := range jobTotal {
		id := fmt.Sprintf("paged-asset-%d", i)
		insertAPITestAsset(t, repo, id)
		if err := repo.EnqueueJob(ctx, id, domain.JobProbe, fmt.Sprintf("input-%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	const limit = 3
	var jobIDs []string
	offset := 0
	for page := 0; page < jobTotal+2; page++ {
		response := getJSONList(handler, fmt.Sprintf("/api/v1/jobs?limit=%d&offset=%d", limit, offset))
		if response.Code != http.StatusOK {
			t.Fatalf("jobs page %d status=%d body=%s", page, response.Code, response.Body.String())
		}
		assertListPaginationHeaders(t, response, limit, offset, offset+limit < jobTotal)
		var pageJobs []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &pageJobs); err != nil {
			t.Fatal(err)
		}
		if len(pageJobs) > limit {
			t.Fatalf("jobs page %d returned %d rows, want at most %d", page, len(pageJobs), limit)
		}
		for _, job := range pageJobs {
			jobIDs = append(jobIDs, job.ID)
		}
		if offset+limit >= jobTotal {
			break
		}
		offset += limit
	}
	if len(jobIDs) != jobTotal {
		t.Fatalf("jobs concatenated %d ids, want %d", len(jobIDs), jobTotal)
	}
	seen := make(map[string]bool, jobTotal)
	for _, id := range jobIDs {
		if seen[id] {
			t.Fatalf("jobs returned duplicate id %q across pages", id)
		}
		seen[id] = true
	}

	// Same walk over the repurpose-plans endpoint.
	const planTotal = 7
	for i := range planTotal {
		if _, err := repo.SaveRepurposePlan(ctx, domain.RepurposePlan{Brief: fmt.Sprintf("brief %d", i), DurationMS: 1000, Title: fmt.Sprintf("plan %d", i), Status: "draft", Provider: "deterministic", Model: "heuristic-v1", Sections: []domain.PlanSection{{Role: "opening", Query: "x", DurationMS: 1000, Required: true}}}); err != nil {
			t.Fatal(err)
		}
	}
	var planIDs []string
	offset = 0
	for page := 0; page < planTotal+2; page++ {
		response := getJSONList(handler, fmt.Sprintf("/api/v1/repurpose/plans?limit=%d&offset=%d", limit, offset))
		if response.Code != http.StatusOK {
			t.Fatalf("plans page %d status=%d body=%s", page, response.Code, response.Body.String())
		}
		assertListPaginationHeaders(t, response, limit, offset, offset+limit < planTotal)
		var pagePlans []domain.RepurposePlanSummary
		if err := json.Unmarshal(response.Body.Bytes(), &pagePlans); err != nil {
			t.Fatal(err)
		}
		for _, plan := range pagePlans {
			planIDs = append(planIDs, plan.ID)
		}
		if offset+limit >= planTotal {
			break
		}
		offset += limit
	}
	if len(planIDs) != planTotal {
		t.Fatalf("plans concatenated %d ids, want %d", len(planIDs), planTotal)
	}
	seenPlans := make(map[string]bool, planTotal)
	for _, id := range planIDs {
		if seenPlans[id] {
			t.Fatalf("plans returned duplicate id %q across pages", id)
		}
		seenPlans[id] = true
	}
}

func TestLegacyListPaginationHeaders(t *testing.T) {
	service, repo := newLegacyListService(t, "legacy-paging-headers")
	handler := NewServer("", service).Handler()
	ctx := context.Background()
	for i := range 5 {
		id := fmt.Sprintf("header-asset-%d", i)
		insertAPITestAsset(t, repo, id)
		if err := repo.EnqueueJob(ctx, id, domain.JobProbe, fmt.Sprintf("h-%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	// One full page plus a probe row: has-more is true.
	first := getJSONList(handler, "/api/v1/jobs?limit=2&offset=0")
	if first.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", first.Code, first.Body.String())
	}
	assertListPaginationHeaders(t, first, 2, 0, true)
	// A page ending exactly at the set boundary has no more rows.
	last := getJSONList(handler, "/api/v1/jobs?limit=2&offset=4")
	if last.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", last.Code, last.Body.String())
	}
	assertListPaginationHeaders(t, last, 2, 4, false)
}

func TestLegacyListPaginationRejectsMalformedParameters(t *testing.T) {
	service, _ := newLegacyListService(t, "legacy-paging-invalid")
	handler := NewServer("", service).Handler()
	cases := []struct {
		target string
	}{
		{"/api/v1/jobs?limit=abc"},
		{"/api/v1/jobs?limit=-1"},
		{"/api/v1/jobs?offset=-3"},
		{"/api/v1/assets?limit=banana"},
		{"/api/v1/repurpose/plans?offset=-1"},
		{"/api/v1/tags/unresolved?limit=nope"},
		{"/api/v1/tags/proposals?limit=-5"},
		{"/api/v1/shoot-sessions?limit=x"},
		{"/api/v1/shoot-sessions?date_from=not-a-date"},
		{"/api/v1/shoot-sessions?date_to=13-99-2026"},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			response := getJSONList(handler, tc.target)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", response.Code, response.Body.String())
			}
			if apiErr := decodeErrorEnvelope(t, response); apiErr.Code != "invalid_request" {
				t.Fatalf("code=%q, want invalid_request", apiErr.Code)
			}
		})
	}
	// An explicit limit above the cap still clamps instead of erroring.
	response := getJSONList(handler, "/api/v1/jobs?limit=99999")
	if response.Code != http.StatusOK {
		t.Fatalf("clamped limit status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	assertListPaginationHeaders(t, response, 500, 0, false)
}

func TestParentChildResourceSemantics(t *testing.T) {
	service, repo := newLegacyListService(t, "parent-child")
	handler := NewServer("", service).Handler()
	ctx := context.Background()
	insertAPITestAsset(t, repo, "known-asset")
	// A collection is a saved filter: a region no asset carries makes this
	// known collection match zero assets, which is the "known empty parent"
	// case the endpoint must answer with 200 [].
	if _, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{ID: "known-collection", Name: "空篮", Filter: domain.AssetCollectionFilter{RegionLabel: "no-such-region"}, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// Unknown asset id: generic 404 with a check-the-identifier action.
	unknownAsset := getJSONList(handler, "/api/v1/assets/missing/shots")
	if unknownAsset.Code != http.StatusNotFound {
		t.Fatalf("unknown asset status=%d body=%s, want 404", unknownAsset.Code, unknownAsset.Body.String())
	}
	apiErr := decodeErrorEnvelope(t, unknownAsset)
	if apiErr.Code != "not_found" || apiErr.Action != "check_the_identifier" {
		t.Fatalf("unknown asset error=%+v, want not_found/check_the_identifier", apiErr)
	}

	// Known asset with zero canonical shots: 200 with an empty array.
	knownAsset := getJSONList(handler, "/api/v1/assets/known-asset/shots")
	if knownAsset.Code != http.StatusOK {
		t.Fatalf("known empty asset status=%d body=%s, want 200", knownAsset.Code, knownAsset.Body.String())
	}
	if !strings.Contains(knownAsset.Body.String(), "[]") || strings.Contains(knownAsset.Body.String(), "null") {
		t.Fatalf("known empty asset body=%s, want JSON array", knownAsset.Body.String())
	}

	// Unknown collection id: generic 404.
	unknownCollection := getJSONList(handler, "/api/v1/collections/missing/assets")
	if unknownCollection.Code != http.StatusNotFound {
		t.Fatalf("unknown collection status=%d body=%s, want 404", unknownCollection.Code, unknownCollection.Body.String())
	}
	collectionErr := decodeErrorEnvelope(t, unknownCollection)
	if collectionErr.Code != "not_found" || collectionErr.Action != "check_the_identifier" {
		t.Fatalf("unknown collection error=%+v, want not_found/check_the_identifier", collectionErr)
	}

	// Known collection with zero assets: 200 with an empty array.
	knownCollection := getJSONList(handler, "/api/v1/collections/known-collection/assets")
	if knownCollection.Code != http.StatusOK {
		t.Fatalf("known empty collection status=%d body=%s, want 200", knownCollection.Code, knownCollection.Body.String())
	}
	if !strings.Contains(knownCollection.Body.String(), "[]") || strings.Contains(knownCollection.Body.String(), "null") {
		t.Fatalf("known empty collection body=%s, want JSON array", knownCollection.Body.String())
	}
}

func TestSuccessfulListsEncodeEmptyArrays(t *testing.T) {
	service, repo := newLegacyListService(t, "empty-arrays")
	handler := NewServer("", service).Handler()
	insertAPITestAsset(t, repo, "known-asset")

	for _, target := range []string{
		"/api/v1/jobs",
		"/api/v1/assets/known-asset/shots",
		"/api/v1/tags",
		"/api/v1/tags/unresolved",
		"/api/v1/tags/proposals",
		"/api/v1/repurpose/plans",
	} {
		t.Run(target, func(t *testing.T) {
			response := getJSONList(handler, target)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", response.Code, response.Body.String())
			}
			body := strings.TrimSpace(response.Body.String())
			if !strings.Contains(body, "[]") || strings.Contains(body, "null") {
				t.Fatalf("body=%s, want JSON array with no null", body)
			}
		})
	}
}
