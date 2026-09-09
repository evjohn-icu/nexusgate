package app

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/providerchannels"
	"github.com/evjohn-icu/nexusgate/internal/providerpool"
	"github.com/evjohn-icu/nexusgate/internal/providers/common"
)

// offsetClock reports real wall-clock time plus a mutable offset. It exists
// so a test can simulate real time passing for the providerpool's cooldown
// bookkeeping (routeUnconfirmedDeferral is 5 minutes; sleeping that for real
// would make the test glacial) while the rest of the test still runs against
// genuine elapsed time -- Advance(0) behaves exactly like time.Now, so the
// first phase of a test needs no special handling at all.
type offsetClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *offsetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *offsetClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// wideChannelRepo is the real repository (see newQueuedIndexJob) with
// RebuildSearch routed through a real providerchannels.Executor over a real
// providerpool.Pool, instead of a fixed injected error. Every member on the
// channel fails the same retryable way (a 500), so this drives the actual
// member-selection, cooldown and confirmation machinery that
// routeFailingEverywhere depends on -- not a stand-in for it. Every attempt
// is also recorded (lease-call index, member ID) so the test can show the
// actual sequence rather than just the settled outcome.
type wideChannelRepo struct {
	*indexFailureRepo
	executor *providerchannels.Executor

	mu       sync.Mutex
	calls    int
	attempts []string
}

func (r *wideChannelRepo) RebuildSearch(ctx context.Context, _ string) error {
	r.mu.Lock()
	r.calls++
	lease := r.calls
	r.mu.Unlock()
	return r.executor.Execute(ctx, providerchannels.CapabilityVideoAnalysis, func(_ context.Context, invocation providerchannels.Invocation) error {
		r.mu.Lock()
		r.attempts = append(r.attempts, fmt.Sprintf("lease%d:%s", lease, invocation.MemberID))
		r.mu.Unlock()
		return &common.StatusError{StatusCode: 500, Body: "boom"}
	})
}

// TestWideChannelDefersBrieflyRatherThanFailingPermanently drives a
// five-member channel where every member fails retryably, through the real
// executor, the real providerpool, and the real sqlite-backed job lease/retry
// machinery (max_attempts=3, attemptsPerChannel=3).
//
// Confirming a route exhausted needs each enabled member to show
// confirmedRetryableFailures (2) consecutive retryable failures while its
// cooldown is still in effect -- and a member reaches that only by failing,
// having its own cooldown actually elapse, being selected again, and failing
// a second time. That is 2*N failures across the whole channel. The job's
// entire budget is three leases of attemptsPerChannel(3) member calls each --
// nine calls -- before max_attempts stops it cold. At five members, 2*5=10 >
// 9: there are not enough attempts left in the job's own budget to ever
// confirm the route exhausted, so providerchannels.ErrRouteExhausted is never
// returned.
//
// Before the fix, that meant the job burned all three attempts on an
// ordinary retryable classification and died permanently -- the opposite of
// what CLAUDE.md's pipeline rules intend for "nothing about this job is
// wrong" failures. Now Execute returns ErrRouteUnconfirmed instead of a bare
// retryable error once routeFailingEverywhere declines to confirm
// exhaustion, and the pipeline's third and final attempt turns that into a
// short defer (routeUnconfirmedDeferral) rather than a terminal failure. What
// happens on the *next* lease -- whether confirmation ever completes, and how
// long that takes -- is TestWideChannelEventuallyConfirmsExhaustionInsteadOfLoopingForever's
// job, not this one's; this test only pins the shape of the first defer.
func TestWideChannelDefersBrieflyRatherThanFailingPermanently(t *testing.T) {
	members := make([]providerchannels.Member, 5)
	for i := range members {
		members[i] = providerchannels.Member{ID: fmt.Sprintf("member-%d", i), Enabled: true}
	}
	clock := &offsetClock{}
	executor, err := providerchannels.NewExecutor([]providerchannels.Channel{{
		ID: "wide", ProviderName: "provider-a", Enabled: true,
		Capabilities: []providerchannels.Capability{providerchannels.CapabilityVideoAnalysis},
		Members:      members,
	}}, providerpool.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}

	base := newQueuedIndexJob(t, nil)
	repo := &wideChannelRepo{indexFailureRepo: base, executor: executor}
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)

	before := time.Now()
	job := singleJob(t, base, pipeline, func(job domain.Job) bool {
		return job.Terminal || job.DeferredReason != ""
	})

	repo.mu.Lock()
	sequence := append([]string(nil), repo.attempts...)
	calls := repo.calls
	repo.mu.Unlock()

	t.Logf("observed attempt sequence across %d leases: %v", calls, sequence)
	t.Logf("settled job: state=%s terminal=%v attempts=%d/%d deferred_reason=%q last_error=%q run_after=%s",
		job.State, job.Terminal, job.AttemptCount, job.MaxAttempts, job.DeferredReason, job.LastError, job.RunAfter)

	if job.DeferredReason == domain.JobDeferProviderRouteExhausted {
		t.Fatalf("route exhaustion was confirmed and got the full five-hour park -- with 5 members that should be mathematically unreachable within a 9-call budget: %+v (sequence=%v)", job, sequence)
	}
	if job.Terminal || job.State != domain.JobPending {
		t.Fatalf("the job must not die permanently for want of confirmable route exhaustion: %+v (sequence=%v)", job, sequence)
	}
	if job.DeferredReason != domain.JobDeferProviderRouteUnconfirmed {
		t.Fatalf("expected the route-unconfirmed defer reason once the budget ran out unconfirmed, got %+v (sequence=%v)", job, sequence)
	}
	// The first two leases spend their attempts normally (ordinary retryable
	// classification); only the third, final one is handed back.
	if job.AttemptCount != job.MaxAttempts-1 {
		t.Fatalf("expected exactly the final attempt handed back (%d of %d), got %d: %+v", job.MaxAttempts-1, job.MaxAttempts, job.AttemptCount, job)
	}
	if calls != job.MaxAttempts {
		t.Fatalf("expected exactly %d leases (one Execute call per lease), got %d: %v", job.MaxAttempts, calls, sequence)
	}
	if job.RunAfter.After(before.Add(routeUnconfirmedDeferral + time.Minute)) {
		t.Fatalf("route-unconfirmed defer must not park anywhere near the five-hour confirmed-exhaustion wait: run_after=%s (started %s)", job.RunAfter, before)
	}
}

// TestWideChannelEventuallyConfirmsExhaustionInsteadOfLoopingForever drives
// the residual TestWideChannelDefersBrieflyRatherThanFailingPermanently
// leaves at its first defer further: not one extra defer cycle, but as many
// as it takes, to answer whether the loop is bounded or indefinite.
//
// routeFailingEverywhere used to also require that each member's cooldown
// *still be in effect* at the instant it was asked, on top of
// confirmedRetryableFailures. The pool's cooldown caps at 30 seconds
// (MaxCooldown); routeUnconfirmedDeferral is 5 minutes. By the time a
// deferred job was naturally due again, every member touched in a *previous*
// lease had an elapsed cooldown, so its accumulated RetryableFails count --
// real evidence, never contradicted by a success -- stopped counting the
// moment the clock, not a new attempt, moved past it. Only the up-to-three
// members touched in the *current* lease had a live cooldown at check time,
// and attemptsPerChannel(3) < 2*N(=10) for a five-member route, so no single
// lease could ever supply enough live evidence on its own: the job cycled
// through provider_route_unconfirmed forever, an unmetered, indefinite
// trickle of calls against a route that was, in fact, already dead.
//
// routeFailingEverywhere no longer requires the cooldown to still be
// in effect -- RetryableFails persists as evidence until something actually
// contradicts it (a success, or a non-retryable failure) -- so this now
// converges: this test requires it to confirm, with the full five-hour park,
// well within cyclesUnderTest.
func TestWideChannelEventuallyConfirmsExhaustionInsteadOfLoopingForever(t *testing.T) {
	// 8 gives comfortable margin over the 4-cycle bound hand-derived from the
	// round-robin touch schedule (lease1 touches members 0,1,2; lease2 touches
	// 3,4,0; lease3 touches 1,2,3; lease4 touches 4,0,1 -- every member has
	// accumulated 2 failures by the end of lease4) without making a failing
	// run (the pre-fix case) take unreasonably long.
	const cyclesUnderTest = 8

	members := make([]providerchannels.Member, 5)
	for i := range members {
		members[i] = providerchannels.Member{ID: fmt.Sprintf("member-%d", i), Enabled: true}
	}
	clock := &offsetClock{}
	executor, err := providerchannels.NewExecutor([]providerchannels.Channel{{
		ID: "wide", ProviderName: "provider-a", Enabled: true,
		Capabilities: []providerchannels.Capability{providerchannels.CapabilityVideoAnalysis},
		Members:      members,
	}}, providerpool.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}

	base := newQueuedIndexJob(t, nil)
	repo := &wideChannelRepo{indexFailureRepo: base, executor: executor}
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)

	before := time.Now()
	job := singleJob(t, base, pipeline, func(job domain.Job) bool {
		return job.Terminal || job.DeferredReason != ""
	})
	if job.Terminal {
		t.Fatalf("job must not die permanently on lease 1: %+v", job)
	}
	if job.DeferredReason != domain.JobDeferProviderRouteUnconfirmed {
		t.Fatalf("expected the first lease to end unconfirmed, got %+v", job)
	}

	confirmed := false
	cyclesTaken := 0
	for cycle := 1; cycle <= cyclesUnderTest; cycle++ {
		clock.Advance(routeUnconfirmedDeferral + time.Second)
		if _, err := base.ResumeDeferredJobs(context.Background(), domain.JobDeferProviderRouteUnconfirmed); err != nil {
			t.Fatal(err)
		}
		if _, err := pipeline.RunUntilIdle(context.Background()); err != nil {
			t.Fatal(err)
		}
		jobs, err := base.ListJobs(context.Background(), 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("cycle %d: jobs=%+v err=%v", cycle, jobs, err)
		}
		job = jobs[0]
		if job.Terminal {
			t.Fatalf("cycle %d: job must never die permanently for want of confirmable route exhaustion: %+v", cycle, job)
		}
		if job.DeferredReason == domain.JobDeferProviderRouteExhausted {
			confirmed = true
			cyclesTaken = cycle
			break
		}
		if job.DeferredReason != domain.JobDeferProviderRouteUnconfirmed {
			t.Fatalf("cycle %d: unexpected non-terminal state: %+v", cycle, job)
		}
	}

	repo.mu.Lock()
	sequence := append([]string(nil), repo.attempts...)
	repo.mu.Unlock()

	if !confirmed {
		t.Fatalf("route exhaustion was never confirmed after %d defer cycles (%d total leases) -- routeFailingEverywhere's live-cooldown requirement makes confirmation unreachable on a %d-member channel retried every %s: sequence=%v",
			cyclesUnderTest, len(sequence)/3+1, len(members), routeUnconfirmedDeferral, sequence)
	}
	t.Logf("confirmed exhaustion after %d additional defer cycle(s); full sequence=%v", cyclesTaken, sequence)
	if job.RunAfter.Before(before.Add(providerRouteDeferral - time.Minute)) {
		t.Fatalf("confirmed exhaustion must still get the full five-hour park, got run_after=%s (started %s)", job.RunAfter, before)
	}
}
