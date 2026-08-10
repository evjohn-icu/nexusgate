package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
)

// LibrarySupervisor is the unattended half of `timingdex serve`: it rescans
// every library root on a timer and then drains the queue, so footage dropped
// onto a share is indexed without anyone running `root scan` and
// `pipeline run`.
//
// Polling, deliberately, rather than fsnotify. The target deployments keep
// footage on SMB/NFS shares, where inotify either does not fire for changes
// made by another host or is not available at all, and the Docker media bind
// uses rslave propagation so a share can appear and disappear underneath the
// bind while the Hub is running. A watcher that misses one of those events
// fails silently — the footage is simply never indexed, and nothing says so. A
// timer cannot miss anything: the worst case is that discovery is one interval
// late.
//
// The loop is deliberately dull. One goroutine, one pass at a time, the pass
// body running inline rather than in a goroutine of its own, so "scans must not
// stack" and "the pipeline pass must stop when the Hub does" are properties of
// the shape rather than of a lock somebody has to keep correct.
type LibrarySupervisor struct {
	service *Service
	enabled bool
	// interval is set once in newLibrarySupervisor and never written
	// afterwards; reads without holding s.mu are safe.
	interval time.Duration

	// ticks replaces the internal time.Ticker. It is nil in every real
	// supervisor — only this package's tests set it, so that the loop can be
	// driven a tick at a time instead of by waiting out a real interval.
	ticks <-chan time.Time
	// pass is the body of one iteration, defaulted to scanAndRun. Tests
	// substitute a body they can make slow or count, because what the loop
	// guarantees (never two bodies at once, never a body after cancellation) is
	// a property of the loop and not of the scanning it happens to do. It is
	// written once at construction and read only by the loop goroutine.
	pass func(context.Context)

	mu     sync.Mutex
	status LibrarySupervisorStatus

	// unavailablePasses counts consecutive passes an unavailable root has been
	// skipped, so a remounted share is re-attempted without a manual scan.
	// Guarded by mu; reset to zero whenever the root is actually attempted.
	unavailablePasses map[string]int
}

// Outcomes of one supervisor pass, as reported to /progress. They are stable
// identifiers rather than prose because the page renders them.
const (
	SupervisorOutcomeScanned      = "scanned"
	SupervisorOutcomeHeldOffPeak  = "held_off_peak"
	SupervisorOutcomePipelineBusy = "pipeline_busy"
	SupervisorOutcomeError        = "error"
)

// defaultLibrarySupervisorInterval is a compromise between noticing a dropped
// clip reasonably soon and not walking a NAS tree constantly.
const defaultLibrarySupervisorInterval = 15 * time.Minute

// minLibrarySupervisorInterval floors a mistyped interval. A scan walks every
// root in full, so a value of 0 or 1 read as "as fast as possible" would keep a
// network share permanently busy for no benefit.
const minLibrarySupervisorInterval = time.Minute

// unavailableRetryEveryPasses is how many consecutive passes an unavailable
// root is skipped before the supervisor attempts it anyway. At the default
// 15-minute interval that is one re-attempt per hour — frequent enough that an
// overnight outage self-recovers the same morning, rare enough that a dead
// mount is not walked every pass.
const unavailableRetryEveryPasses = 4

// LibrarySupervisorStatus is what an operator who turned this on needs in order
// to tell a live loop from one that quietly died: whether it is running, when
// it last ran, and when it will run next.
type LibrarySupervisorStatus struct {
	Enabled         bool `json:"enabled"`
	Running         bool `json:"running"`
	Scanning        bool `json:"scanning"`
	IntervalSeconds int  `json:"interval_seconds"`
	Passes          int  `json:"pass_count"`

	LastPassAt *time.Time `json:"last_pass_at,omitempty"`
	NextPassAt *time.Time `json:"next_pass_at,omitempty"`

	LastOutcome string `json:"last_outcome,omitempty"`
	// HeldUntil is set when the last pass did nothing because the throttle's
	// off-peak window was shut. Without it a supervisor that is working exactly
	// as configured is indistinguishable from one that has stopped.
	HeldUntil       *time.Time `json:"held_until,omitempty"`
	RootsScanned    int        `json:"roots_scanned"`
	Discovered      int        `json:"discovered"`
	PipelineStarted bool       `json:"pipeline_started"`
	// LastError is the last failure text. The API withholds it from
	// unauthenticated callers, as it does for job failures: error text is where
	// paths and upstream detail surface.
	LastError string `json:"last_error,omitempty"`
}

func newLibrarySupervisor(service *Service, cfg config.LibrarySupervisorConfig) *LibrarySupervisor {
	interval := time.Duration(cfg.ScanIntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = defaultLibrarySupervisorInterval
	}
	if interval < minLibrarySupervisorInterval {
		interval = minLibrarySupervisorInterval
	}
	supervisor := &LibrarySupervisor{service: service, enabled: cfg.Enabled, interval: interval, unavailablePasses: map[string]int{}}
	supervisor.pass = supervisor.scanAndRun
	supervisor.status = LibrarySupervisorStatus{Enabled: cfg.Enabled, IntervalSeconds: int(interval / time.Second)}
	return supervisor
}

// Run drives the loop until ctx is cancelled, and returns only after the pass in
// flight has finished. Run returning is therefore the whole proof that nothing
// outlives shutdown: the supervisor starts no goroutine of its own, and the
// pipeline pass it triggers runs inline here via Service.TryRunPipeline rather
// than through Service.StartPipeline, whose detached goroutine is bound to
// context.Background() and would by design keep making paid Provider calls after
// the server had stopped serving.
//
// A disabled supervisor returns immediately. That is the upgrade path: an
// existing install runs exactly the code it ran before.
func (s *LibrarySupervisor) Run(ctx context.Context) error {
	if s == nil || !s.enabled {
		return nil
	}
	// Two loops would be two scans and two pipeline passes competing for the
	// same disk, which is the exact failure the single-pass guard downstream
	// exists to prevent. Refuse rather than quietly double the load.
	if !s.claimRun() {
		return errors.New("library supervisor is already running")
	}
	defer s.releaseRun()
	ticks := s.ticks
	if ticks == nil {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	slog.Info("library supervisor started", "interval", s.interval.String())
	defer slog.Info("library supervisor stopped")

	// One pass at startup. A Hub that has just been restarted should pick up
	// what accumulated while it was down rather than sit idle for a whole
	// interval — and after a crash loop that idle window is the whole uptime.
	if !s.runPass(ctx) {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticks:
			// A tick that arrives while a pass is running is dropped, not
			// queued: time.Ticker's channel holds one value and the pass runs
			// inline on this goroutine. So a scan slower than the interval can
			// neither overlap itself nor build up a backlog of catch-up passes.
			if !s.runPass(ctx) {
				return nil
			}
		}
	}
}

// runPass reports whether the loop should continue.
func (s *LibrarySupervisor) runPass(ctx context.Context) bool {
	// select picks at random among ready cases, so a due tick and a cancelled
	// context can arrive together. Shutdown wins: no scan is started and no
	// Provider call is made on the way out.
	if ctx.Err() != nil {
		return false
	}
	s.pass(ctx)
	return ctx.Err() == nil
}

// scanAndRun is one pass: rescan every root, then drain the queue.
func (s *LibrarySupervisor) scanAndRun(ctx context.Context) {
	s.beginPass(time.Now())
	s.endPass(s.onePass(ctx))
}

// passResult is what one pass reports to the status; it exists so the pass
// itself has a single exit shape rather than mutating status from six places.
type passResult struct {
	outcome          string
	heldUntil        *time.Time
	roots            int
	skippedUnhealthy int
	discovered       int
	pipelineStarted  bool
	err              error
}

func (s *LibrarySupervisor) onePass(ctx context.Context) passResult {
	throttle, err := s.service.PipelineThrottle(ctx)
	if err != nil {
		// The same fallback the pipeline uses. An unreadable throttle must not
		// silently be read as "quiet hours are over".
		slog.Warn("library supervisor: pipeline throttle unreadable", "error", err)
		throttle = domain.DefaultPipelineThrottle()
	}
	now := time.Now()
	// The off-peak window is the operator's statement about when this machine
	// may work the library at all, and an unattended loop is exactly the case
	// it was written for. Deliberately the whole pass, not just the scan: a
	// walk of a NAS tree is itself load, and starting a pipeline pass here
	// would begin paid analysis outside the hours that were asked for. Nothing
	// a human triggers is gated — the /progress button and the CLI still run
	// whenever they are told to, and the per-job levers inside RunUntilIdle
	// (size deferral, cooldown, read-rate cap) are untouched. This adds no
	// second rate limit; it only decides whether the unattended pass happens.
	if !throttle.OffPeakOpenAt(now) {
		result := passResult{outcome: SupervisorOutcomeHeldOffPeak}
		if start, ok := throttle.NextOffPeakStart(now); ok {
			result.heldUntil = &start
		}
		return result
	}

	roots, err := s.service.ListLibraryRoots(ctx)
	if err != nil {
		return passResult{outcome: SupervisorOutcomeError, err: err}
	}
	result := passResult{outcome: SupervisorOutcomeScanned}
	for _, root := range roots {
		if ctx.Err() != nil {
			return result
		}
		// A root the last scan found unavailable is not walked at all, not
		// merely gated at the reconciliation step: walking a dead mount is
		// load that can hang for the length of a protocol timeout, and the
		// scan that would succeed in reaching it is exactly the one that
		// re-triggers the missing-asset reconciliation the persisted verdict
		// is pausing. The skip reads only the persisted verdict, so it can
		// never go stale in the retry direction: an unhealthy root that was
		// remounted flips back to healthy on its next attempt, and healthy or
		// unknown roots — including the zero-value state, which predates
		// health tracking — are always scanned. A never-scanned root after a
		// restore must get its chance, or the first verdict after the data
		// dir was rebuilt would have to come from somewhere other than the
		// scan that only runs while the state is unknown.
		if root.HealthState == domain.RootHealthUnavailable {
			// A share that drops overnight and remounts at 09:00 must not stay
			// paused until a human notices: every unavailableRetryEveryPasses
			// passes the root is attempted anyway. The attempt is safe because
			// ScanLibraryRoot's own gate re-verifies health — a root that is
			// still down is marked unavailable again (no reconciliation), one
			// that came back flips to healthy and reconciles normally.
			s.mu.Lock()
			s.unavailablePasses[root.ID]++
			passes := s.unavailablePasses[root.ID]
			s.mu.Unlock()
			if passes < unavailableRetryEveryPasses {
				result.skippedUnhealthy++
				if root.LastHealthyAt != nil {
					slog.Info("library supervisor: root unhealthy; skipping scan", "root", root.ID, "last_healthy_at", root.LastHealthyAt.Format(time.RFC3339))
				} else {
					slog.Info("library supervisor: root unhealthy; skipping scan", "root", root.ID)
				}
				continue
			}
			s.mu.Lock()
			s.unavailablePasses[root.ID] = 0
			s.mu.Unlock()
			slog.Info("library supervisor: re-attempting previously unavailable root", "root", root.ID)
		}
		scan, scanErr := s.service.ScanLibraryRoot(ctx, root.ID)
		if scanErr != nil {
			// One unreachable root must not stop the others. A share vanishing
			// from under an rslave bind is the failure this loop exists to
			// survive, and the other roots on the box are still fine.
			slog.Warn("library supervisor: root scan failed", "root", root.ID, "error", scanErr)
			result.outcome = SupervisorOutcomeError
			result.err = scanErr
			continue
		}
		s.mu.Lock()
		s.unavailablePasses[root.ID] = 0
		s.mu.Unlock()
		result.roots++
		result.discovered += scan.Discovered
	}
	if ctx.Err() != nil {
		return result
	}
	// TryRunPipeline holds the same single-run guard as the /progress button,
	// so an operator-triggered run and the supervisor can never drive the disk
	// at once — whichever got there first keeps going and the other reports
	// busy.
	started, runErr := s.service.TryRunPipeline(ctx)
	result.pipelineStarted = started
	if !started {
		result.outcome = SupervisorOutcomePipelineBusy
		return result
	}
	if runErr != nil && ctx.Err() == nil {
		// A cancelled pass surfaces as an error from the queue; that is
		// shutdown, not a fault, and reporting it would leave a misleading last
		// error on the page forever.
		result.outcome = SupervisorOutcomeError
		result.err = runErr
	}
	return result
}

func (s *LibrarySupervisor) claimRun() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Running {
		return false
	}
	s.status.Running = true
	return true
}

func (s *LibrarySupervisor) releaseRun() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Running = false
	s.status.Scanning = false
}

func (s *LibrarySupervisor) beginPass(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	started := at
	s.status.Scanning = true
	s.status.LastPassAt = &started
	s.status.Passes++
}

func (s *LibrarySupervisor) endPass(result passResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Scanning = false
	s.status.LastOutcome = result.outcome
	s.status.HeldUntil = result.heldUntil
	s.status.RootsScanned = result.roots
	s.status.Discovered = result.discovered
	s.status.PipelineStarted = result.pipelineStarted
	if result.err != nil {
		s.status.LastError = result.err.Error()
		return
	}
	s.status.LastError = ""
}

// Status is a snapshot. The time pointers it carries are only ever replaced,
// never written through, so handing the caller a copy of the struct hands it
// stable values.
func (s *LibrarySupervisor) Status() LibrarySupervisorStatus {
	if s == nil {
		return LibrarySupervisorStatus{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status
	if status.Running && !status.Scanning && status.LastPassAt != nil {
		// s.interval is immutable after construction; reading it here
		// is safe even though s.mu primarily guards s.status.
		next := nextSupervisorPass(*status.LastPassAt, s.interval, time.Now())
		status.NextPassAt = &next
	}
	return status
}

// nextSupervisorPass is when the ticker will next fire. Passes start on ticks,
// which are on the ticker's own phase, so advancing from the last pass by whole
// intervals lands on the real instant even after a long pass has caused ticks to
// be dropped.
//
// This function is only consulted for the /progress page display. The actual
// ticker uses the kernel monotonic clock and is unaffected by wall-clock
// adjustments. When the wall clock has stepped backwards (elapsed < 0),
// lastPassAt is unreliable and computing a meaningful NextPassAt from it is
// impossible; we return now so the UI shows "next pass is due" rather than a
// misleading future timestamp derived from a stale lastPassAt.
func nextSupervisorPass(lastPassAt time.Time, interval time.Duration, now time.Time) time.Time {
	if interval <= 0 {
		return now
	}
	elapsed := now.Sub(lastPassAt)
	if elapsed < 0 {
		return now
	}
	return lastPassAt.Add((elapsed/interval + 1) * interval)
}
