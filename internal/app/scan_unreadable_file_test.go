package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// One unreadable file in the root must not silence the whole root: the scan
// still walks the readable clips, the root stays reachable, and the pipeline
// gets jobs for the assets that scanned fine. The regression this pins is the
// chain walk error -> !Complete -> root unavailable -> GetPrimaryLocation
// returns no rows -> EnqueueAsset fails for every asset, leaving the entire
// root with zero jobs while the scan still reports success.
func TestScanLibraryRootSingleUnreadableFileStillEnqueuesJobs(t *testing.T) {
	ctx := context.Background()
	service, repo, rootDir := newSupervisedLibrary(t, config.LibrarySupervisorConfig{})
	rootID := scanRootID(t, service)

	for _, name := range []string{"a.mp4", "b.mp4", "c.mp4"} {
		scanWriteVideoFile(t, filepath.Join(rootDir, name))
	}
	unreadable := filepath.Join(rootDir, "locked.mp4")
	if err := os.WriteFile(unreadable, []byte("fake video bytes locked.mp4"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })

	// Skip guard: as root (or with CAP_DAC_OVERRIDE) the mode-000 file still
	// opens, so the trigger cannot be created and the test would prove nothing.
	f, err := os.Open(unreadable)
	if err == nil {
		_ = f.Close()
		t.Skip("could not create an unreadable file: running as root or with CAP_DAC_OVERRIDE")
	}

	result, err := service.ScanLibraryRoot(ctx, rootID)
	if err != nil {
		t.Fatal(err)
	}
	// Positive control: the unreadable file must actually have produced a scan
	// error, otherwise the assertions below would pass for the wrong reason.
	if len(result.Errors) == 0 {
		t.Fatal("the unreadable file produced no scan error; the trigger was not exercised")
	}

	root, err := repo.GetLibraryRoot(ctx, rootID)
	if err != nil {
		t.Fatal(err)
	}
	// Non-fatal so the jobs assertion below still runs and reports: the jobs
	// count is the assertion this regression test exists for.
	if root.HealthState == domain.RootHealthUnavailable {
		t.Errorf("root health_state=%q, one unreadable file must not mark the root unavailable", root.HealthState)
	}

	var jobs int
	if err := repo.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs").Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs == 0 {
		t.Fatalf("jobs=%d, want >0: one unreadable file must not zero out the whole root's enqueue", jobs)
	}
	// The counter is asserted by value, not merely as non-zero, because it is
	// the number an operator reads as `queued=` in `root scan` output. A wrong
	// number there is worse than no number: it reproduces the exact silent
	// total-ingest failure the counter was added to expose, only with a
	// plausible-looking figure in front of it. ScanLibraryRoot does not run the
	// pipeline (the CLI calls TryRunPipeline afterwards), so on this fresh
	// library the jobs table holds exactly the probe jobs this scan wrote and
	// nothing downstream: the counter and the rows must agree, exactly.
	if result.Queued != jobs {
		t.Fatalf("result.Queued=%d, jobs=%d: the scan's queued counter must equal the probe jobs it actually wrote; the jobs table holds only this scan's enqueues, so a mismatch means the counter is not tracking the work", result.Queued, jobs)
	}
	// 3 is the readable clips. The unreadable file never became an asset, so it
	// is not among the queued; an off-by-one here means one of the two
	// result.Queued++ sites was lost or one site double-counted.
	if result.Queued != 3 {
		t.Fatalf("result.Queued=%d, want 3: exactly the three readable clips are enqueued; the unreadable file is not an asset", result.Queued)
	}
}
