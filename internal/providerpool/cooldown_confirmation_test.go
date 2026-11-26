package providerpool

import (
	"errors"
	"testing"
	"time"
)

// errSyntheticRetryable classifies as Retryable through its text (see
// ClassifyFailure's vocabulary branch) without carrying an HTTP status.
var errSyntheticRetryable = errors.New("synthetic upstream timeout")

func newConfirmationPool(t *testing.T, clock *fakeClock) *Pool {
	t.Helper()
	pool, err := NewPool([]Member{
		{Name: "only", Capabilities: []Capability{"video_analysis"}, Enabled: true},
	}, Options{Now: clock.Now, BaseCooldown: time.Second, MaxCooldown: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

// Two leases acquired while the member was healthy and completed with
// retryable failures in overlapping time must count as ONE confirmation: the
// second failure observed the same moment's outage, not a fresh attempt made
// after the backoff. The cooldown starts once and is neither extended nor
// deepened by the overlapping completion.
func TestOverlappingRetryableFailuresCountAsOneConfirmation(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	pool := newConfirmationPool(t, clock)

	first, err := pool.Select("video_analysis")
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.Select("video_analysis")
	if err != nil {
		t.Fatalf("a healthy member with unlimited inflight must accept a second overlapping lease: %v", err)
	}

	first.Done(errSyntheticRetryable)
	health, _ := pool.HealthState("only", "video_analysis")
	if health.RetryableFails != 1 {
		t.Fatalf("first retryable failure of a healthy epoch must count 1, got %d", health.RetryableFails)
	}
	if health.CooldownUntil.IsZero() || !health.CooldownUntil.After(clock.now) {
		t.Fatalf("first failure must start a cooldown, got cooldown_until=%v", health.CooldownUntil)
	}
	cooldown := health.CooldownUntil

	// The overlapping lease completes after the cooldown started: stale
	// evidence, no confirmation, no extension.
	second.Done(errSyntheticRetryable)
	health, _ = pool.HealthState("only", "video_analysis")
	if health.RetryableFails != 1 {
		t.Fatalf("an overlapping pre-cooldown failure must not confirm, got count %d", health.RetryableFails)
	}
	if !health.CooldownUntil.Equal(cooldown) {
		t.Fatalf("a stale completion must not extend the cooldown: got %v, want %v", health.CooldownUntil, cooldown)
	}
}

// A pre-cooldown overlapping lease completing successfully must neither clear
// the cooldown nor reset the confirmation count.
func TestOverlappingSuccessDoesNotClearCooldown(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	pool := newConfirmationPool(t, clock)

	first, err := pool.Select("video_analysis")
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.Select("video_analysis")
	if err != nil {
		t.Fatal(err)
	}
	first.Done(errSyntheticRetryable)
	health, _ := pool.HealthState("only", "video_analysis")
	cooldown := health.CooldownUntil

	second.Done(nil)
	health, _ = pool.HealthState("only", "video_analysis")
	if !health.CooldownUntil.Equal(cooldown) {
		t.Fatalf("a pre-cooldown success must not clear a newer cooldown: got %v, want %v", health.CooldownUntil, cooldown)
	}
	if health.RetryableFails != 1 {
		t.Fatalf("a pre-cooldown success must not reset the confirmation count, got %d", health.RetryableFails)
	}
}

// The full confirmation ladder: first failure counts 1 and starts a cooldown;
// a stale pre-cooldown lease completing after that cooldown expired does not
// confirm anything; and only a distinct half-open probe acquired after the
// cooldown elapsed can confirm count 2. (A probe requires inflight == 0, so a
// stale lease cannot still be in flight when one is issued — the guard it
// exercises is the same epoch mismatch the overlapping-failure test hits.)
func TestSequentialConfirmationStillConfirmsAndStaleCannotOverwriteProbe(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	pool := newConfirmationPool(t, clock)

	// Two overlapping healthy leases; the second stays in flight.
	first, err := pool.Select("video_analysis")
	if err != nil {
		t.Fatal(err)
	}
	stale, err := pool.Select("video_analysis")
	if err != nil {
		t.Fatal(err)
	}
	first.Done(errSyntheticRetryable)

	// While the member cools (and the stale lease is still in flight),
	// Select refuses work.
	if _, err := pool.Select("video_analysis"); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("Select during a cooldown must return ErrNoAvailable, got %v", err)
	}

	// After the cooldown elapses but before any probe, the stale pre-cooldown
	// lease completes with the same failure. It must not count as a second
	// confirmation and must not extend the cooldown the first failure started.
	clock.Advance(2 * time.Second)
	health, _ := pool.HealthState("only", "video_analysis")
	startedCooldown := health.CooldownUntil
	stale.Done(errSyntheticRetryable)
	health, _ = pool.HealthState("only", "video_analysis")
	if health.RetryableFails != 1 {
		t.Fatalf("a stale post-expiry failure must not confirm, got count %d", health.RetryableFails)
	}
	if !health.CooldownUntil.Equal(startedCooldown) {
		t.Fatalf("a stale failure must not extend the cooldown: got %v, want %v", health.CooldownUntil, startedCooldown)
	}

	// The next acquisition is a half-open probe; its failure is the second,
	// independently-timed confirmation.
	probe, err := pool.Select("video_analysis")
	if err != nil {
		t.Fatalf("post-cooldown probe must be acquirable: %v", err)
	}
	if health, _ = pool.HealthState("only", "video_analysis"); !health.HalfOpen {
		t.Fatal("a post-cooldown acquisition must be a half-open probe")
	}
	probe.Done(errSyntheticRetryable)
	health, _ = pool.HealthState("only", "video_analysis")
	if health.RetryableFails != 2 {
		t.Fatalf("a failed post-cooldown probe must confirm count 2, got %d", health.RetryableFails)
	}
	if health.HalfOpen {
		t.Fatal("a failed probe must clear the half-open state")
	}
}

// A successful post-cooldown probe recovers the member: the count resets, and
// a fresh first failure starts the ladder at one again.
func TestSuccessfulProbeResetsAndFreshFailureCountsOne(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	pool := newConfirmationPool(t, clock)

	lease, err := pool.Select("video_analysis")
	if err != nil {
		t.Fatal(err)
	}
	lease.Done(errSyntheticRetryable)

	clock.Advance(2 * time.Second)
	probe, err := pool.Select("video_analysis")
	if err != nil {
		t.Fatal(err)
	}
	probe.Done(nil)
	health, _ := pool.HealthState("only", "video_analysis")
	if health.RetryableFails != 0 || !health.CooldownUntil.IsZero() || health.HalfOpen {
		t.Fatalf("a successful probe must fully recover the member, got %+v", health)
	}

	next, err := pool.Select("video_analysis")
	if err != nil {
		t.Fatal(err)
	}
	next.Done(errSyntheticRetryable)
	health, _ = pool.HealthState("only", "video_analysis")
	if health.RetryableFails != 1 {
		t.Fatalf("after recovery the first failure must count 1 again, got %d", health.RetryableFails)
	}
}

// A configured route with no enabled members keeps the established
// unavailable semantics: Select reports ErrNoAvailable.
func TestRouteWithoutEnabledMembersIsUnavailable(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	pool, err := NewPool([]Member{
		{Name: "disabled", Capabilities: []Capability{"video_analysis"}, Enabled: false},
	}, Options{Now: clock.Now, BaseCooldown: time.Second, MaxCooldown: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Select("video_analysis"); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("Select on a fully disabled route must return ErrNoAvailable, got %v", err)
	}
}
