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
// short defer (routeUnconfirmedDeferral) rather than a terminal failure.
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

	// The residual this leaves: with N=5 > attemptsPerChannel(3), the
	// post-defer cycle never confirms either. DeferJob handed attempt_count
	// back to 2, so the *next* lease is the job's last again -- one lease, up
	// to three member calls, then another short defer if still unconfirmed.
	// The pool's cooldowns cap at 30 seconds, far shorter than
	// routeUnconfirmedDeferral (5 minutes), so by the time the job is
	// naturally due again every member's cooldown from this round has
	// already elapsed and confirmation has to start over from nothing --
	// three fresh attempts cannot supply the two failures each of five
	// members needs. Advancing the pool's own clock by the real
	// routeUnconfirmedDeferral wait (rather than sleeping it, or than
	// resuming instantly, which would leave lease 3's cooldowns still active
	// and let the resumed lease piggyback on them -- an artifact of the test
	// harness, not what happens on a wall clock) reproduces that gap
	// faithfully: run_after is pulled forward the way an operator's
	// resume-now button or the job's own natural due time would, and one
	// more pass shows exactly one more lease, touching at most three of the
	// five members, landing right back in the same deferred state -- not
	// confirmed, not terminal, not idle. That is still strictly better than
	// the permanent failure this fix replaces -- the job stays visible and
	// keeps trying -- but it is not confirmation, and nothing in this fix
	// makes it one.
	clock.Advance(routeUnconfirmedDeferral + time.Second)
	if _, err := base.ResumeDeferredJobs(context.Background(), domain.JobDeferProviderRouteUnconfirmed); err != nil {
		t.Fatal(err)
	}
	callsBeforeResume := calls
	if _, err := pipeline.RunUntilIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobsAfterResume, err := base.ListJobs(context.Background(), 10)
	if err != nil || len(jobsAfterResume) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobsAfterResume, err)
	}
	after := jobsAfterResume[0]
	repo.mu.Lock()
	sequenceAfterResume := append([]string(nil), repo.attempts...)
	callsAfterResume := repo.calls
	repo.mu.Unlock()
	t.Logf("post-resume: %d additional lease(s), sequence now %v", callsAfterResume-callsBeforeResume, sequenceAfterResume)
	t.Logf("post-resume job: state=%s terminal=%v attempts=%d/%d deferred_reason=%q run_after=%s",
		after.State, after.Terminal, after.AttemptCount, after.MaxAttempts, after.DeferredReason, after.RunAfter)
	if callsAfterResume-callsBeforeResume != 1 {
		t.Fatalf("expected exactly one more lease after resuming the defer, got %d", callsAfterResume-callsBeforeResume)
	}
	if after.Terminal {
		t.Fatalf("the residual cycle must still never fail the job permanently: %+v", after)
	}
	if after.DeferredReason != domain.JobDeferProviderRouteUnconfirmed {
		t.Fatalf("expected the job to land back in the same unconfirmed defer, not confirm exhaustion or go idle: %+v", after)
	}
}
