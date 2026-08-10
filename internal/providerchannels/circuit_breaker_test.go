package providerchannels

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/providerpool"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

// circuitClock is a controllable clock for making cooldown expiry
// deterministic in the circuit-breaker tests.
type circuitClock struct {
	now time.Time
}

func (c *circuitClock) Now() time.Time { return c.now }

func (c *circuitClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// The status snapshot has to tell a cooling route from a healthy one. Before
// the cooldown was consulted, a channel whose only key was in backoff reported
// available=true — green in the shell strip and providers page while every job
// on it parked. A cooling member must take the channel out of "available" for
// the duration, and its own entry must show the cooldown.
func TestSnapshotReportsCoolingChannelUnavailable(t *testing.T) {
	clock := &circuitClock{now: time.Unix(1000, 0)}
	executor, err := NewExecutor([]Channel{{
		ID: "single", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members:      []Member{{ID: "only", Enabled: true}},
	}}, providerpool.Options{Now: clock.Now, BaseCooldown: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, _ Invocation) error {
		return &common.StatusError{StatusCode: 429, Body: "slow down"}
	})
	if !errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("a route whose only key is cooling must report exhaustion, got %v", err)
	}

	snapshot := executor.Snapshot(CapabilityVideoAnalysis)
	if len(snapshot.Channels) != 1 {
		t.Fatalf("snapshot channels = %+v", snapshot.Channels)
	}
	if snapshot.Channels[0].Available {
		t.Fatal("a channel whose only key is cooling must not report itself available")
	}
	member := snapshot.Channels[0].Members[0]
	if member.Retired {
		t.Fatal("a 429 is a transient failure, not a retired key")
	}
	if member.CooldownUntil.IsZero() || !member.CooldownUntil.After(clock.now) {
		t.Fatalf("cooling member cooldown_until = %v, want a time in the future", member.CooldownUntil)
	}
	if member.HalfOpen {
		t.Fatal("a cooling member is not half-open; no probe has been issued")
	}
	if !member.LastFailureRetryable {
		t.Fatal("a 429 that cooled the member must be reported as retryable")
	}
}

// The same channel has to come back green once the cooldown has passed and the
// half-open probe succeeds: the pool restores the member, and the snapshot
// must follow.
func TestSnapshotRecoversAfterCooldownAndSuccessfulProbe(t *testing.T) {
	clock := &circuitClock{now: time.Unix(1000, 0)}
	executor, err := NewExecutor([]Channel{{
		ID: "single", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members:      []Member{{ID: "only", Enabled: true}},
	}}, providerpool.Options{Now: clock.Now, BaseCooldown: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	fail := func(_ context.Context, _ Invocation) error {
		return &common.StatusError{StatusCode: 503, Body: "temporary provider failure"}
	}
	if err := executor.Execute(context.Background(), CapabilityVideoAnalysis, fail); err == nil {
		t.Fatal("the failing call must not succeed")
	}
	if snapshot := executor.Snapshot(CapabilityVideoAnalysis); snapshot.Channels[0].Available {
		t.Fatal("channel must be unavailable while its only key cools")
	}

	clock.Advance(2 * time.Hour)
	if err := executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, _ Invocation) error { return nil }); err != nil {
		t.Fatalf("half-open probe failed: %v", err)
	}

	snapshot := executor.Snapshot(CapabilityVideoAnalysis)
	if !snapshot.Channels[0].Available {
		t.Fatal("channel must be available again after the probe succeeds")
	}
	member := snapshot.Channels[0].Members[0]
	if !member.CooldownUntil.IsZero() {
		t.Fatalf("successful probe left cooldown_until = %v, want zero", member.CooldownUntil)
	}
	if member.HalfOpen {
		t.Fatal("successful probe left the member half-open")
	}
}

// The circuit-breaker guarantee: a single malformed response (400) is a fact
// about the request, not the key or the route. It must neither retire the
// member nor mark the route exhausted, and the next call — on the same key —
// must still go through.
func TestSingleNonRetryableFailureDoesNotTripTheRoute(t *testing.T) {
	executor, err := NewExecutor([]Channel{{
		ID: "single", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members:      []Member{{ID: "only", Enabled: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, _ Invocation) error {
		return &common.StatusError{StatusCode: 400, Body: "invalid request"}
	})
	if err == nil {
		t.Fatal("a request the provider rejects must fail")
	}
	if errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("one malformed response is not an exhausted route: %v", err)
	}

	snapshot := executor.Snapshot(CapabilityVideoAnalysis)
	channel := snapshot.Channels[0]
	member := channel.Members[0]
	if member.Retired {
		t.Fatal("a 400 describes the request, not the key; the member must not retire")
	}
	if !member.CooldownUntil.IsZero() {
		t.Fatalf("a 400 must not cool the member, got cooldown_until = %v", member.CooldownUntil)
	}
	if !channel.Available {
		t.Fatal("a channel whose key is untouched by the bad request must stay available")
	}

	// The next call goes to the same member and succeeds: the route was never
	// tripped.
	var calls []string
	if err := executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation.MemberID)
		return nil
	}); err != nil {
		t.Fatalf("call after a malformed response failed: %v", err)
	}
	if !equalStrings(calls, []string{"only"}) {
		t.Fatalf("calls = %v, want the same key tried again", calls)
	}
}

// A retired key must not drag a healthy one down with it: with two members,
// one answering 401 takes itself out of the route, and the channel stays
// available on the survivor.
func TestSnapshotStaysAvailableWhileASpentKeyIsRetired(t *testing.T) {
	executor, err := NewExecutor([]Channel{{
		ID: "pooled", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members:      []Member{{ID: "spent", Enabled: true}, {ID: "live", Enabled: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation.MemberID)
		if invocation.MemberID == "spent" {
			return &common.StatusError{StatusCode: 401, Body: "invalid api key"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("one spent key must not fail a request the next key can serve: %v", err)
	}
	if !equalStrings(calls, []string{"spent", "live"}) {
		t.Fatalf("calls = %v, want the spent key to hand over to the live one", calls)
	}

	snapshot := executor.Snapshot(CapabilityVideoAnalysis)
	channel := snapshot.Channels[0]
	if !channel.Available {
		t.Fatal("a channel with a working key must stay available even if a sibling retired")
	}
	members := make(map[string]MemberStatus, len(channel.Members))
	for _, member := range channel.Members {
		members[member.ID] = member
	}
	if !members["spent"].Retired {
		t.Fatal("the spent member is not reported as retired")
	}
	if members["live"].Retired {
		t.Fatal("the live member was reported retired")
	}
	if !members["live"].CooldownUntil.IsZero() {
		t.Fatalf("the live member shows a cooldown: %v", members["live"].CooldownUntil)
	}
}
