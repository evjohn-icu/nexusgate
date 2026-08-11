package app

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	sqliterepo "github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

func scanWriteVideoFile(t *testing.T, path string) {
	t.Helper()
	// Distinct-by-default content: the scanner keys assets on a content
	// fingerprint plus size, so two fixtures with identical bytes would dedupe
	// onto the same asset and stop counting as separate files.
	if err := os.WriteFile(path, []byte("fake video bytes "+filepath.Base(path)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// scanRootID returns the id of the single root a supervised library was built
// with.
func scanRootID(t *testing.T, service *Service) string {
	t.Helper()
	roots, err := service.repo.ListLibraryRoots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 {
		t.Fatalf("want exactly one root, got %d", len(roots))
	}
	return roots[0].ID
}

// The reason this whole change exists: a rescan of an untouched directory must
// not re-enqueue the library. The first scan reports the new file; the second
// must report nothing.
func TestScanLibraryRootSecondScanOfUnchangedLibraryIsEmpty(t *testing.T) {
	ctx := context.Background()
	service, _, rootDir := newSupervisedLibrary(t, config.LibrarySupervisorConfig{})
	scanWriteVideoFile(t, filepath.Join(rootDir, "clip.mp4"))
	rootID := scanRootID(t, service)

	first, err := service.ScanLibraryRoot(ctx, rootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.ChangedAssetIDs) != 1 {
		t.Fatalf("first scan changed=%v, want exactly the new clip", first.ChangedAssetIDs)
	}
	if first.Discovered != 1 {
		t.Fatalf("first scan discovered=%d, want 1", first.Discovered)
	}

	second, err := service.ScanLibraryRoot(ctx, rootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.ChangedAssetIDs) != 0 {
		t.Fatalf("rescan of an unchanged library reported changed assets: %v", second.ChangedAssetIDs)
	}
}

// Touching a file's mtime between scans is the exact signal the probe input
// hash keys on, so the rescan must report exactly that asset and no other.
func TestScanLibraryRootMtimeTouchReportsExactlyThatAsset(t *testing.T) {
	ctx := context.Background()
	service, _, rootDir := newSupervisedLibrary(t, config.LibrarySupervisorConfig{})
	clip := filepath.Join(rootDir, "clip.mp4")
	scanWriteVideoFile(t, clip)
	rootID := scanRootID(t, service)

	if _, err := service.ScanLibraryRoot(ctx, rootID); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(clip)
	if err != nil {
		t.Fatal(err)
	}
	moved := info.ModTime().Add(2 * time.Hour)
	if err := os.Chtimes(clip, moved, moved); err != nil {
		t.Fatal(err)
	}

	result, err := service.ScanLibraryRoot(ctx, rootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ChangedAssetIDs) != 1 {
		t.Fatalf("changed=%v, want exactly the touched asset", result.ChangedAssetIDs)
	}
	if result.Discovered != 0 {
		t.Fatalf("a mtime touch must not mint a new asset, discovered=%d", result.Discovered)
	}
	// The reported id must be the same asset the first scan created, not some
	// unrelated row.
	assets, err := service.repo.ListAssets(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].ID != result.ChangedAssetIDs[0] {
		t.Fatalf("changed asset %q does not match the scanned asset %q", result.ChangedAssetIDs[0], assets[0].ID)
	}
}

// A brand-new file dropped into an existing root is discovered and reported as
// changed, so a probe job gets enqueued for it.
func TestScanLibraryRootNewFileInExistingRootIsDiscoveredAndChanged(t *testing.T) {
	ctx := context.Background()
	service, _, rootDir := newSupervisedLibrary(t, config.LibrarySupervisorConfig{})
	scanWriteVideoFile(t, filepath.Join(rootDir, "clip.mp4"))
	rootID := scanRootID(t, service)
	if _, err := service.ScanLibraryRoot(ctx, rootID); err != nil {
		t.Fatal(err)
	}

	scanWriteVideoFile(t, filepath.Join(rootDir, "later.mp4"))
	result, err := service.ScanLibraryRoot(ctx, rootID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Discovered != 1 {
		t.Fatalf("discovered=%d, want 1 for the new file", result.Discovered)
	}
	if len(result.ChangedAssetIDs) != 1 {
		t.Fatalf("changed=%v, want exactly the new file", result.ChangedAssetIDs)
	}
}

// The tests above assert on what a scan reports; this one asserts on what it
// does to the queue -- that a changed file gets a genuinely new job and an
// unchanged rescan adds nothing.
//
// It deliberately does not claim to catch a regression back to enqueuing the
// whole library: EnqueueJob is INSERT OR IGNORE keyed on the input hash, so
// re-enqueuing an unchanged asset produces no row either way. The cost of that
// regression is query volume, which is not observable from here. What is
// observable, and what this pins, is that the narrowed set still enqueues
// everything it must.
func TestScanLibraryRootEnqueuesOnlyForChangedAssets(t *testing.T) {
	ctx := context.Background()
	service, repo, rootDir := newSupervisedLibrary(t, config.LibrarySupervisorConfig{})
	clip := filepath.Join(rootDir, "clip.mp4")
	scanWriteVideoFile(t, clip)
	rootID := scanRootID(t, service)

	if _, err := service.ScanLibraryRoot(ctx, rootID); err != nil {
		t.Fatal(err)
	}
	afterFirst, err := repo.ListJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterFirst) != 1 || afterFirst[0].Type != domain.JobProbe {
		t.Fatalf("first scan produced jobs=%+v, want one probe job", afterFirst)
	}

	if _, err := service.ScanLibraryRoot(ctx, rootID); err != nil {
		t.Fatal(err)
	}
	afterSecond, err := repo.ListJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterSecond) != len(afterFirst) {
		t.Fatalf("rescan of an unchanged library added jobs: %d -> %d", len(afterFirst), len(afterSecond))
	}

	// Moving the mtime changes the probe job's input hash, so a genuinely new
	// job must appear -- the deduplication is on the hash, not on the asset.
	info, err := os.Stat(clip)
	if err != nil {
		t.Fatal(err)
	}
	moved := info.ModTime().Add(2 * time.Hour)
	if err := os.Chtimes(clip, moved, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ScanLibraryRoot(ctx, rootID); err != nil {
		t.Fatal(err)
	}
	afterTouch, err := repo.ListJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterTouch) != len(afterFirst)+1 {
		t.Fatalf("touching the file produced %d jobs, want %d", len(afterTouch), len(afterFirst)+1)
	}
}

// An explicit scan is also a pipeline entry point for callers that need the
// pass to finish before they return, such as the CLI. The real SQLite queue is
// part of this assertion: checking only the scan result would miss a broken
// enqueue-to-pipeline handoff.
func TestScanLibraryRootThenTryRunPipelineDrainsTheQueuedProbe(t *testing.T) {
	ctx := context.Background()
	service, repo, rootDir := newSupervisedLibrary(t, config.LibrarySupervisorConfig{})
	scanWriteVideoFile(t, filepath.Join(rootDir, "clip.mp4"))
	rootID := scanRootID(t, service)

	if _, err := service.ScanLibraryRoot(ctx, rootID); err != nil {
		t.Fatal(err)
	}
	queued, err := repo.ListJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Type != domain.JobProbe {
		t.Fatalf("queued jobs=%+v, want one probe job", queued)
	}

	ran, err := service.TryRunPipeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("TryRunPipeline reported that the explicit scan could not run")
	}
	if service.PipelineRunning() {
		t.Fatal("synchronous TryRunPipeline left the pass marked as running")
	}
	jobs, err := repo.ListJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) == 0 {
		t.Fatal("pipeline pass removed the queued job record")
	}
	for _, job := range jobs {
		if job.State == domain.JobRunning {
			t.Fatalf("pipeline pass left job running: %+v", job)
		}
	}
	if jobs[0].AttemptCount == 0 {
		t.Fatalf("pipeline pass never attempted the queued probe: %+v", jobs[0])
	}
}

// A scan of one root must never pull another root's assets into its changed
// set, even when the other root was scanned around the same time.
func TestScanLibraryRootChangeInOneRootDoesNotAffectAnother(t *testing.T) {
	ctx := context.Background()
	service, repo, rootADir := newSupervisedLibrary(t, config.LibrarySupervisorConfig{})
	rootAID := scanRootID(t, service)
	rootBDir := filepath.Join(t.TempDir(), "footage-b")
	if err := os.MkdirAll(rootBDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootB, err := repo.CreateLibraryRoot(ctx, rootBDir)
	if err != nil {
		t.Fatal(err)
	}

	clipA := filepath.Join(rootADir, "a.mp4")
	scanWriteVideoFile(t, clipA)
	scanWriteVideoFile(t, filepath.Join(rootBDir, "b.mp4"))
	if _, err := service.ScanLibraryRoot(ctx, rootAID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ScanLibraryRoot(ctx, rootB.ID); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(clipA)
	if err != nil {
		t.Fatal(err)
	}
	moved := info.ModTime().Add(2 * time.Hour)
	if err := os.Chtimes(clipA, moved, moved); err != nil {
		t.Fatal(err)
	}

	result, err := service.ScanLibraryRoot(ctx, rootAID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ChangedAssetIDs) != 1 {
		t.Fatalf("root A scan changed=%v, want exactly root A's asset", result.ChangedAssetIDs)
	}
	// Root B's asset must be absent: the earlier B scan enqueued its probe job,
	// and nothing in root B moved.
	assetsB, err := repo.AssetsWithoutProbeJob(ctx, rootB.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(assetsB) != 0 {
		t.Fatalf("root B assets were disturbed by a root A scan: %v", assetsB)
	}
}

// TestScanLibraryRootConcurrentSameRoot exercises the scan-local failure tracking
// fix: two concurrent scans of the same root must not corrupt each other's failure
// counts.  The race detector (-race) verifies there is no data race on the shared
// scanFailures map.
func TestScanLibraryRootConcurrentSameRoot(t *testing.T) {
	ctx := context.Background()
	service, _, rootDir := newSupervisedLibrary(t, config.LibrarySupervisorConfig{})
	rootID := scanRootID(t, service)

	// Create several files and do an initial scan to populate the library.
	for _, name := range []string{"a.mp4", "b.mp4", "c.mp4"} {
		scanWriteVideoFile(t, filepath.Join(rootDir, name))
	}
	if _, err := service.ScanLibraryRoot(ctx, rootID); err != nil {
		t.Fatal(err)
	}

	// Touch all files so the next scans see changes.
	for _, name := range []string{"a.mp4", "b.mp4", "c.mp4"} {
		path := filepath.Join(rootDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		moved := info.ModTime().Add(2 * time.Hour)
		if err := os.Chtimes(path, moved, moved); err != nil {
			t.Fatal(err)
		}
	}

	// Run two concurrent scans of the same root.  With the old code (shared
	// map reference) this could cause one scan to delete the other's failure
	// counter entries.  With the fix each scan has its own local copy and
	// merges back at the end.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := service.ScanLibraryRoot(ctx, rootID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent scan failed: %v", err)
		}
	}

	// Verify the scanFailures map is in a sane state (no lingering entries
	// from a partially-deleted concurrent prune).
	service.scanFailuresMu.Lock()
	failures := service.scanFailures[rootID]
	service.scanFailuresMu.Unlock()
	for id, count := range failures {
		if count <= 0 {
			t.Errorf("scanFailure for %q has count %d, want >0 or absent", id, count)
		}
	}
	var live int
	if err := service.repo.(*sqliterepo.Repository).DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_locations WHERE root_id=? AND exists_now=1`, rootID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live == 0 {
		t.Fatal("overlapping same-root scans left all locations missing")
	}
}
