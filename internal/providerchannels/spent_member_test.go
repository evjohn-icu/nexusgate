package providerchannels

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/providers/common"
)

// The documented deployment pools several plan keys as members of one channel.
// A key that has run out of credit answers 402 and must take itself out of the
// route; the request is fine and the next key can serve it. A malformed request
// answers 400 and must not cost a second paid call to hear the same thing.
func TestSpentKeyFallsThroughToTheNextMemberButABadRequestDoesNot(t *testing.T) {
	channels := func() []Channel {
		return []Channel{{
			ID: "pooled", ProviderName: "provider-a", Enabled: true,
			Capabilities: []Capability{CapabilityVideoAnalysis},
			Members:      []Member{{ID: "spent", Enabled: true}, {ID: "live", Enabled: true}},
		}}
	}

	executor, err := NewExecutor(channels())
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation.MemberID)
		if invocation.MemberID == "spent" {
			return &common.StatusError{StatusCode: 402, Body: "insufficient balance"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("one spent key must not fail a request the next key can serve: %v", err)
	}
	if !equalStrings(calls, []string{"spent", "live"}) {
		t.Fatalf("calls = %v, want the spent key to hand over to the next member", calls)
	}

	// The operator has to be able to see which key died; nothing in
	// configuration records it.
	snapshot := executor.Snapshot(CapabilityVideoAnalysis)
	if len(snapshot.Channels) != 1 || len(snapshot.Channels[0].Members) != 2 {
		t.Fatalf("unexpected snapshot shape: %+v", snapshot)
	}
	if !snapshot.Channels[0].Members[0].Retired {
		t.Fatal("the spent member is not reported as retired")
	}
	if snapshot.Channels[0].Members[1].Retired || !snapshot.Channels[0].Available {
		t.Fatalf("a channel with a working key must stay available: %+v", snapshot.Channels[0])
	}

	badRequest, err := NewExecutor(channels())
	if err != nil {
		t.Fatal(err)
	}
	calls = nil
	err = badRequest.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation.MemberID)
		return &common.StatusError{StatusCode: 400, Body: "invalid request"}
	})
	if err == nil {
		t.Fatal("a request the provider rejects must fail")
	}
	if !equalStrings(calls, []string{"spent"}) {
		t.Fatalf("calls = %v; a malformed request was paid for twice", calls)
	}
	if errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("a bad request is not an exhausted route: %v", err)
	}
}

// Retiring a key is not one of the channel's three tries. If it were, two dead
// keys would be enough to hide a live third and the job would fail on a route
// that still works.
func TestRetiringAKeyDoesNotSpendTheChannelsRetryBudget(t *testing.T) {
	executor, err := NewExecutor([]Channel{{
		ID: "pooled", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members: []Member{
			{ID: "a-1", Enabled: true}, {ID: "a-2", Enabled: true},
			{ID: "a-3", Enabled: true}, {ID: "a-4", Enabled: true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation.MemberID)
		if invocation.MemberID == "a-4" {
			return nil
		}
		return &common.StatusError{StatusCode: 401, Body: "invalid api key"}
	})
	if err != nil {
		t.Fatalf("three spent keys hid a working fourth: %v", err)
	}
	if !equalStrings(calls, []string{"a-1", "a-2", "a-3", "a-4"}) {
		t.Fatalf("calls = %v, want every key tried until one worked", calls)
	}
}

// A channel whose every key is spent is not a malformed request and must not
// fail a job at 03:00. It is the same shape as a spent monthly quota: park it
// where an operator can see it and let the key be fixed.
func TestEveryKeySpentParksTheRouteInsteadOfFailingTheJob(t *testing.T) {
	executor, err := NewExecutor([]Channel{{
		ID: "pooled", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members:      []Member{{ID: "a-1", Enabled: true}, {ID: "a-2", Enabled: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	spent := func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation.MemberID)
		return &common.StatusError{StatusCode: 402, Body: "insufficient balance"}
	}
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, spent)
	if !errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("a route with no working key left must report exhaustion, got %v", err)
	}
	// The cause has to survive: it is what tells the operator to top up rather
	// than to wait out an outage.
	if !strings.Contains(err.Error(), "insufficient balance") {
		t.Fatalf("exhaustion error dropped the cause: %v", err)
	}
	if !equalStrings(calls, []string{"a-1", "a-2"}) {
		t.Fatalf("calls = %v, want each key tried exactly once", calls)
	}

	// Every later job on this route must reach the same conclusion without
	// paying for it, until the Hub restarts or the channel is edited.
	before := len(calls)
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, spent)
	if len(calls) != before {
		t.Fatalf("a fully retired route issued another paid request: %v", calls)
	}
	if !errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("a fully retired route must still report exhaustion, got %v", err)
	}

	snapshot := executor.Snapshot(CapabilityVideoAnalysis)
	if snapshot.Channels[0].Available {
		t.Fatal("a channel with no working key left must not report itself available")
	}
	for _, member := range snapshot.Channels[0].Members {
		if !member.Retired {
			t.Fatalf("member %q is spent but not reported retired", member.ID)
		}
		if member.LastFailureRetryable {
			t.Fatalf("member %q retired on a dead key; that is not a transient failure", member.ID)
		}
	}
}
