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
		"disk_space_low",
		"source_missing",
		"worker_offline",
		"configuration",
		"unknown",
	}
	got := []JobFailureCategory{
		JobFailureCategoryProviderQuota,
		JobFailureCategoryProviderAuth,
		JobFailureCategoryProviderUnavailable,
		JobFailureCategoryProviderRouteExhausted,
		JobFailureCategoryMediaDecode,
		JobFailureCategoryUnsupportedMedia,
		JobFailureCategoryDiskSpaceLow,
		JobFailureCategorySourceMissing,
		JobFailureCategoryWorkerOffline,
		JobFailureCategoryConfiguration,
		JobFailureCategoryUnknown,
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
		JobFailureCategoryDiskSpaceLow:           true,
		JobFailureCategorySourceMissing:          true,
		JobFailureCategoryWorkerOffline:          true,
		JobFailureCategoryConfiguration:          false,
		JobFailureCategoryUnknown:                false,
	}
	for category, want := range cases {
		if got := category.IsRetryable(); got != want {
			t.Errorf("%q IsRetryable = %v, want %v", category, got, want)
		}
	}
	if JobFailureCategory("").IsRetryable() {
		t.Error("empty category must not read as retryable; unknown should be explicit")
	}
}
