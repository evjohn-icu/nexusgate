package providerpool

import (
	"errors"
	"testing"
	"time"
)

// TestSelectDuringAShortCooldownReturnsErrNoAvailableNotAVerdict is the
// confirmation the audited defect asked for: put a single-member pool into a
// cooldown short enough that it is a wall-clock blip, then ask Select what
// comes back while it is still cooling and again once the cooldown has
// cleared.
//
// What Select returns while cooling is exactly ErrNoAvailable -- the pool
// itself carries no notion of "exhausted", only "not eligible right now". It
// says nothing about whether one member cooled for a second or every member
// on a route was permanently retired; that distinction is made one layer up,
// in providerchannels.Executor, which is where the reported bug actually
// lived (routeFailingEverywhere treated "eligible right now" and "will never
// be eligible again" as the same fact). This test exists to pin the pool's
// half of the contract so that layer cannot silently start assuming Select
// answers a question it was never designed to answer.
func TestSelectDuringAShortCooldownReturnsErrNoAvailableNotAVerdict(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	pool, err := NewPool([]Member{
		{Name: "only", Capabilities: []Capability{"video_analysis"}, Enabled: true},
	}, Options{Now: clock.Now, BaseCooldown: time.Second, MaxCooldown: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	lease, err := pool.Select("video_analysis")
	if err != nil {
		t.Fatalf("first Select on a fresh pool must succeed, got %v", err)
	}
	// A single transient failure: a timeout, a 503, a dropped connection.
	// Not a spent key, not a 401/402/403.
	lease.Done(HTTPError{Code: 503, Err: errors.New("upstream unavailable")})

	// Immediately after: the one-second cooldown has not elapsed. Report
	// exactly what Select returns.
	_, err = pool.Select("video_analysis")
	if err == nil {
		t.Fatal("Select on a pool whose only member is cooling must fail, got nil error")
	}
	if !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("Select while cooling returned %v (%T), want ErrNoAvailable -- the pool must not itself report exhaustion", err, err)
	}
	t.Logf("Select during cooldown returned: %v", err)

	// The cooldown is bounded: once it elapses the same member is eligible
	// again, exactly as the audited chain assumed. This is the fact that
	// providerchannels.Executor must respect instead of collapsing this
	// state into the same answer as a permanently retired route.
	clock.Advance(2 * time.Second)
	probe, err := pool.Select("video_analysis")
	if err != nil {
		t.Fatalf("Select after the cooldown elapsed must succeed (a half-open probe), got %v", err)
	}
	if probe.Name() != "only" {
		t.Fatalf("probe selected %q, want the only member", probe.Name())
	}
}
