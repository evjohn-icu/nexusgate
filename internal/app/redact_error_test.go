package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/providers/common"
)

// secret cannot occur by accident in any provider-channel error text produced
// by this package, so any leftover trace of it in the redacted chain is a
// leak, not a coincidence.
const redactErrorTestSecret = "sk-test-KEYVALUE-do-not-leak"

// TestRedactErrorScrubsWholeChainIncludingWrappedStatusError is the strict
// leak test. A relay can echo the request back into the response body, and
// common.StatusError.Body carries that upstream body verbatim, so the secret
// must not survive in the top-level message, in a *common.StatusError found
// via errors.As, or in that status's own %v formatting.
func TestRedactErrorScrubsWholeChainIncludingWrappedStatusError(t *testing.T) {
	original := fmt.Errorf("analyze: %w", &common.StatusError{
		StatusCode: 400,
		Body:       "…echoed request Authorization: Bearer " + redactErrorTestSecret + " …",
	})

	redacted := redactError(original, redactErrorTestSecret)

	if strings.Contains(redacted.Error(), redactErrorTestSecret) {
		t.Fatalf("redacted top-level message leaked the secret: %q", redacted.Error())
	}

	var status *common.StatusError
	if !errors.As(redacted, &status) {
		t.Fatal("errors.As found no *common.StatusError in the redacted chain; classification by status code is impossible")
	}
	if status.StatusCode != 400 {
		t.Fatalf("status.StatusCode = %d; want 400", status.StatusCode)
	}
	if strings.Contains(status.Body, redactErrorTestSecret) {
		t.Fatalf("status.Body leaked the secret: %q", status.Body)
	}
	if formatted := fmt.Sprintf("%v", status); strings.Contains(formatted, redactErrorTestSecret) {
		t.Fatalf("formatting the recovered *common.StatusError leaked the secret: %q", formatted)
	}
}

// TestRedactErrorPreservesClassificationAcrossStatusCodes is the assertion
// that would have caught the original bug: redactError used to flatten the
// chain with errors.New, so errors.As(err, &status) in isRetryableJobError
// never matched a channel-routed call, and every 4xx fell through to the
// substring list -- burning the full retry ladder against a paid provider on
// a deterministic rejection.
func TestRedactErrorPreservesClassificationAcrossStatusCodes(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
	}{
		{400, false},
		{404, false},
		// 401/402/403 answer about the key, not the request: providerpool
		// retires the member that carried them, so the next attempt reaches a
		// different one and retrying is the right call.
		{401, true},
		{402, true},
		{403, true},
		{408, true},
		{429, true},
		{500, true},
		{503, true},
	}
	for _, tc := range cases {
		original := fmt.Errorf("analyze: %w", &common.StatusError{
			StatusCode: tc.status,
			Body:       "upstream said no, secret=" + redactErrorTestSecret,
		})
		redacted := redactError(original, redactErrorTestSecret)
		if got := isRetryableJobError(redacted); got != tc.retryable {
			t.Fatalf("status %d: isRetryableJobError(redactError(...)) = %v; want %v", tc.status, got, tc.retryable)
		}
	}
}

// TestRedactErrorNonStatusErrorStillRedactsAndClassifies covers an error
// chain that never carried a *common.StatusError -- e.g. a config-shaped
// failure raised before any HTTP call. errors.As must cleanly find nothing,
// and redaction must not cost the error its permanence: redactError rebuilds
// the chain from scratch, which is what makes the scrub total and also what
// would drop a marker it did not deliberately carry over.
func TestRedactErrorNonStatusErrorStillRedactsAndClassifies(t *testing.T) {
	original := domain.Permanent(errors.New("video analysis provider is not configured, key=" + redactErrorTestSecret))

	redacted := redactError(original, redactErrorTestSecret)

	if strings.Contains(redacted.Error(), redactErrorTestSecret) {
		t.Fatalf("redacted message leaked the secret: %q", redacted.Error())
	}
	var status *common.StatusError
	if errors.As(redacted, &status) {
		t.Fatalf("errors.As found a *common.StatusError that was never in the chain: %+v", status)
	}
	if isRetryableJobError(redacted) {
		t.Fatal("redaction dropped the permanence marker; a rebuilt chain must keep it")
	}
	// The default survives redaction too: an unmarked failure stays retryable
	// however its text reads.
	if !isRetryableJobError(redactError(errors.New("video analysis provider is not configured"), redactErrorTestSecret)) {
		t.Fatal("an unmarked error must stay retryable through redaction")
	}
}

// TestRedactErrorNilInNilOut matches the nil short-circuit every other
// redact* helper in this file takes.
func TestRedactErrorNilInNilOut(t *testing.T) {
	if err := redactError(nil, redactErrorTestSecret); err != nil {
		t.Fatalf("redactError(nil, secret) = %v; want nil", err)
	}
}

// TestRedactErrorEmptySecretPreservesStatus exercises redactString's early
// return on an empty secret ("" never matches, so nothing is replaced). The
// wrapping must not panic on that path, and a status found in the chain must
// still be rebuilt (with its Body untouched, since there is nothing to
// redact).
func TestRedactErrorEmptySecretPreservesStatus(t *testing.T) {
	original := fmt.Errorf("analyze: %w", &common.StatusError{StatusCode: 401, Body: "unauthorized"})

	redacted := redactError(original, "")

	if redacted == nil {
		t.Fatal("redactError(err, \"\") = nil; want a non-nil redacted error")
	}
	var status *common.StatusError
	if !errors.As(redacted, &status) {
		t.Fatal("errors.As found no *common.StatusError when the secret was empty")
	}
	if status.StatusCode != 401 || status.Body != "unauthorized" {
		t.Fatalf("status = %+v; want StatusCode=401 Body=\"unauthorized\" unchanged", status)
	}
	// 401 describes the key, not the request: providerpool retires the
	// member that carried it, so the next attempt reaches a different one.
	if !isRetryableJobError(redacted) {
		t.Fatal("401 must still classify as MemberSpent-retryable when the secret is empty")
	}
}
