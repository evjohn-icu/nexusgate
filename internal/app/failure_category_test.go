package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/providerchannels"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

// classifyJobFailure's contract: the category is decided by the sentinel the
// error chain carries, never by reading the message. Each case below pins one
// mapping so a reworded message can never change a job's category.
func TestClassifyJobFailureSentinelMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want domain.JobFailureCategory
	}{
		{"429 rate limit", &common.StatusError{StatusCode: 429, Body: "quota exhausted"}, domain.JobFailureCategoryProviderQuota},
		{"408 request timeout", &common.StatusError{StatusCode: 408, Body: "slow down"}, domain.JobFailureCategoryProviderQuota},
		{"401 unauthorized", &common.StatusError{StatusCode: 401, Body: "invalid key"}, domain.JobFailureCategoryProviderAuth},
		{"402 payment required", &common.StatusError{StatusCode: 402, Body: "out of credit"}, domain.JobFailureCategoryProviderAuth},
		{"403 forbidden", &common.StatusError{StatusCode: 403, Body: "not entitled"}, domain.JobFailureCategoryProviderAuth},
		{"500 server error", &common.StatusError{StatusCode: 500, Body: "boom"}, domain.JobFailureCategoryProviderUnavailable},
		{"503 unavailable", &common.StatusError{StatusCode: 503, Body: "maintenance"}, domain.JobFailureCategoryProviderUnavailable},
		{"429 wrapped by Permanent", domain.Permanent(&common.StatusError{StatusCode: 429, Body: "quota exhausted"}), domain.JobFailureCategoryProviderQuota},
		{"503 wrapped by Permanent", domain.Permanent(&common.StatusError{StatusCode: 503, Body: "maintenance"}), domain.JobFailureCategoryProviderUnavailable},
		{"other 4xx stays unknown", &common.StatusError{StatusCode: 400, Body: "bad request"}, domain.JobFailureCategoryUnknown},
		{"route exhausted", providerchannels.ErrRouteExhausted, domain.JobFailureCategoryProviderRouteExhausted},
		{"route exhausted wrapped", fmt.Errorf("outer: %w", providerchannels.ErrRouteExhausted), domain.JobFailureCategoryProviderRouteExhausted},
		{"preflight disk sentinel", fmt.Errorf("cache volume full: %w", errDiskSpaceLow), domain.JobFailureCategoryDiskSpaceLow},
		{"ENOSPC", syscall.ENOSPC, domain.JobFailureCategoryDiskSpaceLow},
		{"ENOSPC wrapped", fmt.Errorf("write failed: %w", syscall.ENOSPC), domain.JobFailureCategoryDiskSpaceLow},
		{"context deadline", context.DeadlineExceeded, domain.JobFailureCategoryProviderUnavailable},
		{"network error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, domain.JobFailureCategoryProviderUnavailable},
		{"plain media decode error", errors.New("ffprobe: exit status 1"), domain.JobFailureCategoryUnknown},
		// A cold-start Hub with zero providers reaches exactly the first of
		// these: channelASR.Transcribe and channelVideo.Analyze short-circuit
		// to ErrProviderChannelNotConfigured before any executor runs, so
		// ErrNoRoute never surfaces. The other two are the same family — a
		// route that resolves to no credential, and a protocol the channel
		// router cannot build — and their own doc comments say so.
		{"provider channel not configured", ErrProviderChannelNotConfigured, domain.JobFailureCategoryConfiguration},
		{"provider channel not configured wrapped", fmt.Errorf("analyze asset: %w", ErrProviderChannelNotConfigured), domain.JobFailureCategoryConfiguration},
		{"provider channel secret missing", errProviderChannelSecretMissing, domain.JobFailureCategoryConfiguration},
		{"provider channel secret missing wrapped", fmt.Errorf("transcribe: %w", errProviderChannelSecretMissing), domain.JobFailureCategoryConfiguration},
		{"provider channel multiframe unsupported", errProviderChannelMultiframeUnsupported, domain.JobFailureCategoryConfiguration},
		// A spent key is not a configuration problem: the deployment is fine,
		// the credential is dead. It must stay ProviderAuth.
		{"spent key is not configuration", &common.StatusError{StatusCode: 402, Body: "payment required"}, domain.JobFailureCategoryProviderAuth},
		{"permanent without a known sentinel", domain.Permanent(errors.New("audio artifact missing")), domain.JobFailureCategoryUnknown},
		{"nil", nil, domain.JobFailureCategoryUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyJobFailure(tc.err); got != tc.want {
				t.Fatalf("classifyJobFailure(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
