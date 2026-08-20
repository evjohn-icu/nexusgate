package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// issuesRetryFixture opens a repository and service with one asset and
// returns the handler along with the ids of three failed jobs: one terminal
// on provider_quota, one terminal on provider_auth, and one failed with no
// code at all (the 'unknown' bucket). A fourth job succeeds and must never
// move. Priorities are descending so each LeaseNextJob picks the job enqueued
// for that step deterministically, and the uncoded job is failed last so a
// later lease cannot re-pick a non-terminal failed job.
func issuesRetryFixture(t *testing.T) (http.Handler, *app.Service, map[string]string) {
	t.Helper()
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "issues-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assetID := scanOneAsset(t, repo, "issues-clip.mov")
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range []struct {
		priority int
		hash     string
	}{
		{30, "hash-done"},
		{20, "hash-quota"},
		{10, "hash-auth"},
		{0, "hash-uncoded"},
	} {
		if err := repo.EnqueueJob(ctx, assetID, domain.JobAnalyze, job.hash, job.priority); err != nil {
			t.Fatal(err)
		}
	}

	ids := map[string]string{}
	// The succeeded job is leased and completed first: a failed non-terminal
	// job is itself leasable (the queue retries it), so anything failed before
	// the last lease would be re-picked instead of the intended row.
	done, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
	if err != nil || done == nil {
		t.Fatalf("lease done job=%+v err=%v", done, err)
	}
	if err := repo.CompleteJob(ctx, done.ID, "worker", domain.JobSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	ids["hash-done"] = done.ID
	for _, job := range []struct {
		hash string
		step func(string) error
	}{
		{"hash-quota", func(id string) error {
			return repo.FailJobTerminally(ctx, id, "worker", domain.JobFailureCategoryProviderQuota, "monthly quota exhausted")
		}},
		{"hash-auth", func(id string) error {
			return repo.FailJobTerminally(ctx, id, "worker", domain.JobFailureCategoryProviderAuth, "key rejected")
		}},
		{"hash-uncoded", func(id string) error {
			// CompleteJob fails without a category code: last_error_code stays
			// NULL, which is exactly the bucket JobIssues reports as 'unknown'.
			return repo.CompleteJob(ctx, id, "worker", domain.JobFailed, "predates classification")
		}},
	} {
		leased, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
		if err != nil || leased == nil {
			t.Fatalf("lease job=%+v err=%v", leased, err)
		}
		if err := job.step(leased.ID); err != nil {
			t.Fatal(err)
		}
		ids[job.hash] = leased.ID
	}
	return NewServer("", service).Handler(), service, ids
}

// jobStatesAfter reads the job list the way the progress page does and maps
// id -> state so an assertion reads as state, not as SQL.
func jobStatesAfter(t *testing.T, handler http.Handler, service *app.Service) map[string]string {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodGet, "/api/v1/jobs?limit=100", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var jobs []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	states := make(map[string]string, len(jobs))
	for _, job := range jobs {
		states[job.ID] = job.State
	}
	return states
}

// POST /api/v1/pipeline/retry-failed with a category must revive only the
// failed jobs that carry that code; an empty body keeps the historical
// retry-everything behavior; the unknown category must reach the NULL-code
// rows; and the whole route stays behind the admin token.
func TestRetryFailedJobsByCategory(t *testing.T) {
	handler, service, ids := issuesRetryFixture(t)

	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, lanRequest(http.MethodPost, "/api/v1/pipeline/retry-failed", strings.NewReader(`{"category":"provider_quota"}`)))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d, want 401", unauthenticated.Code)
	}

	post := func(body string) map[string]int {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/pipeline/retry-failed", strings.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var result map[string]int
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	// Category-scoped: only the provider_quota job moves.
	if result := post(`{"category":"provider_quota"}`); result["requeued"] != 1 {
		t.Fatalf("requeued=%d, want 1", result["requeued"])
	}
	states := jobStatesAfter(t, handler, service)
	if states[ids["hash-quota"]] != string(domain.JobPending) {
		t.Fatalf("provider_quota job state=%q, want pending", states[ids["hash-quota"]])
	}
	for _, hash := range []string{"hash-auth", "hash-uncoded", "hash-done"} {
		want := string(domain.JobFailed)
		if hash == "hash-done" {
			want = string(domain.JobSucceeded)
		}
		if states[ids[hash]] != want {
			t.Fatalf("job %s state=%q, want %q (another category was disturbed)", hash, states[ids[hash]], want)
		}
	}

	// The unknown category reaches the job whose failure carried no code.
	if result := post(`{"category":"unknown"}`); result["requeued"] != 1 {
		t.Fatalf("unknown requeued=%d, want 1", result["requeued"])
	}

	// Empty JSON body keeps the historical retry-everything behavior.
	if result := post(`{}`); result["requeued"] != 1 {
		t.Fatalf("empty body requeued=%d, want 1 (the provider_auth job)", result["requeued"])
	}

	// A malformed body is refused rather than silently requeueing everything.
	malformed := httptest.NewRecorder()
	handler.ServeHTTP(malformed, hubAdminRequest(service, http.MethodPost, "/api/v1/pipeline/retry-failed", strings.NewReader(`{"category":`)))
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status=%d, want 400", malformed.Code)
	}

	states = jobStatesAfter(t, handler, service)
	for _, hash := range []string{"hash-quota", "hash-auth", "hash-uncoded"} {
		if states[ids[hash]] != string(domain.JobPending) {
			t.Fatalf("job %s state=%q, want pending", hash, states[ids[hash]])
		}
	}
	if states[ids["hash-done"]] != string(domain.JobSucceeded) {
		t.Fatalf("succeeded job was disturbed: %q", states[ids["hash-done"]])
	}
}

// resume-deferred's optional reason scopes the release to one deferral code,
// so the issues view's per-category retry can push a parked category forward
// without touching the other one. The absent-body path still releases the
// historical default, provider_route_exhausted.
func TestResumeDeferredJobsByReason(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "issues-resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assetID := scanOneAsset(t, repo, "resume-clip.mov")
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	ids := map[string]string{}
	for i, reason := range []domain.JobFailureCategory{domain.JobFailureCategoryProviderRouteExhausted, domain.JobFailureCategoryDiskSpaceLow} {
		if err := repo.EnqueueJob(ctx, assetID, domain.JobAnalyze, "hash-"+string(reason), 20-i); err != nil {
			t.Fatal(err)
		}
		job, err := repo.LeaseNextJob(ctx, "worker", nil, domain.LeaseFilter{})
		if err != nil || job == nil {
			t.Fatalf("lease job=%+v err=%v", job, err)
		}
		if err := repo.DeferJob(ctx, job.ID, "worker", time.Now().Add(5*time.Hour), string(reason), "parked"); err != nil {
			t.Fatal(err)
		}
		ids[string(reason)] = job.ID
	}

	post := func(body string) map[string]int {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/pipeline/resume-deferred", strings.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var result map[string]int
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	// Scoped to disk_space_low: only that job moves forward.
	if result := post(`{"reason":"disk_space_low"}`); result["resumed"] != 1 {
		t.Fatalf("resumed=%d, want 1", result["resumed"])
	}
	states := jobStatesAfter(t, handler, service)
	if states[ids[string(domain.JobFailureCategoryDiskSpaceLow)]] != string(domain.JobPending) {
		t.Fatalf("disk_space_low job state=%q, want pending", states[ids[string(domain.JobFailureCategoryDiskSpaceLow)]])
	}
	if states[ids[string(domain.JobFailureCategoryProviderRouteExhausted)]] != string(domain.JobPending) {
		t.Fatalf("route-exhausted job was disturbed: %q", states[ids[string(domain.JobFailureCategoryProviderRouteExhausted)]])
	}

	// The absent-body path is the historical default: provider_route_exhausted.
	if result := post(``); result["resumed"] != 1 {
		t.Fatalf("absent body resumed=%d, want 1 (the route-exhausted job)", result["resumed"])
	}
}

// The progress page carries the issues view: the section heading and the
// per-row retry button, both of which the page's scripts depend on by exact
// id and function name.
func TestProgressPageRendersIssuesSection(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "progress-issues.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, lanRequest(http.MethodGet, "/progress", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, marker := range []string{"需处理的问题", "重试这组", "issuesRefresh", "issuesRow", "retryIssueGroup", "/api/v1/issues"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("progress page missing %q", marker)
		}
	}
}
