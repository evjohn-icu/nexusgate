package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// TestGCOrphanFixturePreservesManagedSubtreesAndQueuedAssets is the
// `--orphans --yes` acceptance fixture: one real orphan asset directory is
// removed, while the managed search-references tree, the active inference
// scratch, and the artifacts of an asset with queued pipeline work all
// survive. The subtrees are the regression: their names match no asset row,
// so an orphan listing that did not know them would delete a 24-hour query
// image or the input a live inference run is about to read.
func TestGCOrphanFixturePreservesManagedSubtreesAndQueuedAssets(t *testing.T) {
	ctx := context.Background()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	writeFile(t, filepath.Join(cacheDir, "gone-asset", "odd-file.bin"), 5)
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(filepath.Join(cacheDir, "gone-asset"), old, old); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cacheDir, "search-references", "abc123.jpg"), 11)
	writeFile(t, filepath.Join(cacheDir, "inference", "inputs", "job1", "frame.jpg"), 13)
	// A protected asset's directory is a known asset, so it is not an orphan
	// candidate at all; its artifacts are covered by the protection set in the
	// rebuildable test below.
	writeFile(t, filepath.Join(cacheDir, "kept-asset", "thumbnail-sw.jpg"), 7)

	lister := fakeAssetLister{ids: []string{"kept-asset"}}
	orphanDirs, err := OrphanDirectories(ctx, lister, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphanDirs) != 1 || filepath.Base(orphanDirs[0].Path) != "gone-asset" {
		t.Fatalf("orphan candidates = %+v, want only gone-asset", orphanDirs)
	}
	result, err := GC(cacheDir, GCOptions{
		RemoveOrphans:     orphanDirs,
		ProtectedAssetIDs: map[string]struct{}{"kept-asset": {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedFiles != 1 || result.FreedBytes != 5 {
		t.Fatalf("removed %d files/%d bytes, want 1/5", result.RemovedFiles, result.FreedBytes)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "gone-asset")); !os.IsNotExist(err) {
		t.Fatalf("the real orphan must be gone, stat err = %v", err)
	}
	for _, preserved := range []string{
		filepath.Join(cacheDir, "search-references", "abc123.jpg"),
		filepath.Join(cacheDir, "inference", "inputs", "job1", "frame.jpg"),
		filepath.Join(cacheDir, "kept-asset", "thumbnail-sw.jpg"),
	} {
		if _, err := os.Stat(preserved); err != nil {
			t.Fatalf("%s must survive an orphan run: %v", preserved, err)
		}
	}
}

// TestGCRebuildableGatedOnDurableRepair pins the eligibility gate: artifacts
// whose asset is absent from the run's durable-repair set are preserved and
// reported as skipped-unrepaired, never deleted behind a merely promised
// rebuild. Only a nil set disables gating.
func TestGCRebuildableGatedOnDurableRepair(t *testing.T) {
	cacheDir, _, _, _ := gcFixture(t)
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(filepath.Join(cacheDir, "assetA", "unlisted.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	// Ungated: everything rebuildable goes.
	result, err := GC(cacheDir, GCOptions{RemoveRebuildable: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.SkippedUnrepairedFiles != 0 {
		t.Fatalf("a nil eligibility set must gate nothing, skipped %d", result.SkippedUnrepairedFiles)
	}
	if result.RemovedRebuildableAssetIDs == nil || len(result.RemovedRebuildableAssetIDs) != 1 {
		t.Fatalf("ungated run must report assetA, got %v", result.RemovedRebuildableAssetIDs)
	}

	// Gated away: the same artifact survives and the skip is accounted.
	cacheDir, _, _, _ = gcFixture(t)
	result, err = GC(cacheDir, GCOptions{
		RemoveRebuildable: true,
		EligibleAssetIDs:  map[string]struct{}{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "assetA", "thumbnail-sw.jpg")); err != nil {
		t.Fatalf("an asset without a durable repair keeps its files: %v", err)
	}
	if result.SkippedUnrepairedFiles != 3 || result.SkippedUnrepairedBytes != 10 {
		t.Fatalf("skipped unrepaired = %d files/%d bytes, want 3/10", result.SkippedUnrepairedFiles, result.SkippedUnrepairedBytes)
	}
	if len(result.RemovedRebuildableAssetIDs) != 0 {
		t.Fatalf("a gated-out asset must not be reported as removed, got %v", result.RemovedRebuildableAssetIDs)
	}
}

// waveformPairFixture writes a manifest and the payload it names under one
// asset directory and returns the manifest path, payload path and payload
// bytes.
func waveformPairFixture(t *testing.T, cacheDir, assetID string) (manifestPath, payloadPath string, payload []byte) {
	t.Helper()
	payload = []byte{0, 0, 0x80, 0xbf, 0, 0, 0x80, 0x3f, 0, 0, 0, 0, 0, 0, 0, 0}
	sum := sha256.Sum256(payload)
	manifest := map[string]any{
		"version":        "waveform.v1",
		"sample_rate":    16000,
		"bucket_ms":      10,
		"duration_ms":    20,
		"levels":         []map[string]any{{"level": 0, "offset": 0, "count": 2}},
		"payload_size":   len(payload),
		"payload_sha256": hex.EncodeToString(sum[:]),
		"payload_file":   "waveform." + hex.EncodeToString(sum[:]) + ".bin",
	}
	manifestPath = filepath.Join(cacheDir, assetID, "waveform.json")
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	payloadPath = filepath.Join(filepath.Dir(manifestPath), "waveform."+hex.EncodeToString(sum[:])+".bin")
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return manifestPath, payloadPath, payload
}

// TestGCWaveformPairIsOneLogicalArtifact pins the pair contract in GC: a
// rebuildable run deletes the manifest and the payload it names together, an
// unregistered payload (no manifest names it) goes on its own merits, and a
// dry run reports both halves without deleting either.
func TestGCWaveformPairIsOneLogicalArtifact(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	manifestPath, payloadPath, pairPayload := waveformPairFixture(t, cacheDir, "assetA")
	// A payload no manifest names: a crashed publish's litter.
	lonePath := filepath.Join(cacheDir, "assetA", "waveform."+strings.Repeat("ab", 32)+".bin")
	writeFile(t, lonePath, 8)
	old := time.Now().Add(-10 * time.Minute)
	if err := filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, old, old)
	}); err != nil {
		t.Fatal(err)
	}

	// Dry run: both halves reported, nothing deleted.
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := manifestInfo.Size() + int64(len(pairPayload)) + 8
	dry, err := GC(cacheDir, GCOptions{RemoveRebuildable: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.RemovedFiles != 3 || dry.FreedBytes != wantBytes {
		t.Fatalf("dry run removed %d files/%d bytes, want 3/%d", dry.RemovedFiles, dry.FreedBytes, wantBytes)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatal("a dry run must not delete the manifest")
	}
	if _, err := os.Stat(payloadPath); err != nil {
		t.Fatal("a dry run must not delete the payload")
	}

	// Real run: manifest and referenced payload go together; the lone payload
	// goes too.
	result, err := GC(cacheDir, GCOptions{RemoveRebuildable: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedFiles != 3 || result.FreedBytes != wantBytes {
		t.Fatalf("removed %d files/%d bytes, want 3/%d", result.RemovedFiles, result.FreedBytes, wantBytes)
	}
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("manifest must be removed, stat err = %v", err)
	}
	if _, err := os.Stat(payloadPath); !os.IsNotExist(err) {
		t.Fatalf("referenced payload must be removed with its manifest, stat err = %v", err)
	}
	if _, err := os.Stat(lonePath); !os.IsNotExist(err) {
		t.Fatalf("an unregistered payload must be removed, stat err = %v", err)
	}
}

// TestGCMissingWaveformPayloadStillRemovesManifest pins the missing-pair
// shape: a manifest whose payload is already gone is still deletable
// rebuildable content — the row survives and the derive re-creates both
// halves — and the missing payload is not an error.
func TestGCMissingWaveformPayloadStillRemovesManifest(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	manifestPath, payloadPath, _ := waveformPairFixture(t, cacheDir, "assetA")
	if err := os.Remove(payloadPath); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(manifestPath, old, old); err != nil {
		t.Fatal(err)
	}
	result, err := GC(cacheDir, GCOptions{RemoveRebuildable: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedFiles != 1 {
		t.Fatalf("removed %d files, want 1 (the manifest)", result.RemovedFiles)
	}
	for _, err := range result.Errors {
		t.Fatalf("unexpected error: %s", err)
	}
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("manifest must be removed, stat err = %v", err)
	}
}

// typedArtifactLister returns rows with explicit artifact types, so Verify's
// waveform pair logic can be exercised against real "waveform" rows.
type typedArtifactLister struct{ rows []domain.DerivedArtifact }

func (f typedArtifactLister) ListDerivedArtifacts(context.Context) ([]domain.DerivedArtifact, error) {
	return f.rows, nil
}

// TestVerifyWaveformRowReportsMissingPair pins the pair contract in Verify: a
// waveform row whose manifest exists but whose named payload is missing is
// ONE missing artifact, and the payload a registered manifest names is never
// an orphan. A payload no manifest or row accounts for is still an orphan.
func TestVerifyWaveformRowReportsMissingPair(t *testing.T) {
	ctx := context.Background()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	manifestPath, payloadPath, _ := waveformPairFixture(t, cacheDir, "assetA")
	rows := []domain.DerivedArtifact{{ID: "w1", AssetID: "assetA", Type: "waveform", LocalPath: manifestPath}}
	result, err := Verify(ctx, typedArtifactLister{rows: rows}, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.MissingFiles != 0 {
		t.Fatalf("an intact pair is not missing, got %d", result.MissingFiles)
	}
	if result.OrphanFiles != 0 {
		t.Fatalf("the registered payload must not be an orphan, got %d orphans", result.OrphanFiles)
	}

	// The payload vanishes: one missing artifact, reported against the row.
	if err := os.Remove(payloadPath); err != nil {
		t.Fatal(err)
	}
	result, err = Verify(ctx, typedArtifactLister{rows: rows}, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.MissingFiles != 1 || len(result.MissingPaths) != 1 || result.MissingPaths[0] != manifestPath {
		t.Fatalf("a missing payload is one missing pair, got %d/%v", result.MissingFiles, result.MissingPaths)
	}
	if result.OrphanFiles != 0 {
		t.Fatalf("got %d orphans, want 0", result.OrphanFiles)
	}
}

// TestInspectClassifiesManagedSubtreesAndWaveform pins the classification the
// settings page and the CLI table read: managed search references, active
// inference scratch, and registered waveform pairs are their own categories,
// never orphans; a payload no manifest names is an orphan.
func TestInspectClassifiesManagedSubtreesAndWaveform(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	_, payloadPath, _ := waveformPairFixture(t, cacheDir, "assetA")
	writeFile(t, filepath.Join(cacheDir, "search-references", "abc.jpg"), 4)
	writeFile(t, filepath.Join(cacheDir, "inference", "run1.bin"), 6)
	lone := filepath.Join(cacheDir, "assetA", "waveform."+strings.Repeat("cd", 32)+".bin")
	writeFile(t, lone, 3)

	stats, err := Inspect(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SearchReferenceCount != 1 || stats.SearchReferenceBytes != 4 {
		t.Fatalf("references = %d/%d, want 1/4", stats.SearchReferenceCount, stats.SearchReferenceBytes)
	}
	if stats.InferenceScratchCount != 1 || stats.InferenceScratchBytes != 6 {
		t.Fatalf("inference scratch = %d/%d, want 1/6", stats.InferenceScratchCount, stats.InferenceScratchBytes)
	}
	if stats.WaveformCount != 2 {
		t.Fatalf("waveform pair must count manifest and payload, got %d", stats.WaveformCount)
	}
	if stats.OrphanCount != 1 || stats.OrphanBytes != 3 {
		t.Fatalf("only the unregistered payload is an orphan, got %d/%d", stats.OrphanCount, stats.OrphanBytes)
	}
	// The registered pair is rebuildable content, and its registration is
	// what keeps the payload out of the orphan count.
	if stats.RebuildableBytes < stats.WaveformBytes {
		t.Fatalf("waveform bytes must be counted as rebuildable, %d < %d", stats.RebuildableBytes, stats.WaveformBytes)
	}
	if _, err := os.Stat(payloadPath); err != nil {
		t.Fatalf("inspection must not delete anything: %v", err)
	}
}
