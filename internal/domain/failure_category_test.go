package domain

import "testing"

// The categories are the issues view's aggregation keys, written into
// jobs.last_error_code. A renamed value would split the issues history into
// two buckets, so each spelling is pinned here and only here.
func TestJobFailureCategoryConstantsAreStable(t *testing.T) {
	want := []JobFailureCategory{
		"provider_quota",
		"provider_auth",
		"provider_unavailable",
		"provider_route_exhausted",
		"media_decode",
		"unsupported_media",
		"preview_lut_missing",
		"disk_space_low",
		// Retired: no sentinel produces budget_exhausted anymore, but
		// historical rows still carry it in last_error_code, so the
		// spelling stays pinned alongside the live ones.
		"budget_exhausted",
		"source_missing",
		"worker_offline",
		"configuration",
		"unknown",
	}
	// got comes from AllJobFailureCategories instead of a hand-written list
	// of the constants, so a newly declared category that is not pinned
	// above — or a pin whose constant vanished — fails the length check
	// below instead of passing silently.
	got := AllJobFailureCategories()
	if len(got) != len(want) {
		t.Fatalf("AllJobFailureCategories returned %d categories, want %d; a category was added without pinning its spelling here (or the declaration list lost one)", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("category %d = %q, want %q", i, got[i], want[i])
		}
	}
	seen := map[JobFailureCategory]bool{}
	for _, c := range got {
		if seen[c] {
			t.Errorf("duplicate category value %q", c)
		}
		seen[c] = true
	}
}

// The defer reasons are written into the same jobs.last_error_code column the
// issues view aggregates categories from, so each pair must be one spelling —
// enforced at the declaration site, pinned here so the enforcement cannot rot.
func TestJobDeferReasonsAreCategoryValues(t *testing.T) {
	if JobDeferProviderRouteExhausted != string(JobFailureCategoryProviderRouteExhausted) {
		t.Errorf("route-exhausted defer reason %q diverged from category %q", JobDeferProviderRouteExhausted, JobFailureCategoryProviderRouteExhausted)
	}
	if JobDeferDiskSpaceLow != string(JobFailureCategoryDiskSpaceLow) {
		t.Errorf("disk-space defer reason %q diverged from category %q", JobDeferDiskSpaceLow, JobFailureCategoryDiskSpaceLow)
	}
}

// IsRetryable is the single verdict both the pipeline and the issues view use;
// the table below is its contract. Retryable means "heals on its own without
// an operator changing anything" — the account is uncapped, the provider comes
// back, the disk frees. Everything else either presents identical inputs to a
// deterministic decision on the next attempt, or needs a human to look.
func TestJobFailureCategoryIsRetryableTruthTable(t *testing.T) {
	cases := map[JobFailureCategory]bool{
		JobFailureCategoryProviderQuota:          true,
		JobFailureCategoryProviderAuth:           false,
		JobFailureCategoryProviderUnavailable:    true,
		JobFailureCategoryProviderRouteExhausted: true,
		JobFailureCategoryMediaDecode:            false,
		JobFailureCategoryUnsupportedMedia:       false,
		JobFailureCategoryPreviewLUTMissing:      false,
		JobFailureCategoryDiskSpaceLow:           true,
		// Retired producer, kept so historical rows keep their verdict.
		JobFailureCategoryBudgetExhausted: true,
		JobFailureCategorySourceMissing:   true,
		JobFailureCategoryWorkerOffline:   true,
		JobFailureCategoryConfiguration:   false,
		JobFailureCategoryUnknown:         false,
	}
	for _, category := range AllJobFailureCategories() {
		want, ok := cases[category]
		if !ok {
			t.Errorf("category %q has no retryability entry; every value AllJobFailureCategories yields must declare in this table whether it heals on retry", category)
			continue
		}
		if got := category.IsRetryable(); got != want {
			t.Errorf("%q IsRetryable = %v, want %v", category, got, want)
		}
	}
	if JobFailureCategory("").IsRetryable() {
		t.Error("empty category must not read as retryable; unknown should be explicit")
	}
}
