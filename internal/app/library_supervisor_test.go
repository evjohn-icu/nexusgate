package app

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// newLoopSupervisor builds a supervisor whose clock and whose pass body are both
// the test's. What the loop promises -- never two passes at once, never a pass
// after cancellation -- is a property of the loop and not of the scanning it
// happens to drive, so substituting the body is what makes those promises
// testable without waiting out a real interval or touching a disk.
func newLoopSupervisor(pass func(context.Context)) (*LibrarySupervisor, chan time.Time) {
	supervisor := newLibrarySupervisor(nil, config.LibrarySupervisorConfig{Enabled: true, ScanIntervalMinutes: 60})
	// Capacity one, like time.Ticker's own channel: ticks that arrive during a
	// pass are dropped rather than queued, which is the behaviour under test.
	ticks := make(chan time.Time, 1)
	supervisor.ticks = ticks
	supervisor.pass = pass
	return supervisor, ticks
}

func offerTick(ticks chan time.Time) {
	select {
	case ticks <- time.Now():
	default:
	}
}

// Off is the only safe default: an install that upgrades into this must keep
// behaving exactly as it did, because the loop it would otherwise start spends
// Provider quota with nobody watching.
func TestLibrarySupervisorIsOffByDefaultAndScansNothing(t *testing.T) {
	service, repo, _ := newSupervisedLibrary(t, config.LibrarySupervisorConfig{})

	status := service.LibrarySupervisorStatus()
	if status.Enabled || status.Running {
		t.Fatalf("supervisor must be off by default: %+v", status)
	}

	done := make(chan error, 1)
	go func() { done <- service.RunLibrarySupervisor(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("disabled supervisor returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disabled supervisor kept running; an upgrade would start unattended work")
	}

	assets, err := repo.ListAssets(context.Background(), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 0 {
		t.Fatalf("disabled supervisor scanned the library: %+v", assets)
	}
	jobs, err := repo.ListJobs(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("disabled supervisor queued work: %+v", jobs)
	}
}

// Run returning is the whole shutdown proof: the supervisor starts no goroutine
// of its own and runs the pipeline pass inline, so there is nothing left that
// could outlive it.
func TestLibrarySupervisorStopsWhenTheContextIsCancelled(t *testing.T) {
	passes := make(chan struct{}, 8)
	supervisor, ticks := newLoopSupervisor(func(context.Context) { passes <- struct{}{} })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()

	<-passes // the pass every start does, so a restarted Hub is not idle for an interval
	offerTick(ticks)
	<-passes

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run outlived its cancelled context")
	}
	if status := supervisor.Status(); status.Running {
		t.Fatalf("status still reports a running loop after Run returned: %+v", status)
	}
}

// A second loop would be a second scan and a second pipeline pass on the same
// disk. Refusing is the point; quietly doubling the load is not.
func TestLibrarySupervisorRefusesASecondConcurrentLoop(t *testing.T) {
	passes := make(chan struct{}, 4)
	supervisor, _ := newLoopSupervisor(func(context.Context) { passes <- struct{}{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	<-passes

	if err := supervisor.Run(ctx); err == nil {
		t.Fatal("a second Run must be refused, not started alongside the first")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the first Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the refused second Run stopped the first from shutting down")
	}
}

// A scan slower than the interval must not overlap itself, and the ticks it
// misses must not queue up into a burst of catch-up passes.
func TestLibrarySupervisorPassesDoNotStackUnderRepeatedTicks(t *testing.T) {
	var inFlight, total atomic.Int32
	var overlapped atomic.Bool
	started := make(chan struct{}, 8)
	gate := make(chan struct{})
	supervisor, ticks := newLoopSupervisor(func(context.Context) {
		if inFlight.Add(1) > 1 {
			overlapped.Store(true)
		}
		total.Add(1)
		started <- struct{}{}
		<-gate
		inFlight.Add(-1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()

	<-started // the startup pass is now in flight and will not finish until released
	for i := 0; i < 5; i++ {
		offerTick(ticks)
	}
	gate <- struct{}{} // release the startup pass

	<-started // exactly one pass follows, from the single tick the channel held
	if overlapped.Load() {
		t.Fatal("two supervisor passes ran at once")
	}

	cancel()
	gate <- struct{}{} // release the second pass into a cancelled context
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the pass in flight finished")
	}
	if got := total.Load(); got != 2 {
		t.Fatalf("passes=%d, want 2: five ticks during one pass must collapse to one follow-up", got)
	}
}

// A tick and a cancelled context can be ready together, and select picks at
// random between them. Shutdown has to win, or the Hub starts a scan and a round
// of paid analysis on its way out.
func TestLibrarySupervisorStartsNoWorkDuringShutdown(t *testing.T) {
	var passes atomic.Int32
	supervisor, ticks := newLoopSupervisor(func(context.Context) { passes.Add(1) })
	offerTick(ticks)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := supervisor.Run(ctx); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if got := passes.Load(); got != 0 {
		t.Fatalf("passes=%d, want 0: work was started while shutting down", got)
	}
}

// The off-peak window is the operator's statement about when this machine may
// work the library. NextOffPeakStart is reported so the pause reads as a
// schedule rather than as a loop that died.
func TestLibrarySupervisorHoldsOutsideTheOffPeakWindow(t *testing.T) {
	ctx := context.Background()
	service, repo, _ := newSupervisedLibrary(t, config.LibrarySupervisorConfig{Enabled: true, ScanIntervalMinutes: 5})
	closed := time.Now().Add(2 * time.Hour)
	if err := service.SavePipelineThrottle(ctx, domain.PipelineThrottle{OffPeakEnabled: true, OffPeakStart: closed.Format("15:04"), OffPeakEnd: closed.Add(time.Hour).Format("15:04")}); err != nil {
		t.Fatal(err)
	}

	service.supervisor.scanAndRun(ctx)

	status := service.LibrarySupervisorStatus()
	if status.LastOutcome != SupervisorOutcomeHeldOffPeak {
		t.Fatalf("outcome=%q, want %q", status.LastOutcome, SupervisorOutcomeHeldOffPeak)
	}
	if status.HeldUntil == nil {
		t.Fatal("a held pass must report when it resumes; the page has nothing else to show")
	}
	assets, err := repo.ListAssets(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 0 {
		t.Fatalf("supervisor scanned outside the off-peak window: %+v", assets)
	}
}

// The end-to-end shape of one pass with the real body: footage that appeared
// under a root since the last poll is discovered and queued without anyone
// running `root scan`.
func TestLibrarySupervisorPassDiscoversNewFootage(t *testing.T) {
	ctx := context.Background()
	service, repo, rootDir := newSupervisedLibrary(t, config.LibrarySupervisorConfig{Enabled: true, ScanIntervalMinutes: 5})
	if err := os.WriteFile(filepath.Join(rootDir, "dropped-in.mov"), []byte("footage bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	service.supervisor.scanAndRun(ctx)

	status := service.LibrarySupervisorStatus()
	if status.LastOutcome != SupervisorOutcomeScanned {
		t.Fatalf("outcome=%q err=%q, want %q", status.LastOutcome, status.LastError, SupervisorOutcomeScanned)
	}
	if status.RootsScanned != 1 || status.Discovered != 1 {
		t.Fatalf("roots=%d discovered=%d, want 1/1", status.RootsScanned, status.Discovered)
	}
	if status.LastPassAt == nil {
		t.Fatal("a pass that ran must be visible on the page")
	}
	assets, err := repo.ListAssets(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 {
		t.Fatalf("assets=%+v, want the dropped-in clip", assets)
	}
	jobs, err := repo.ListJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) == 0 {
		t.Fatal("discovered footage was never queued")
	}
}

// One pipeline pass at a time, whoever asked for it. An operator pressing "run"
// and the supervisor waking up must not drive the same disk at once, which would
// defeat the throttle rather than obey it.
func TestPipelinePassesAreMutuallyExclusive(t *testing.T) {
	service, _, _ := newSupervisedLibrary(t, config.LibrarySupervisorConfig{})

	if !service.beginPipelinePass() {
		t.Fatal("the first claim on the pipeline must succeed")
	}
	if ran, err := service.TryRunPipeline(context.Background()); ran || err != nil {
		t.Fatalf("ran=%v err=%v: the supervisor must not start a second concurrent pass", ran, err)
	}
	if service.StartPipeline() {
		t.Fatal("the progress button must not start a second concurrent pass")
	}
	service.endPipelinePass()

	if ran, err := service.TryRunPipeline(context.Background()); !ran || err != nil {
		t.Fatalf("ran=%v err=%v: the guard was not released", ran, err)
	}
	if service.PipelineRunning() {
		t.Fatal("TryRunPipeline left the guard held after returning")
	}
}

// A mistyped interval must not turn into a loop that walks a NAS tree as fast as
// it can.
func TestLibrarySupervisorIntervalIsFloored(t *testing.T) {
	for _, testCase := range []struct {
		minutes int
		want    time.Duration
	}{
		{minutes: 0, want: defaultLibrarySupervisorInterval},
		{minutes: -5, want: defaultLibrarySupervisorInterval},
		{minutes: 1, want: time.Minute},
		{minutes: 45, want: 45 * time.Minute},
	} {
		supervisor := newLibrarySupervisor(nil, config.LibrarySupervisorConfig{Enabled: true, ScanIntervalMinutes: testCase.minutes})
		if supervisor.interval != testCase.want {
			t.Fatalf("minutes=%d interval=%s, want %s", testCase.minutes, supervisor.interval, testCase.want)
		}
		if supervisor.Status().IntervalSeconds != int(testCase.want/time.Second) {
			t.Fatalf("minutes=%d reported %ds", testCase.minutes, supervisor.Status().IntervalSeconds)
		}
	}
}

// "When will it scan next" has to stay true after a pass that overran its own
// interval, or the page shows a time that has already passed.
func TestNextSupervisorPassLandsOnTheTickerPhase(t *testing.T) {
	last := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	interval := 15 * time.Minute
	for _, testCase := range []struct {
		now  time.Time
		want time.Time
	}{
		{now: last, want: last.Add(15 * time.Minute)},
		{now: last.Add(time.Minute), want: last.Add(15 * time.Minute)},
		{now: last.Add(40 * time.Minute), want: last.Add(45 * time.Minute)},
	} {
		if got := nextSupervisorPass(last, interval, testCase.now); !got.Equal(testCase.want) {
			t.Fatalf("now=%s next=%s, want %s", testCase.now, got, testCase.want)
		}
	}
}

// newSupervisedLibrary builds a Hub with one empty library root. The repository
// is the real one: what these tests assert about is whether a scan happened and
// what it queued, which only the real SQL can answer.
func newSupervisedLibrary(t *testing.T, supervisor config.LibrarySupervisorConfig) (*Service, *sqlite.Repository, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dir, "supervisor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Join(dir, "footage")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateLibraryRoot(ctx, rootDir); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, config.Config{DataDir: dir, CacheDir: filepath.Join(dir, "cache"), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}, LibrarySupervisor: supervisor})
	if err != nil {
		t.Fatal(err)
	}
	return service, repo, rootDir
}
