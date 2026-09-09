package providerchannels

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/providerpool"
	"github.com/evjohn-icu/nexusgate/internal/providers/common"
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
//
// This must hold even though a single 429 on the route's only key is *not*
// exhaustion (see routeFailingEverywhere's doc comment and
// TestASingleRetryableFailureIsNotExhaustionEvenOnAOneMemberRoute):
// "unavailable right now" and "exhausted" are different questions, and the
// snapshot answers the first one.
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
	if errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("one 429 on the only key is not confirmed exhaustion, got %v", err)
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

// This is the audited defect, reproduced directly: a library run parked 23
// analyze jobs five hours into the future on a route whose single member had
// only ever failed once, then recovered on its own within a second. A
// one-second cooldown on the route's only key must not read as "every key on
// this route is spent" -- that sentinel (ErrRouteExhausted) is reserved for
// confirmed exhaustion, and one failure is not confirmation.
//
// The second Execute call is the shape that actually parked those 23 jobs:
// each found the member still cooling from the *first* job's failure and
// made no request of its own (providerpool.Pool.Select returns ErrNoAvailable
// before any call is attempted). That must read as an ordinary, short-lived
// unavailability -- not as route exhaustion, and not as "no route configured"
// (ErrNoRoute) either, since a route that will recover on its own is not a
// configuration problem for an operator to fix.
func TestASingleRetryableFailureIsNotExhaustionEvenOnAOneMemberRoute(t *testing.T) {
	clock := &circuitClock{now: time.Unix(1_600_000_000, 0)}
	executor, err := NewExecutor([]Channel{{
		ID: "single", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members:      []Member{{ID: "only", Enabled: true}},
	}}, providerpool.Options{Now: clock.Now, BaseCooldown: time.Second, MaxCooldown: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	// Job 1: the only member answers a transient failure once.
	var calls []string
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation.MemberID)
		return &common.StatusError{StatusCode: 503, Body: "upstream unavailable"}
	})
	if errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("job 1: one transient failure on the only key is not exhaustion, got %v", err)
	}
	if errors.Is(err, ErrNoRoute) {
		t.Fatalf("job 1: a configured, momentarily-cooling route is not \"no route\", got %v", err)
	}

	// Job 2, issued immediately after (same instant, well inside the
	// one-second cooldown): must find the member cooling and make no request
	// at all -- exactly what happened to jobs 2-23 in the traced incident.
	before := len(calls)
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation.MemberID)
		return nil
	})
	if len(calls) != before {
		t.Fatalf("job 2: a route still within its first cooldown should not have issued a request: %v", calls)
	}
	if errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("job 2: zero requests issued is not confirmed exhaustion, got %v", err)
	}
	if errors.Is(err, ErrNoRoute) {
		t.Fatalf("job 2: a route that will recover on its own is not \"no route\", got %v", err)
	}
	if !errors.Is(err, providerpool.ErrNoAvailable) {
		t.Fatalf("job 2: expected the plain \"not eligible right now\" signal, got %v", err)
	}

	// Once the cooldown actually elapses, the same member serves the next
	// call normally -- the whole point of treating this as bounded rather
	// than exhausted.
	clock.Advance(2 * time.Second)
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation.MemberID)
		return nil
	})
	if err != nil {
		t.Fatalf("job 3, after the cooldown cleared, must succeed: %v", err)
	}
}

// A member whose retryable-failure count has already reached the confirmed
// threshold, but whose cooldown from that *last* failure has since elapsed
// and whose slot is currently held by a concurrent caller, must not read as
// exhausted. This isolates routeFailingEverywhere's "cooldown must still be
// in effect right now" clause from its "confirmed failure count" clause: the
// member here clears confirmedRetryableFailures (so a mutation that dropped
// the elapsed-cooldown check would not be caught by the failure count alone),
// and its unavailability right now is saturation on an elapsed cooldown, not
// an active one.
func TestConfirmedMemberWithAnElapsedCooldownIsNotExhaustion(t *testing.T) {
	clock := &circuitClock{now: time.Unix(1_600_000_000, 0)}
	executor, err := NewExecutor([]Channel{{
		ID: "single", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members:      []Member{{ID: "only", Enabled: true, MaxInflight: 1}},
	}}, providerpool.Options{Now: clock.Now, BaseCooldown: time.Second, MaxCooldown: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	fail := func(_ context.Context, _ Invocation) error {
		return &common.StatusError{StatusCode: 503, Body: "upstream unavailable"}
	}

	// Fail once, let that cooldown elapse, fail again: retryableFails now
	// reaches confirmedRetryableFailures (2). This alone must already read as
	// exhausted (its cooldown is active) -- confirmed via the earlier
	// multi-round test, not re-asserted here.
	if err := executor.Execute(context.Background(), CapabilityVideoAnalysis, fail); errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("one failure is not exhaustion, got %v", err)
	}
	clock.Advance(2 * time.Second)
	if err := executor.Execute(context.Background(), CapabilityVideoAnalysis, fail); !errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("two confirmed failures on the only key must read as exhausted before the cooldown elapses, got %v", err)
	}

	// Now let that second, longer cooldown elapse too.
	clock.Advance(3 * time.Second)

	// Hold the member's single inflight slot busy with a concurrent caller so
	// the next Execute finds it saturated on an elapsed cooldown, not cooling.
	acquired := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, _ Invocation) error {
			close(acquired)
			<-release
			return nil
		})
	}()
	<-acquired

	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, _ Invocation) error {
		return nil
	})
	close(release)
	<-done

	if errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("a confirmed member saturated after its cooldown elapsed is not exhaustion, got %v", err)
	}
}
