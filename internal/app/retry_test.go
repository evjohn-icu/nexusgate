package app

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/providers/common"
)

func TestRetryPolicyRetriesTransientFailuresWithBoundedBackoff(t *testing.T) {
	if !isRetryableJobError(errors.New("provider request: temporary timeout")) {
		t.Fatal("provider timeout should be retryable")
	}
	// The same sentence, marked and unmarked. The wording is identical, so the
	// marker is the only thing the two differ by — the property a list of
	// message substrings could not have had.
	if !isRetryableJobError(errors.New("video analysis provider is not configured")) {
		t.Fatal("an unmarked error must keep the retrying default, whatever its wording reads like")
	}
	permanent := domain.Permanent(errors.New("video analysis provider is not configured"))
	if isRetryableJobError(permanent) {
		t.Fatal("configuration error must not consume retries")
	}
	// execute's callers wrap on the way up, so the marker has to survive
	// wrapping the same way the status-code classification does.
	if isRetryableJobError(fmt.Errorf("analyze asset: %w", permanent)) {
		t.Fatal("a wrapped permanent failure must still be permanent")
	}
	if retryDelay(1) != time.Second || retryDelay(2) != 2*time.Second || retryDelay(100) != 30*time.Second {
		t.Fatalf("unexpected bounded backoff: first=%s second=%s capped=%s", retryDelay(1), retryDelay(2), retryDelay(100))
	}
}

// A 4xx from a provider is a setup error: the same request will be rejected
// identically on every attempt, so retrying only spends paid quota. 408 and 429
// are the exceptions that genuinely clear on their own -- and so, now, are
// 401/402/403: providerpool.ClassifyFailure calls those MemberSpent, meaning
// the pool has already retired the member that answered by the time this
// runs, so the next attempt lands on a different one.
func TestRetryPolicyTreatsProviderClientErrorsAsPermanent(t *testing.T) {
	for _, status := range []int{400, 404, 422} {
		err := &common.StatusError{StatusCode: status, Body: "unauthorized"}
		if isRetryableJobError(err) {
			t.Fatalf("HTTP %d must not consume retries or provider quota", status)
		}
		// The classification must survive the wrapping the pipeline applies.
		if isRetryableJobError(fmt.Errorf("analyze asset: %w", err)) {
			t.Fatalf("wrapped HTTP %d must still be permanent", status)
		}
	}
	for _, status := range []int{401, 402, 403, 408, 429, 500, 502, 503} {
		err := &common.StatusError{StatusCode: status, Body: "try again"}
		if !isRetryableJobError(err) {
			t.Fatalf("HTTP %d should be retryable", status)
		}
	}
}
