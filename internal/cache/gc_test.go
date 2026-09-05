package cache

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

type fakeAssetLister struct{ ids []string }

func (f fakeAssetLister) ListAllAssetIDs(context.Context) ([]string, error) { return f.ids, nil }

// gcFixture builds a cache dir with the full zoo: rebuildable artifacts,
// scratch dirs, orphan asset dirs, a sources/ staging area and an unlisted
// file. writeFile is J1a's helper (writes size zero bytes). The categories do
// not nest — each orphan dir holds only its own file, so the byte math is
// additive. Returns the cache dir and the exact byte totals the tests assert
// against.
func gcFixture(t *testing.T) (cacheDir string, rebuildableBytes, scratchBytes, orphanBytes int64) {
	t.Helper()
	cacheDir = filepath.Join(t.TempDir(), "cache")

	// Rebuildable artifacts, all under assetA: 3 + 3 + 4 bytes.
	writeFile(t, filepath.Join(cacheDir, "assetA", "thumbnail-sw.jpg"), 3)
	writeFile(t, filepath.Join(cacheDir, "assetA", "proxy-sw.mp4"), 3)
	writeFile(t, filepath.Join(cacheDir, "assetA", "audio.m4a"), 4)
	rebuildableBytes = 3 + 3 + 4

	// Scratch dirs, all under assetA: 2 + 3 bytes.
	writeFile(t, filepath.Join(cacheDir, "assetA", "analysis-frames", "f1.jpg"), 2)
	writeFile(t, filepath.Join(cacheDir, "assetA", "analysis-windows", "w1.jpg"), 3)
	scratchBytes = 2 + 3

	// Orphan asset dirs, one stray (non-rebuildable) file each: assetB (5)
	// and assetC (3). Their content must not match any gc category by name,
	// so the byte math is additive.
	writeFile(t, filepath.Join(cacheDir, "assetB", "odd-file.bin"), 5)
	writeFile(t, filepath.Join(cacheDir, "assetC", "odd-file.bin"), 3)
	orphanBytes = 5 + 3

	// sources/ staging must survive every gc mode.
	writeFile(t, filepath.Join(cacheDir, "sources", "nas-clip.mp4"), 9)

	// An unclassified file is not rebuildable, not scratch, not an orphan dir.
	writeFile(t, filepath.Join(cacheDir, "assetA", "unlisted.txt"), 7)
	old := time.Now().Add(-10 * time.Minute)
	for _, name := range []string{"thumbnail-sw.jpg", "proxy-sw.mp4", "audio.m4a"} {
		if err := os.Chtimes(filepath.Join(cacheDir, "assetA", name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, old, old)
	}); err != nil {
		t.Fatal(err)
	}

	return cacheDir, rebuildableBytes, scratchBytes, orphanBytes
}

// The safety default: a dry run deletes nothing but reports exactly what a
// real run would free, byte for byte.
func TestGCDryRunReportsWithoutDeleting(t *testing.T) {
	cacheDir, rebuildableBytes, scratchBytes, orphanBytes := gcFixture(t)
	orphans, err := OrphanDirectories(context.Background(), fakeAssetLister{ids: []string{"assetA"}}, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 2 { // assetB, assetC — sources/ and scratch are never orphans
		t.Fatalf("orphans = %d, want 2: %+v", len(orphans), orphans)
	}

	result, err := GC(cacheDir, GCOptions{DryRun: true, RemoveScratch: true, RemoveRebuildable: true, RemoveOrphans: orphans})
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := 3 + 2 + 2 // rebuildable files + scratch dirs + orphan dirs
	if result.RemovedFiles != wantFiles {
		t.Errorf("dry-run RemovedFiles = %d, want %d", result.RemovedFiles, wantFiles)
	}
	if result.FreedBytes != rebuildableBytes+scratchBytes+orphanBytes {
		t.Errorf("dry-run FreedBytes = %d, want %d", result.FreedBytes, rebuildableBytes+scratchBytes+orphanBytes)
	}
	for _, mustSurvive := range []string{
		"assetA/thumbnail-sw.jpg",
		"assetA/analysis-frames/f1.jpg",
		"assetB/odd-file.bin",
		"assetC/odd-file.bin",
		"sources/nas-clip.mp4",
		"assetA/unlisted.txt",
	} {
		if _, err := os.Stat(filepath.Join(cacheDir, mustSurvive)); err != nil {
			t.Errorf("dry run deleted %s: %v", mustSurvive, err)
		}
	}
	if len(result.Errors) != 0 {
		t.Errorf("dry run reported errors: %v", result.Errors)
	}
}

// The real run: scratch + rebuildable go, sources/ and unclassified files
// stay, and the freed bytes match the dry-run accounting.
func TestGCDeletesScratchAndRebuildableKeepsSourcesAndUnlisted(t *testing.T) {
	cacheDir, rebuildableBytes, scratchBytes, _ := gcFixture(t)

	result, err := GC(cacheDir, GCOptions{RemoveScratch: true, RemoveRebuildable: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedFiles != 3+2 {
		t.Errorf("RemovedFiles = %d, want %d", result.RemovedFiles, 5)
	}
	if result.FreedBytes != rebuildableBytes+scratchBytes {
		t.Errorf("FreedBytes = %d, want %d", result.FreedBytes, rebuildableBytes+scratchBytes)
	}
	for _, gone := range []string{
		"assetA/thumbnail-sw.jpg",
		"assetA/proxy-sw.mp4",
		"assetA/audio.m4a",
		"assetA/analysis-frames",
		"assetA/analysis-windows",
	} {
		if _, err := os.Stat(filepath.Join(cacheDir, gone)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted, stat err = %v", gone, err)
		}
	}
	// Orphan dirs were not selected, so they survive along with staging and
	// unclassified content.
	for _, kept := range []string{
		"sources/nas-clip.mp4",
		"assetA/unlisted.txt",
		"assetB/odd-file.bin",
		"assetC/odd-file.bin",
	} {
		if _, err := os.Stat(filepath.Join(cacheDir, kept)); err != nil {
			t.Errorf("expected %s to survive: %v", kept, err)
		}
	}
	if len(result.Errors) != 0 {
		t.Errorf("reported errors: %v", result.Errors)
	}
}

// Orphan deletion removes exactly the given directories — an unlisted asset
// dir is not collateral damage.
func TestGCOrphanDeletionRemovesOnlyGivenDirs(t *testing.T) {
	cacheDir, _, _, _ := gcFixture(t)
	// Only assetC is an orphan now (assetA and assetB are known assets).
	target := filepath.Join(cacheDir, "assetC")
	result, err := GC(cacheDir, GCOptions{RemoveOrphans: []OrphanDir{{Path: target}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("expected orphan dir %s to be deleted, stat err = %v", target, err)
	}
	for _, kept := range []string{"assetA/thumbnail-sw.jpg", "assetB/odd-file.bin"} {
		if _, err := os.Stat(filepath.Join(cacheDir, kept)); err != nil {
			t.Errorf("expected %s to survive: %v", kept, err)
		}
	}
	if len(result.Removed) != 1 || result.Removed[0] != "assetC" {
		t.Errorf("Removed = %v, want [assetC]", result.Removed)
	}
}

// sources/ is never deletable, even when it matches a category pattern: a
// file named like a thumbnail inside sources/ is staging, not an artifact.
func TestGCSourcesNeverDeleted(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	writeFile(t, filepath.Join(cacheDir, "sources", "thumbnail-sw.jpg"), 9)
	writeFile(t, filepath.Join(cacheDir, "sources", "analysis-frames", "f.jpg"), 9)

	result, err := GC(cacheDir, GCOptions{RemoveScratch: true, RemoveRebuildable: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedFiles != 0 || result.FreedBytes != 0 {
		t.Errorf("gc touched sources/ (removed %d files, %d bytes)", result.RemovedFiles, result.FreedBytes)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "sources", "thumbnail-sw.jpg")); err != nil {
		t.Errorf("sources/thumbnail deleted: %v", err)
	}
}

// An orphan path outside the cache dir is refused outright: the cache command
// must never be able to delete through a path mistake.
func TestGCRefusesOrphansOutsideCacheDir(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-dir")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := GC(cacheDir, GCOptions{RemoveOrphans: []OrphanDir{{Path: outside}}}); err == nil {
		t.Error("expected GC to refuse an orphan path outside the cache dir")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("outside dir was touched: %v", err)
	}
}

// A missing cache dir (fresh install) is an empty success, matching
// Inspect's contract: `cache gc` must run before the first scan.
func TestGCMissingCacheDirIsEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	result, err := GC(missing, GCOptions{DryRun: true, RemoveScratch: true, RemoveRebuildable: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedFiles != 0 || result.FreedBytes != 0 {
		t.Errorf("missing cache dir = %d files, %d bytes; want empty", result.RemovedFiles, result.FreedBytes)
	}
}

func TestGCCompleteRebuildableInventoryIsNotReportCapped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	now := time.Now().UTC()
	for i := 0; i < 27; i++ {
		id := fmt.Sprintf("asset-%02d", i)
		writeFile(t, filepath.Join(dir, id, "proxy-sw.mp4"), 1)
		old := now.Add(-10 * time.Minute)
		if err := os.Chtimes(filepath.Join(dir, id, "proxy-sw.mp4"), old, old); err != nil {
			t.Fatal(err)
		}
	}
	want := make([]string, 27)
	for i := range want {
		want[i] = fmt.Sprintf("asset-%02d", i)
	}
	for _, dry := range []bool{true, false} {
		result, err := GC(dir, GCOptions{DryRun: dry, RemoveRebuildable: true, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result.RemovedRebuildableAssetIDs, want) {
			t.Fatalf("dry=%v ids = %v, want %v", dry, result.RemovedRebuildableAssetIDs, want)
		}
		if len(result.Removed) != reportLimit || !result.RemovedTruncated {
			t.Fatalf("dry=%v report = %d truncated=%v", dry, len(result.Removed), result.RemovedTruncated)
		}
		if result.RemovedFiles != 27 {
			t.Fatalf("dry=%v files = %d", dry, result.RemovedFiles)
		}
		if dry {
			for _, id := range want {
				if _, err := os.Stat(filepath.Join(dir, id, "proxy-sw.mp4")); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestGCProtectedYoungAndNonRebuildableDoNotEnterInventory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	now := time.Now().UTC()
	old := now.Add(-10 * time.Minute)
	writeFile(t, filepath.Join(dir, "active", "proxy-sw.mp4"), 2)
	writeFile(t, filepath.Join(dir, "young", "proxy-sw.mp4"), 3)
	writeFile(t, filepath.Join(dir, "old", "proxy-sw.mp4"), 4)
	writeFile(t, filepath.Join(dir, "old", "scratch", "x"), 5)
	writeFile(t, filepath.Join(dir, "orphan", "odd.bin"), 6)
	for _, id := range []string{"active", "old"} {
		p := filepath.Join(dir, id, "proxy-sw.mp4")
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	result, err := GC(dir, GCOptions{DryRun: true, RemoveRebuildable: true, ProtectedAssetIDs: map[string]struct{}{"active": {}}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.RemovedRebuildableAssetIDs, []string{"old"}) {
		t.Fatalf("ids = %v", result.RemovedRebuildableAssetIDs)
	}
	if result.SkippedActiveFiles != 1 || result.SkippedYoungFiles != 1 {
		t.Fatalf("skips = %+v", result)
	}
}

// Inspection and orphan classification agree on the fixture layout.
func TestInspectClassifiesFixture(t *testing.T) {
	cacheDir, rebuildableBytes, scratchBytes, _ := gcFixture(t)
	stats, err := Inspect(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ThumbnailCount != 1 || stats.ProxyCount != 1 || stats.AudioCount != 1 {
		t.Errorf("artifact counts = t%d p%d a%d, want 1/1/1", stats.ThumbnailCount, stats.ProxyCount, stats.AudioCount)
	}
	if stats.ThumbnailBytes+stats.ProxyBytes+stats.AudioBytes != rebuildableBytes {
		t.Errorf("derived bytes = %d, want %d", stats.ThumbnailBytes+stats.ProxyBytes+stats.AudioBytes, rebuildableBytes)
	}
	if stats.ScratchBytes != scratchBytes {
		t.Errorf("scratch bytes = %d, want %d", stats.ScratchBytes, scratchBytes)
	}
	if stats.RebuildableBytes != rebuildableBytes+scratchBytes {
		t.Errorf("rebuildable bytes = %d, want %d", stats.RebuildableBytes, rebuildableBytes+scratchBytes)
	}
	if stats.SourceStagingCount != 1 {
		t.Errorf("source staging files = %d, want 1", stats.SourceStagingCount)
	}
	if stats.OrphanCount != 3 { // unlisted.txt + the two orphan-dir strays
		t.Errorf("other files = %d, want 3", stats.OrphanCount)
	}
	if stats.TotalFiles != 9 {
		t.Errorf("total files = %d, want 9", stats.TotalFiles)
	}
}

func TestOrphanDirectoriesSkipsSourcesAndKnownAssets(t *testing.T) {
	cacheDir, _, _, _ := gcFixture(t)
	orphans, err := OrphanDirectories(context.Background(), fakeAssetLister{ids: []string{"assetA", "sources"}}, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 2 {
		t.Fatalf("orphans = %+v, want [assetB assetC]", orphans)
	}
	if filepath.Base(orphans[0].Path) != "assetB" || filepath.Base(orphans[1].Path) != "assetC" {
		t.Errorf("orphans = %+v, want assetB then assetC", orphans)
	}
	if orphans[0].Bytes != 5 || orphans[1].Bytes != 3 {
		t.Errorf("orphan bytes = %d/%d, want 5/3", orphans[0].Bytes, orphans[1].Bytes)
	}
}

// Verify must report the DB-row/file gap both ways: rows without files
// (rebuildable gaps) and files without rows (orphans, sources/ excluded).
func TestVerifyReportsRowsFilesAndOrphans(t *testing.T) {
	cacheDir, _, _, _ := gcFixture(t)
	rows := []string{
		filepath.Join(cacheDir, "assetA", "thumbnail-sw.jpg"), // exists
		filepath.Join(cacheDir, "assetA", "proxy-sw.mp4"),     // exists
		filepath.Join(cacheDir, "assetA", "audio.m4a"),        // exists
		filepath.Join(cacheDir, "assetC", "odd-file.bin"),     // exists
		filepath.Join(cacheDir, "assetX", "thumbnail-sw.jpg"), // row without file
	}
	repo := fakeArtifactLister{paths: rows}
	result, err := Verify(context.Background(), repo, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.ArtifactRows != 5 {
		t.Errorf("ArtifactRows = %d, want 5", result.ArtifactRows)
	}
	if result.MissingFiles != 1 {
		t.Errorf("MissingFiles = %d, want 1", result.MissingFiles)
	}
	// 9 fixture files, minus sources/nas-clip.mp4 (excluded from orphan
	// accounting) and the 4 files that have rows = 4 files with no row.
	if result.OrphanFiles != 4 {
		t.Errorf("OrphanFiles = %d, want 4", result.OrphanFiles)
	}
	if result.TotalCacheFiles != 9 {
		t.Errorf("TotalCacheFiles = %d, want 9", result.TotalCacheFiles)
	}
	if len(result.MissingPaths) != 1 || result.MissingPaths[0] != filepath.Join(cacheDir, "assetX", "thumbnail-sw.jpg") {
		t.Errorf("MissingPaths = %v", result.MissingPaths)
	}
}

// Verify on a missing cache dir is an empty result, matching Inspect's
// fresh-install contract.
func TestVerifyMissingCacheDirIsEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	result, err := Verify(context.Background(), fakeArtifactLister{}, missing)
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCacheFiles != 0 || result.OrphanFiles != 0 {
		t.Errorf("missing cache dir = %+v, want empty", result)
	}
}

type fakeArtifactLister struct{ paths []string }

func (f fakeArtifactLister) ListDerivedArtifacts(context.Context) ([]domain.DerivedArtifact, error) {
	var artifacts []domain.DerivedArtifact
	for i, path := range f.paths {
		artifacts = append(artifacts, domain.DerivedArtifact{ID: "art", AssetID: "asset", Type: "thumbnail", LocalPath: path, SizeBytes: int64(i)})
	}
	return artifacts, nil
}
