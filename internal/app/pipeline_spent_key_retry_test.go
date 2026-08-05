package app

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/providerchannels"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

// isRetryableJobError's 4xx block assumes the executor has already turned a
// route with nothing left into providerchannels.ErrRouteExhausted, which the
// case above this one parks instead of retrying. That assumption breaks in
// one place: providerchannels.Executor.routeFailingEverywhere requires every
// member to have been *attempted* before it will call a route exhausted (see
// its doc comment), and a member that is merely saturated -- its MaxInflight
// held by a concurrent caller -- was never attempted. providerpool.Pool.Select
// returns ErrNoAvailable for it without an attempt, Execute's inner loop
// breaks on that error, and the raw MemberSpent status (401/402/403)
// surfaces to the caller exactly as if no retirement had happened.
//
// This drives that path for real rather than asserting it from the outside:
// two members share a channel, one is held busy by a concurrent call for the
// whole test (so it can never be attempted), and the only member Execute can
// reach is spent.
func TestSpentKeyErrorFromASaturatedLastMemberIsRetryable(t *testing.T) {
	acquired := make(chan struct{})
	release := make(chan struct{})

	executor, err := providerchannels.NewExecutor([]providerchannels.Channel{{
		ID: "pooled", ProviderName: "provider-a", Enabled: true,
		Capabilities: []providerchannels.Capability{providerchannels.CapabilityVideoAnalysis},
		Members: []providerchannels.Member{
			{ID: "busy", Enabled: true, MaxInflight: 1},
			{ID: "spent", Enabled: true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// The pool's cursor starts at zero and "busy" is member zero, so this
		// call claims it before the test's own Execute call ever runs, and
		// holds it inflight (MaxInflight: 1) until release closes.
		_ = executor.Execute(context.Background(), providerchannels.CapabilityVideoAnalysis, func(_ context.Context, _ providerchannels.Invocation) error {
			close(acquired)
			<-release
			return nil
		})
	}()
	<-acquired

	var sawMember string
	err = executor.Execute(context.Background(), providerchannels.CapabilityVideoAnalysis, func(_ context.Context, invocation providerchannels.Invocation) error {
		sawMember = invocation.MemberID
		return &common.StatusError{StatusCode: 402, Body: "insufficient balance"}
	})
	close(release)
	wg.Wait()

	if sawMember != "spent" {
		t.Fatalf("expected only the spent member to be reachable while busy is saturated, got %q", sawMember)
	}
	if errors.Is(err, providerchannels.ErrRouteExhausted) {
		t.Fatalf("routeFailingEverywhere must not call this route exhausted -- \"busy\" was saturated, never attempted: %v", err)
	}
	var status *common.StatusError
	if !errors.As(err, &status) || status.StatusCode != 402 {
		t.Fatalf("expected the raw spent-key error to surface un-wrapped, got %v", err)
	}
	if !isRetryableJobError(err) {
		t.Fatalf("a spent key masked by a saturated last member must be retried, not failed permanently -- the pool already retired it, so the very next attempt selects a different member: %v", err)
	}
}

// The classification above must come from the status alone. A relay or an
// upstream provider can echo request text into an error body (see
// redactError's own doc comment on that hazard), so if a permanent-sounding
// word in the body could flip a spent-key error back to "permanent", a
// worker-side echo would silently re-break the fix above.
func TestSpentKeyRetryDecisionIsDrivenByStatusNotWording(t *testing.T) {
	for _, body := range []string{
		"insufficient balance",
		"validation_error: this reads like a permanent, not-configured, unauthorized, forbidden rejection",
	} {
		for _, status := range []int{401, 402, 403} {
			err := &common.StatusError{StatusCode: status, Body: body}
			if !isRetryableJobError(err) {
				t.Fatalf("HTTP %d with body %q was treated as permanent; classification must come from the status, not the wording", status, body)
			}
		}
	}
}
