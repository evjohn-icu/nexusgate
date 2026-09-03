package providerpool

import (
	"errors"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/providers/common"
)

// The status probe is duck-typed, but the error every provider adapter actually
// returns exposes its status as a *field*, and a field cannot satisfy an
// interface. So the probe returned 0 for every real provider failure and
// classification fell through to matching digits in "provider returned HTTP
// %d". That was invisible: the common statuses came out right by accident.
//
// This test imports a transport package only to name the concrete error the
// pool is asked to classify in production; providerpool itself still has no
// transport dependency.
func TestStatusCodeReadsProviderStatusError(t *testing.T) {
	if got := statusCode(&common.StatusError{StatusCode: 429, Body: "slow down"}); got != 429 {
		t.Fatalf("statusCode(*common.StatusError) = %d, want 429 — the probe cannot see the status and is guessing from error text", got)
	}
}

// Agreement between the status and the wording is what hid the defect. These
// cases pull the two apart, so only a classifier that reads the status can pass
// them.
func TestClassifyFailureTrustsStatusOverMessageText(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want FailureClass
	}{
		// No digits and no retry vocabulary anywhere in the message ("provider
		// status error"): nothing but the status can produce this answer.
		{name: "silent 429", err: statusError{code: 429}, want: Retryable},
		// A relay that echoes an auth-flavoured body under a 429 is still rate
		// limiting, not refusing the key.
		{name: "429 with permanent-sounding body", err: &common.StatusError{StatusCode: 429, Body: "unauthorized: forbidden by upstream"}, want: Retryable},
		// Before the status probe was fixed, 402 matched none of the substring
		// lists this package then used, fell to the closing NonRetryable, and
		// killed the whole channel for the job.
		{name: "402 is about the key", err: &common.StatusError{StatusCode: 402, Body: "insufficient balance"}, want: MemberSpent},
		// The body asks to be retried; the status says the request itself was
		// wrong, and no other key would answer differently.
		{name: "400 with transient-sounding body", err: &common.StatusError{StatusCode: 400, Body: "temporarily unavailable, please try again"}, want: NonRetryable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyFailure(tt.err); got != tt.want {
				t.Fatalf("ClassifyFailure(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// The substring fallback below the probe is not dead — adapters that never
// spoke HTTP still reach it — but no error that carries a status may reach it
// any more, or the digit matching gets a second vote on cases the status
// already settled. Every code is checked against a body written to argue for
// the opposite answer.
func TestStatusBearingErrorsNeverReachTheSubstringFallback(t *testing.T) {
	const argueRetryable = "temporarily unavailable: 503 service unavailable, please retry"
	const arguePermanent = "unauthorized: bad request 400, invalid request"

	for code := 400; code <= 599; code++ {
		want := Retryable
		switch {
		case code == 401 || code == 402 || code == 403:
			want = MemberSpent
		case code >= 400 && code <= 499 && code != 408 && code != 429:
			want = NonRetryable
		}
		body := arguePermanent
		if want != Retryable {
			body = argueRetryable
		}
		if got := ClassifyFailure(&common.StatusError{StatusCode: code, Body: body}); got != want {
			t.Fatalf("ClassifyFailure(HTTP %d with body %q) = %v, want %v — the message text is still getting a vote", code, body, got, want)
		}
	}
}

// A key the provider refuses is not a key that works later, so a cooldown is
// the wrong tool: the member has to leave the route entirely, or every
// selection keeps paying for the same refusal.
func TestSpentMemberRetiresItselfAndLeavesTheRouteToTheNextKey(t *testing.T) {
	clock := &fakeClock{now: time.Unix(500, 0)}
	pool, err := NewPool([]Member{
		{Name: "spent", Capabilities: []Capability{"vision"}, Enabled: true},
		{Name: "live", Capabilities: []Capability{"vision"}, Enabled: true},
	}, Options{Now: clock.Now, BaseCooldown: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	first, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	if first.Name() != "spent" {
		t.Fatalf("first selection = %q, want spent", first.Name())
	}
	first.Done(&common.StatusError{StatusCode: 402, Body: "insufficient balance"})

	if enabled, known := pool.EnabledState("spent"); !known || enabled {
		t.Fatalf("EnabledState(spent) = (%v, %v), want a known, retired member", enabled, known)
	}
	for i := 0; i < 3; i++ {
		lease, err := pool.Select("vision")
		if err != nil {
			t.Fatalf("selection %d: a spent key must not take the route down with it: %v", i, err)
		}
		if lease.Name() != "live" {
			t.Fatalf("selection %d landed on %q; the retired key is still being paid for", i, lease.Name())
		}
		lease.Done(nil)
	}

	// Retirement is runtime state an operator can undo, and undoing it must not
	// hand back a clean bill of health it never earned.
	if !pool.SetEnabled("spent", true) {
		t.Fatal("SetEnabled could not restore the retired member")
	}
	if enabled, _ := pool.EnabledState("spent"); !enabled {
		t.Fatal("restored member is still retired")
	}
}

// The distinction is the whole point: a request the provider rejects must not
// cost the operator a key.
func TestRequestLevelFailureDoesNotRetireMember(t *testing.T) {
	pool, err := NewPool([]Member{
		{Name: "only", Capabilities: []Capability{"vision"}, Enabled: true},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	lease.Done(&common.StatusError{StatusCode: 400, Body: "invalid request"})

	if enabled, known := pool.EnabledState("only"); !known || !enabled {
		t.Fatalf("EnabledState(only) = (%v, %v); a malformed request retired the key", enabled, known)
	}
	next, err := pool.Select("vision")
	if err != nil {
		t.Fatalf("member left the route after a request-level failure: %v", err)
	}
	next.Done(nil)
}

// Retirement leaves health exactly as it stands, for the same reason
// SetEnabled does. In particular it is not a cooldown in disguise: an operator
// who fixes the key and re-enables the member gets it back at once, and a
// member that was already cooling comes back still cooling.
func TestRetirementNeitherAddsNorErasesCooldown(t *testing.T) {
	clock := &fakeClock{now: time.Unix(600, 0)}
	pool, err := NewPool([]Member{
		{Name: "a", Capabilities: []Capability{"vision"}, Enabled: true},
	}, Options{Now: clock.Now, BaseCooldown: time.Minute, MaxCooldown: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	spent, err := pool.Select("vision")
	if err != nil {
		t.Fatal(err)
	}
	spent.Done(&common.StatusError{StatusCode: 401, Body: "invalid api key"})
	if !pool.SetEnabled("a", true) {
		t.Fatal("SetEnabled could not restore the retired member")
	}
	restored, err := pool.Select("vision")
	if err != nil {
		t.Fatalf("a restored member must be usable immediately, not left cooling: %v", err)
	}
	restored.Done(errors.New("connection reset by peer"))

	// Now it is cooling. Retiring and restoring it must not launder that away.
	pool.SetEnabled("a", false)
	pool.SetEnabled("a", true)
	clock.Advance(30 * time.Second)
	if lease, err := pool.Select("vision"); !errors.Is(err, ErrNoAvailable) {
		if err == nil {
			lease.Done(nil)
		}
		t.Fatalf("select during cooldown err=%v; disable/enable erased an active cooldown", err)
	}
}
