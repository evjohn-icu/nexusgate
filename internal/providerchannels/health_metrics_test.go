package providerchannels

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/providerpool"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

// healthMetricsClock returns a mutable clock shared by the executor and its
// pools, so tests can advance cooldowns and fabricate deterministic
// invocation latencies: the operation advances the clock, and the duration
// Execute measures spans exactly that advance.
func healthMetricsClock() (func() time.Time, func(time.Duration)) {
	now := time.Unix(1_700_000_000, 0)
	return func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }
}

// The providers page needs a smoothed latency signal, not a stopwatch: a run
// of successful invocations must leave the member with a last-success time
// and an EWMA (alpha 0.2) of the durations. With the fake clock the op
// advances by exactly 10ms, so the EWMA after two calls is deterministic:
// 0.2*10ms after the first, 0.2*10ms + 0.8*2ms = 3.6ms after the second.
func TestHealthMetricsSuccessRunProducesLatencyEWMAAndLastSuccessAt(t *testing.T) {
	now, advance := healthMetricsClock()
	executor, err := NewExecutor([]Channel{{
		ID: "channel-a", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members:      []Member{{ID: "a-1", Enabled: true}},
	}}, providerpool.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	success := func(_ context.Context, _ Invocation) error {
		advance(10 * time.Millisecond)
		return nil
	}
	for i := 0; i < 2; i++ {
		if err := executor.Execute(context.Background(), CapabilityVideoAnalysis, success); err != nil {
			t.Fatalf("call %d failed: %v", i, err)
		}
	}

	member := executor.Snapshot(CapabilityVideoAnalysis).Channels[0].Members[0]
	if member.Successes != 2 || member.Attempts != 2 || member.Failures != 0 {
		t.Fatalf("counters = successes=%d attempts=%d failures=%d, want 2/2/0", member.Successes, member.Attempts, member.Failures)
	}
	if member.LastSuccessAt.IsZero() {
		t.Fatal("last_success_at is zero after a successful run")
	}
	if got, want := member.LatencyMS, 3.6; got < want-0.01 || got > want+0.01 {
		t.Fatalf("latency_ms = %.4f, want %.2f (EWMA of two 10ms samples)", got, want)
	}
}

// A rate-limited key is the shape an operator has to act on by waiting or
// swapping keys, so it must count separately from generic failures — and the
// pool's cooldown must be visible as cooldown_until, or the providers page
// cannot tell 正常 from 冷却中.
func TestHealthMetrics429FailuresIncrementRetryable429AndSetCooldown(t *testing.T) {
	now, advance := healthMetricsClock()
	executor, err := NewExecutor([]Channel{{
		ID: "channel-a", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members:      []Member{{ID: "a-1", Enabled: true}},
	}}, providerpool.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	quota := providerpool.HTTPError{Code: 429, Err: errors.New("monthly quota exhausted")}
	attempt := func(_ context.Context, _ Invocation) error { return quota }

	if err := executor.Execute(context.Background(), CapabilityVideoAnalysis, attempt); err == nil {
		t.Fatal("a rate-limited single-member route must fail")
	}
	// The pool cools for one second; let it expire so the second call is a
	// fresh attempt rather than a no-op, proving the counter accumulates.
	advance(2 * time.Second)
	if err := executor.Execute(context.Background(), CapabilityVideoAnalysis, attempt); err == nil {
		t.Fatal("a rate-limited single-member route must fail")
	}

	member := executor.Snapshot(CapabilityVideoAnalysis).Channels[0].Members[0]
	if member.Retryable429 != 2 {
		t.Fatalf("retryable_429 = %d, want 2", member.Retryable429)
	}
	if member.ServerError5xx != 0 {
		t.Fatalf("server_error_5xx = %d, want 0", member.ServerError5xx)
	}
	if member.Failures != 2 || member.Successes != 0 {
		t.Fatalf("failures=%d successes=%d, want 2/0", member.Failures, member.Successes)
	}
	if !member.LastFailureRetryable {
		t.Fatal("a 429 must be reported as a retryable failure")
	}
	// Second failure doubles the backoff: base 1s, then 2s.
	wantCooldown := now().Add(2 * time.Second)
	if !member.CooldownUntil.Equal(wantCooldown) {
		t.Fatalf("cooldown_until = %v, want %v", member.CooldownUntil, wantCooldown)
	}
	if member.HalfOpen {
		t.Fatal("a cooled member that is not probing must not be half-open")
	}
}

// A provider whose server answers 5xx is a different operator story from a
// rate limit, and the page renders them differently, so the counters must
// not share a bucket.
func TestHealthMetrics5xxFailuresIncrementServerError5xx(t *testing.T) {
	now, advance := healthMetricsClock()
	executor, err := NewExecutor([]Channel{{
		ID: "channel-a", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members:      []Member{{ID: "a-1", Enabled: true}},
	}}, providerpool.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	// common.StatusError exercises the HTTPStatusCode probe; the 429 tests
	// above exercised the StatusCode probe.
	down := &common.StatusError{StatusCode: 503, Body: "service unavailable"}
	attempt := func(_ context.Context, _ Invocation) error { return down }

	if err := executor.Execute(context.Background(), CapabilityVideoAnalysis, attempt); err == nil {
		t.Fatal("a 503-ing single-member route must fail")
	}
	// Let the one-second cooldown expire so the second call is a fresh
	// attempt rather than a no-op, proving the counter accumulates.
	advance(2 * time.Second)
	if err := executor.Execute(context.Background(), CapabilityVideoAnalysis, attempt); err == nil {
		t.Fatal("a 503-ing single-member route must fail")
	}

	member := executor.Snapshot(CapabilityVideoAnalysis).Channels[0].Members[0]
	if member.ServerError5xx != 2 {
		t.Fatalf("server_error_5xx = %d, want 2", member.ServerError5xx)
	}
	if member.Retryable429 != 0 {
		t.Fatalf("retryable_429 = %d, want 0", member.Retryable429)
	}
	if !member.CooldownUntil.After(now()) {
		t.Fatalf("cooldown_until = %v, want a future time", member.CooldownUntil)
	}
}

// One route carrying both shapes at once is the real dashboard: each member
// keeps its own counts, and the surviving member still serves the route.
func TestHealthMetricsSeparates429And5xxCountersPerMember(t *testing.T) {
	executor, err := NewExecutor([]Channel{{
		ID: "channel-a", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members: []Member{
			{ID: "throttled", Enabled: true},
			{ID: "broken", Enabled: true},
			{ID: "live", Enabled: true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, invocation Invocation) error {
		switch invocation.MemberID {
		case "throttled":
			return providerpool.HTTPError{Code: 429, Err: errors.New("rate limited")}
		case "broken":
			return &common.StatusError{StatusCode: 500, Body: "internal error"}
		default:
			return nil
		}
	})
	if err != nil {
		t.Fatalf("the live member must let the route succeed: %v", err)
	}

	snapshot := executor.Snapshot(CapabilityVideoAnalysis)
	byID := make(map[string]MemberStatus, len(snapshot.Channels[0].Members))
	for _, member := range snapshot.Channels[0].Members {
		byID[member.ID] = member
	}
	if got := byID["throttled"]; got.Retryable429 != 1 || got.ServerError5xx != 0 || got.Failures != 1 {
		t.Fatalf("throttled member counters = %+v", got)
	}
	if got := byID["broken"]; got.ServerError5xx != 1 || got.Retryable429 != 0 || got.Failures != 1 {
		t.Fatalf("broken member counters = %+v", got)
	}
	if got := byID["live"]; got.Successes != 1 || got.Failures != 0 || got.LastSuccessAt.IsZero() {
		t.Fatalf("live member counters = %+v", got)
	}
	if !snapshot.Channels[0].Available {
		t.Fatal("a channel with a live member must remain available")
	}
}

// After its cooldown expires, the pool admits exactly one probe and marks the
// member half-open until it answers. The status view must expose that state
// — it is the difference between "waiting for the cooldown" and "a call is
// out right now" — and must show the member restored once the probe succeeds.
func TestHealthMetricsReportsHalfOpenProbeAndRecovery(t *testing.T) {
	now, advance := healthMetricsClock()
	executor, err := NewExecutor([]Channel{{
		ID: "channel-a", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members:      []Member{{ID: "a-1", Enabled: true}},
	}}, providerpool.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	quota := providerpool.HTTPError{Code: 429, Err: errors.New("rate limited")}
	if err := executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, _ Invocation) error { return quota }); err == nil {
		t.Fatal("the cooling failure must fail")
	}
	advance(2 * time.Second)

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, _ Invocation) error {
			advance(5 * time.Millisecond)
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	probing := executor.Snapshot(CapabilityVideoAnalysis).Channels[0].Members[0]
	if !probing.HalfOpen {
		t.Fatal("a probe in flight must be reported half-open")
	}
	if probing.CooldownUntil.IsZero() || probing.CooldownUntil.After(now()) {
		t.Fatalf("half-open cooldown_until = %v, want the expired cooldown", probing.CooldownUntil)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the successful probe must let the route succeed: %v", err)
	}
	recovered := executor.Snapshot(CapabilityVideoAnalysis).Channels[0].Members[0]
	if recovered.HalfOpen {
		t.Fatal("a member whose probe succeeded must not stay half-open")
	}
	if !recovered.CooldownUntil.IsZero() {
		t.Fatalf("a member whose probe succeeded must not stay cooled: cooldown_until = %v", recovered.CooldownUntil)
	}
	if recovered.Successes != 1 || recovered.LastSuccessAt.IsZero() {
		t.Fatalf("the probe success must be recorded: %+v", recovered)
	}
}

// The wire names are what the providers page reads; a renamed JSON tag
// silently breaks it, so pin them.
func TestHealthMetricsSnapshotJSONExposesHealthFields(t *testing.T) {
	now, advance := healthMetricsClock()
	executor, err := NewExecutor([]Channel{{
		ID: "channel-a", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members:      []Member{{ID: "throttled", Enabled: true}, {ID: "live", Enabled: true}},
	}}, providerpool.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, invocation Invocation) error {
		if invocation.MemberID == "throttled" {
			return providerpool.HTTPError{Code: 429, Err: errors.New("rate limited")}
		}
		advance(10 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("the live member must let the route succeed: %v", err)
	}

	raw, err := json.Marshal(executor.Snapshot(CapabilityVideoAnalysis))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, field := range []string{`"last_success_at"`, `"latency_ms"`, `"retryable_429"`, `"server_error_5xx"`, `"cooldown_until"`} {
		if !strings.Contains(body, field) {
			t.Fatalf("status JSON missing %s\n%s", field, body)
		}
	}
}
