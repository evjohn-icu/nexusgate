package providerpool

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func TestPoolSelectsLeastInflightWithRoundRobinTiesAndPreservesCapabilityRoute(t *testing.T) {
	pool, err := NewPool([]Member{
		{Name: "vision-a", Capabilities: []Capability{"vision"}, Enabled: true},
		{Name: "vision-b", Capabilities: []Capability{"vision"}, Enabled: true},
		{Name: "vision-c", Capabilities: []Capability{"vision"}, Enabled: true},
		{Name: "text-only", Capabilities: []Capability{"text"}, Enabled: true},
		{Name: "vision-disabled", Capabilities: []Capability{"vision"}, Enabled: false},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	assertNames(t, pool.Route("vision"), []string{"vision-a", "vision-b", "vision-c", "vision-disabled"})
	assertNames(t, pool.Route("text"), []string{"text-only"})

	first, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Name(); got != "vision-a" {
		t.Fatalf("first route member = %q, want vision-a", got)
	}
	first.Done(nil)

	second, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Name(); got != "vision-b" {
		t.Fatalf("round-robin tie member = %q, want vision-b", got)
	}

	third, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	if got := third.Name(); got != "vision-c" {
		t.Fatalf("least-inflight member = %q, want vision-c", got)
	}
	second.Done(nil)
	third.Done(nil)

	text, err := pool.Select("text")
	if err != nil {
		t.Fatal(err)
	}
	if got := text.Name(); got != "text-only" {
		t.Fatalf("capability crossed route to %q", got)
	}
	text.Done(nil)
}

func TestPoolDoesNotSelectCooledMember(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	pool, err := NewPool([]Member{
		{Name: "a", Capability: "vision", Enabled: true},
		{Name: "b", Capability: "vision", Enabled: true},
	}, Options{Now: clock.Now, BaseCooldown: 10 * time.Second, MaxCooldown: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	failed, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	failed.Done(context.DeadlineExceeded)

	available, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	if got := available.Name(); got != "b" {
		t.Fatalf("cooled member selected as %q, want b", got)
	}
	available.Done(nil)

	clock.Advance(9 * time.Second)
	stillAvailable, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	if got := stillAvailable.Name(); got != "b" {
		t.Fatalf("cooled member became eligible early as %q, want b", got)
	}
	stillAvailable.Done(nil)
}

func TestPoolAllowsOnlyOneHalfOpenProbeAndDoublesCooldown(t *testing.T) {
	clock := &fakeClock{now: time.Unix(200, 0)}
	pool, err := NewPool([]Member{{Name: "a", Capability: "vision", Enabled: true}}, Options{
		Now:          clock.Now,
		BaseCooldown: 5 * time.Second,
		MaxCooldown:  12 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	initial, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	initial.Done(errors.New("temporarily unavailable"))

	clock.Advance(5 * time.Second)
	probe, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	if got := probe.Name(); got != "a" {
		t.Fatalf("half-open probe selected %q, want a", got)
	}
	if _, err := pool.Select("vision"); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("second concurrent half-open probe error = %v, want ErrNoAvailable", err)
	}
	probe.Done(errors.New("temporarily unavailable"))

	clock.Advance(9 * time.Second)
	if _, err := pool.Select("vision"); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("provider reopened before bounded second cooldown: %v", err)
	}
	clock.Advance(time.Second)

	probe, err = pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	probe.Done(errors.New("temporarily unavailable"))
	clock.Advance(11 * time.Second)
	if _, err := pool.Select("vision"); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("provider reopened before bounded maximum cooldown: %v", err)
	}
	clock.Advance(time.Second)
	probe, err = pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	probe.Done(nil)
	if _, err := pool.Select("vision"); err != nil {
		t.Fatalf("successful half-open probe did not restore provider: %v", err)
	}
}

func TestPoolNonRetryableFailureDoesNotCooldownMember(t *testing.T) {
	clock := &fakeClock{now: time.Unix(300, 0)}
	pool, err := NewPool([]Member{{Name: "a", Capability: "vision", Enabled: true}}, Options{
		Now:          clock.Now,
		BaseCooldown: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	lease, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	// 400: a fact about the request. The member is fine and must stay hot —
	// unlike 401/402/403, which are facts about the key (see the spent-member
	// tests).
	lease.Done(statusError{code: 400})

	lease, err = pool.Select("vision")
	if err != nil {
		t.Fatalf("non-retryable failure cooled member: %v", err)
	}
	lease.Done(nil)
}

func TestPoolHealthIsIndependentAcrossCapabilities(t *testing.T) {
	clock := &fakeClock{now: time.Unix(400, 0)}
	pool, err := NewPool([]Member{{
		Name:         "shared",
		Capabilities: []Capability{"vision", "text"},
		Enabled:      true,
	}}, Options{Now: clock.Now, BaseCooldown: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	vision, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	vision.Done(errors.New("temporarily unavailable"))

	text, err := pool.Select("text")
	if err != nil {
		t.Fatalf("vision cooldown leaked into text capability: %v", err)
	}
	text.Done(nil)
}

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want FailureClass
	}{
		{name: "rate limited", err: statusError{code: 429}, want: Retryable},
		{name: "timeout", err: context.DeadlineExceeded, want: Retryable},
		{name: "network", err: &net.DNSError{Err: "temporary DNS failure", IsTemporary: true}, want: Retryable},
		{name: "server", err: statusError{code: 503}, want: Retryable},
		{name: "unauthorized", err: statusError{code: 401}, want: MemberSpent},
		{name: "payment required", err: statusError{code: 402}, want: MemberSpent},
		{name: "forbidden", err: statusError{code: 403}, want: MemberSpent},
		{name: "bad request", err: statusError{code: 400}, want: NonRetryable},
		{name: "not found", err: statusError{code: 404}, want: NonRetryable},
		{name: "unprocessable", err: statusError{code: 422}, want: NonRetryable},
		{name: "configuration", err: errors.New("provider is not configured"), want: NonRetryable},
		{name: "schema", err: errors.New("response schema validation failed"), want: NonRetryable},
		// Without a status there is only wording, and wording is not enough to
		// retire a key: these stay NonRetryable rather than joining the 401/403
		// cases above.
		{name: "unauthorized wording only", err: errors.New("aligner reports unauthorized"), want: NonRetryable},
		{name: "forbidden wording only", err: errors.New("forbidden by local policy"), want: NonRetryable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyFailure(tt.err); got != tt.want {
				t.Fatalf("ClassifyFailure(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

type statusError struct {
	code int
}

func (e statusError) Error() string { return "provider status error" }

func (e statusError) StatusCode() int { return e.code }

func assertNames(t *testing.T, members []Member, want []string) {
	t.Helper()
	if len(members) != len(want) {
		t.Fatalf("route length = %d, want %d: %+v", len(members), len(want), members)
	}
	for i := range want {
		if members[i].Name != want[i] {
			t.Fatalf("route[%d] = %q, want %q", i, members[i].Name, want[i])
		}
	}
}

// MaxInflight is what lets one provider be configured with several keys that are
// rate limited independently. It round-tripped through SQLite, the admin API and
// the UI for several releases while never reaching selection, so a member
// configured with a cap of one still accepted unlimited concurrent work.
func TestPoolCapsConcurrentLeasesPerMember(t *testing.T) {
	pool, err := NewPool([]Member{
		{Name: "capped", Capabilities: []Capability{"vision"}, Enabled: true, MaxInflight: 1},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	held, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Select("vision"); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("second lease on a MaxInflight=1 member err=%v, want ErrNoAvailable", err)
	}
	held.Done(nil)

	after, err := pool.Select("vision")
	if err != nil {
		t.Fatalf("member should be selectable once the lease completes: %v", err)
	}
	after.Done(nil)
}

func TestPoolSpillsToNextMemberWhenSaturated(t *testing.T) {
	pool, err := NewPool([]Member{
		{Name: "primary", Capabilities: []Capability{"vision"}, Enabled: true, MaxInflight: 1},
		{Name: "spare", Capabilities: []Capability{"vision"}, Enabled: true, MaxInflight: 1},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	if first.Name() == second.Name() {
		t.Fatalf("both leases landed on %q despite MaxInflight=1", first.Name())
	}
	if _, err := pool.Select("vision"); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("third lease err=%v, want ErrNoAvailable", err)
	}
	first.Done(nil)
	second.Done(nil)
}

// A zero MaxInflight has to stay unlimited: existing routes are built from
// members that never set the field.
func TestPoolTreatsZeroMaxInflightAsUnlimited(t *testing.T) {
	pool, err := NewPool([]Member{
		{Name: "uncapped", Capabilities: []Capability{"vision"}, Enabled: true},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := pool.Select("vision"); err != nil {
			t.Fatalf("lease %d: %v", i, err)
		}
	}
}

func TestPoolWeightBiasesSelectionWithoutDuplicatingTheRoute(t *testing.T) {
	pool, err := NewPool([]Member{
		{Name: "heavy", Capabilities: []Capability{"vision"}, Enabled: true, Weight: 3},
		{Name: "light", Capabilities: []Capability{"vision"}, Enabled: true, Weight: 1},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Route stays deduplicated even though selection is weighted.
	assertNames(t, pool.Route("vision"), []string{"heavy", "light"})

	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		lease, err := pool.Select("vision")
		if err != nil {
			t.Fatal(err)
		}
		counts[lease.Name()]++
		lease.Done(nil)
	}
	if counts["heavy"] <= counts["light"] {
		t.Fatalf("weighted selection did not favour the heavier member: %v", counts)
	}
}
