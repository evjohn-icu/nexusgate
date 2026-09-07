package staging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCopyStagesSourceLocallyAndReusesCompleteCache(t *testing.T) {
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, "nas-footage.mov")
	original := []byte("source bytes must stay on the NAS")
	if err := os.WriteFile(sourcePath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	stager, err := New("copy", filepath.Join(t.TempDir(), "nexusgate-cache"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := stager.Stage(context.Background(), sourcePath, "asset-1", "input-v1")
	if err != nil {
		t.Fatal(err)
	}
	if first == sourcePath || !strings.Contains(first, string(filepath.Separator)+"sources"+string(filepath.Separator)+"asset-1"+string(filepath.Separator)) {
		t.Fatalf("staged path=%q, want a local source-cache path", first)
	}
	staged, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(staged) != string(original) {
		t.Fatalf("staged=%q want=%q", staged, original)
	}

	second, err := stager.Stage(context.Background(), sourcePath, "asset-1", "input-v1")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second stage=%q want cache reuse at %q", second, first)
	}
	current, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(original) {
		t.Fatalf("source was changed: got=%q want=%q", current, original)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}
	afterDisconnect, err := stager.Stage(context.Background(), sourcePath, "asset-1", "input-v1")
	if err != nil {
		t.Fatalf("completed local cache should remain usable after source disconnect: %v", err)
	}
	if afterDisconnect != first {
		t.Fatalf("after disconnect path=%q want cached %q", afterDisconnect, first)
	}
}

func TestNoneModeLeavesSourceInPlace(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "local-footage.mp4")
	if err := os.WriteFile(sourcePath, []byte("local source"), 0o600); err != nil {
		t.Fatal(err)
	}
	stager, err := New("none", filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := stager.Stage(context.Background(), sourcePath, "asset-1", "input-v1")
	if err != nil {
		t.Fatal(err)
	}
	if got != sourcePath {
		t.Fatalf("none mode path=%q want original %q", got, sourcePath)
	}
}

func TestNewRejectsUnknownSourceStagingMode(t *testing.T) {
	if _, err := New("mirror_nas", t.TempDir()); err == nil {
		t.Fatal("unknown staging mode should be rejected")
	}
}

func TestCopyRejectsSymlinkDestination(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.mov")
	if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	destinationDir := filepath.Join(cacheDir, "sources", "asset-1")
	if err := os.MkdirAll(destinationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(destinationDir, "input-v1.mov")
	target := filepath.Join(t.TempDir(), "outside.mov")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, destination); err != nil {
		t.Fatal(err)
	}
	stager, err := New("copy", cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stager.Stage(context.Background(), sourcePath, "asset-1", "input-v1"); err == nil {
		t.Fatal("staging should reject a symlink destination")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "outside" {
		t.Fatalf("symlink target changed to %q", got)
	}
}

func TestCappedZeroDoesNotEvict(t *testing.T) {
	cacheDir := t.TempDir()
	stager, err := NewCapped("copy", cacheDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	firstSource := writeSource(t, 8)
	first, err := stager.Stage(context.Background(), firstSource, "asset-old", "input-v1")
	if err != nil {
		t.Fatal(err)
	}
	secondSource := writeSource(t, 8)
	if _, err := stager.Stage(context.Background(), secondSource, "asset-new", "input-v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("unbounded cache evicted its first entry: %v", err)
	}
	if got, err := os.ReadFile(first); err != nil || !bytes.Equal(got, bytes.Repeat([]byte{'x'}, 8)) {
		t.Fatalf("first cached bytes changed: %q, %v", got, err)
	}
}

func TestCappedTriggerEdgeDoesNotEvict(t *testing.T) {
	cacheDir := t.TempDir()
	old := writeCacheFile(t, cacheDir, "asset-old", "input-v1", 94)
	setMTime(t, old, time.Unix(10, 0))
	stager, err := NewCapped("copy", cacheDir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stager.Stage(context.Background(), writeSource(t, 5), "asset-new", "input-v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("entry at the trigger edge was evicted: %v", err)
	}
}

func TestCappedEvictsOldestToTarget(t *testing.T) {
	cacheDir := t.TempDir()
	oldest := writeCacheFile(t, cacheDir, "asset-oldest", "input-v1", 40)
	newest := writeCacheFile(t, cacheDir, "asset-newest", "input-v1", 30)
	setMTime(t, oldest, time.Unix(10, 0))
	setMTime(t, newest, time.Unix(20, 0))
	stager, err := NewCapped("copy", cacheDir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stager.Stage(context.Background(), writeSource(t, 31), "asset-incoming", "input-v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Fatalf("oldest entry survived eviction, stat error=%v", err)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("newest entry was evicted: %v", err)
	}
	if total := cacheBytes(t, cacheDir); total > 90 {
		t.Fatalf("cache total=%d, want at most 90 after eviction", total)
	}
}

func TestCappedKeepsAssetBeingStaged(t *testing.T) {
	cacheDir := t.TempDir()
	kept := writeCacheFile(t, cacheDir, "asset-keep", "input-old", 60)
	other := writeCacheFile(t, cacheDir, "asset-other", "input-v1", 50)
	setMTime(t, kept, time.Unix(10, 0))
	setMTime(t, other, time.Unix(20, 0))
	stager, err := NewCapped("copy", cacheDir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stager.Stage(context.Background(), writeSource(t, 10), "asset-keep", "input-new"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("entry under the staged asset was evicted: %v", err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("other asset was not evicted, stat error=%v", err)
	}
}

func TestCappedSkipsPartialFiles(t *testing.T) {
	cacheDir := t.TempDir()
	regular := writeCacheFile(t, cacheDir, "asset-regular", "input-v1", 50)
	partial := filepath.Join(cacheDir, "sources", "asset-partial", ".input-v1-123.partial")
	if err := os.MkdirAll(filepath.Dir(partial), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, bytes.Repeat([]byte{'p'}, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	setMTime(t, regular, time.Unix(10, 0))
	setMTime(t, partial, time.Unix(1, 0))
	stager, err := NewCapped("copy", cacheDir, 40)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stager.Stage(context.Background(), writeSource(t, 10), "asset-incoming", "input-v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(regular); !os.IsNotExist(err) {
		t.Fatalf("regular file was not evicted, stat error=%v", err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("partial file was not preserved: %v", err)
	}
}

// TestCappedStagesIntoAFreshCacheDirectory pins the first stage into a cache
// that does not exist yet: the walk gets ENOENT and eviction returns before it
// decides anything. It is deliberately NOT the oversized-source test even
// though the source here is larger than the cap — the give-up path is never
// reached, because there is nothing to walk. That case is
// TestCappedStagesASourceLargerThanTheWholeCap, which seeds the cache first.
func TestCappedStagesIntoAFreshCacheDirectory(t *testing.T) {
	cacheDir := t.TempDir()
	stager, err := NewCapped("copy", cacheDir, 5)
	if err != nil {
		t.Fatal(err)
	}
	source := writeSource(t, 10)
	staged, err := stager.Stage(context.Background(), source, "asset-large", "input-v1")
	if err != nil {
		t.Fatalf("incoming source larger than cap failed: %v", err)
	}
	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("staged size=%d, want 10", len(got))
	}
}

func TestCacheHitRefreshesMTime(t *testing.T) {
	cacheDir := t.TempDir()
	stager, err := NewCapped("copy", cacheDir, 100)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := stager.Stage(context.Background(), writeSource(t, 4), "asset-1", "input-v1")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1, 0)
	setMTime(t, staged, old)
	if _, err := stager.Stage(context.Background(), filepath.Join(t.TempDir(), "disconnected.bin"), "asset-1", "input-v1"); err != nil {
		t.Fatalf("cache hit failed: %v", err)
	}
	info, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().After(old) {
		t.Fatalf("cache hit mtime=%v, want after %v", info.ModTime(), old)
	}
}

func TestCappedDeleteFailureContinues(t *testing.T) {
	cacheDir := t.TempDir()
	blocked := writeCacheFile(t, cacheDir, "asset-blocked", "input-v1", 50)
	removed := writeCacheFile(t, cacheDir, "asset-removed", "input-v1", 50)
	setMTime(t, blocked, time.Unix(10, 0))
	setMTime(t, removed, time.Unix(20, 0))
	stager, err := NewCapped("copy", cacheDir, 100)
	if err != nil {
		t.Fatal(err)
	}
	stager.remove = func(path string) error {
		if path == blocked {
			return errors.New("simulated delete failure")
		}
		return os.Remove(path)
	}
	if _, err := stager.Stage(context.Background(), writeSource(t, 10), "asset-incoming", "input-v1"); err != nil {
		t.Fatalf("delete failure made Stage fail: %v", err)
	}
	if _, err := os.Stat(blocked); err != nil {
		t.Fatalf("failed delete should leave blocked file: %v", err)
	}
	if _, err := os.Stat(removed); !os.IsNotExist(err) {
		t.Fatalf("eviction did not continue after failure, stat error=%v", err)
	}
}

func writeSource(t *testing.T, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeCacheFile(t *testing.T, cacheDir, asset, version string, size int) string {
	t.Helper()
	path := filepath.Join(cacheDir, "sources", asset, version+".bin")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{'c'}, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func setMTime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func cacheBytes(t *testing.T, cacheDir string) int64 {
	t.Helper()
	var total int64
	if err := filepath.Walk(cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && !strings.HasPrefix(filepath.Base(path), ".") && !strings.HasSuffix(filepath.Base(path), ".partial") {
			total += info.Size()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return total
}

// The three tests below close gaps found by mutating the implementation: each
// one fails for a mutation that the tests above let through.

// TestCappedEvictsPastTheCapDownToTheTarget is the only test that separates the
// trigger from the target. TestCappedEvictsOldestToTarget cannot: its cache
// holds two files, and removing either one lands under both numbers, so an
// implementation that evicted only to the cap would pass it. Nine small files
// make the difference observable — evicting to the cap stops one file early.
func TestCappedEvictsPastTheCapDownToTheTarget(t *testing.T) {
	cacheDir := t.TempDir()
	for i := 0; i < 9; i++ {
		path := writeCacheFile(t, cacheDir, fmt.Sprintf("asset-%d", i), "input-v1", 10)
		setMTime(t, path, time.Unix(int64(10+i), 0))
	}
	stager, err := NewCapped("copy", cacheDir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stager.Stage(context.Background(), writeSource(t, 15), "asset-incoming", "input-v1"); err != nil {
		t.Fatal(err)
	}
	// 90 cached + 15 incoming trips the cap, and eviction must run until the
	// total is at or under 90 — two files, not the one that would clear 100.
	if total := cacheBytes(t, cacheDir); total > 90 {
		t.Fatalf("cache total=%d after eviction, want at most 90 (the 90%% target).\nEvicting only to the cap would stop at 95 and delete one file on every\nsubsequent stage, which is the churn the target exists to prevent", total)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "sources", "asset-8", "input-v1.bin")); err != nil {
		t.Fatalf("the newest cached entry was evicted: %v", err)
	}
}

// TestCappedStagesASourceLargerThanTheWholeCap exercises the give-up path.
// TestCappedAllowsIncomingLargerThanCap never reaches it: its cache directory
// does not exist yet, so the walk returns ENOENT and eviction returns before
// it can decide anything about the oversized file.
func TestCappedStagesASourceLargerThanTheWholeCap(t *testing.T) {
	cacheDir := t.TempDir()
	existing := writeCacheFile(t, cacheDir, "asset-old", "input-v1", 40)
	setMTime(t, existing, time.Unix(10, 0))
	stager, err := NewCapped("copy", cacheDir, 50)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := stager.Stage(context.Background(), writeSource(t, 100), "asset-huge", "input-v1")
	if err != nil {
		t.Fatalf("a source larger than the whole cap must still stage — a 100 GB clip\ncannot become unprocessable because the cache budget is 50 GB: %v", err)
	}
	if got, err := os.Stat(staged); err != nil || got.Size() != 100 {
		t.Fatalf("staged file: size=%v err=%v, want 100 bytes", got, err)
	}
	if _, err := os.Stat(existing); !os.IsNotExist(err) {
		t.Fatalf("eviction gave up before reclaiming what it could, stat error=%v", err)
	}
}

// TestCappedWalkFailureDoesNotFailStage pins eviction as advisory. Everything
// under sources can be re-copied from the original, so a cache the walk cannot
// read must cost one file over the budget, never a failed job.
func TestCappedWalkFailureDoesNotFailStage(t *testing.T) {
	cacheDir := t.TempDir()
	existing := writeCacheFile(t, cacheDir, "asset-old", "input-v1", 90)
	setMTime(t, existing, time.Unix(10, 0))
	stager, err := NewCapped("copy", cacheDir, 100)
	if err != nil {
		t.Fatal(err)
	}
	stager.walk = func(string, filepath.WalkFunc) error { return errors.New("simulated walk failure") }

	staged, err := stager.Stage(context.Background(), writeSource(t, 30), "asset-new", "input-v1")
	if err != nil {
		t.Fatalf("a failed cache walk failed the stage: %v", err)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(existing); err != nil {
		t.Fatalf("nothing may be evicted when the walk never produced a list: %v", err)
	}
}
