package app

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ev/timingdex/internal/providers/common"
)

func TestRetryPolicyRetriesTransientFailuresWithBoundedBackoff(t *testing.T) {
	if !isRetryableJobError(errors.New("provider request: temporary timeout")) {
		t.Fatal("provider timeout should be retryable")
	}
	if isRetryableJobError(errors.New("video analysis provider is not configured")) {
		t.Fatal("configuration error must not consume retries")
	}
	if retryDelay(1) != time.Second || retryDelay(2) != 2*time.Second || retryDelay(100) != 30*time.Second {
		t.Fatalf("unexpected bounded backoff: first=%s second=%s capped=%s", retryDelay(1), retryDelay(2), retryDelay(100))
	}
}

// A 4xx from a provider is a setup error: the same request will be rejected
// identically on every attempt, so retrying only spends paid quota. 408 and 429
// are the exceptions that genuinely clear on their own.
func TestRetryPolicyTreatsProviderClientErrorsAsPermanent(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422} {
		err := &common.StatusError{StatusCode: status, Body: "unauthorized"}
		if isRetryableJobError(err) {
			t.Fatalf("HTTP %d must not consume retries or provider quota", status)
		}
		// The classification must survive the wrapping the pipeline applies.
		if isRetryableJobError(fmt.Errorf("analyze asset: %w", err)) {
			t.Fatalf("wrapped HTTP %d must still be permanent", status)
		}
	}
	for _, status := range []int{408, 429, 500, 502, 503} {
		err := &common.StatusError{StatusCode: status, Body: "try again"}
		if !isRetryableJobError(err) {
			t.Fatalf("HTTP %d should be retryable", status)
		}
	}
}
