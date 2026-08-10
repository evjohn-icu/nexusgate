package sqlite

// Real-DB regression fixtures for the moved-file identity contract: rename,
// folder move, hardlink, byte-identical duplicate, case change and remount
// all resolve to the SAME asset — never a duplicate — and a pure move must
// not re-enqueue the pipeline chain (the probe input hash is content-derived,
// so the moved file's new probe job carries the old hash and INSERT OR IGNORE
// makes the re-enqueue a no-op).
//
// Every fixture drives the real scanner (ingest.NewScanner) against the real
// repository and real files on disk, because what these tests are about is
// what the database actually does with moved files.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/ingest"
)

// movedScan runs one real scanner pass over the fixture root and then applies
// the missing-file reconciliation exactly as the app-layer gate does
// (internal/app ScanLibraryRoot): the scanner only reports the seen list, and
// marking unseen files missing is decided AFTER the walk from a healthy-root
// verdict — which these fixtures always are.
func movedScan(t *testing.T, repo *Repository, root domain.LibraryRoot) domain.ScanResult {
	t.Helper()
	ctx := context.Background()
	result, err := ingest.NewScanner(repo).Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := repo.MarkUnseenLocationsMissing(ctx, root.ID, result.SeenRelativePaths)
	if err != nil {
		t.Fatal(err)
	}
	result.Missing = missing
	return result
}

func countAssets(t *testing.T, repo *Repository) int {
	t.Helper()
	var n int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countLocationsWithExists(t *testing.T, repo *Repository, assetID string, existsNow int) int {
	t.Helper()
	var n int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM asset_locations WHERE asset_id=? AND exists_now=?`, assetID, existsNow).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countProbeJobs(t *testing.T, repo *Repository, assetID string) int {
	t.Helper()
	var n int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE asset_id=? AND job_type=?`, assetID, string(domain.JobProbe)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// pathMtimeHash reproduces the PRE-FIX probe input hash (sha256 of absolute
// path + mtime). It exists only to prove the fix matters: that key changed
// with the path, so a moved file minted a fresh probe job and re-paid the
// whole downstream chain.
func pathMtimeHash(path string, modifiedNS int64) string {
	h := sha256.New()
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(modifiedNS, 10)))
	h.Write([]byte{0})
	return hex.EncodeToString(h.Sum(nil))
}

func TestRenameKeepsSameAsset(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)

	path1 := filepath.Join(rootDir, "clip.mp4")
	scanWriteVideo(t, path1)

	first := movedScan(t, repo, root)
	if first.Discovered != 1 || len(first.ChangedAssetIDs) != 1 {
		t.Fatalf("first scan discovered=%d changed=%v, want 1 and exactly the new clip", first.Discovered, first.ChangedAssetIDs)
	}
	assetID := first.ChangedAssetIDs[0]

	path2 := filepath.Join(rootDir, "renamed.mp4")
	if err := os.Rename(path1, path2); err != nil {
		t.Fatal(err)
	}

	second := movedScan(t, repo, root)
	if second.Discovered != 0 || second.Linked != 1 {
		t.Fatalf("rescan after rename discovered=%d linked=%d, want 0 and 1", second.Discovered, second.Linked)
	}
	if len(second.ChangedAssetIDs) != 1 || second.ChangedAssetIDs[0] != assetID {
		t.Fatalf("rescan after rename changed=%v, want only the moved asset %q", second.ChangedAssetIDs, assetID)
	}
	if got := countAssets(t, repo); got != 1 {
		t.Fatalf("a rename minted a second asset: %d assets", got)
	}
	loc, err := repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if loc.AbsolutePath != path2 {
		t.Fatalf("primary location after rename = %q, want %q", loc.AbsolutePath, path2)
	}
	if got := countLocationsWithExists(t, repo, assetID, 1); got != 1 {
		t.Fatalf("live locations = %d, want 1 (the new path)", got)
	}
	if got := countLocationsWithExists(t, repo, assetID, 0); got != 1 {
		t.Fatalf("dead locations = %d, want 1 (the old path must die)", got)
	}
}

func TestMoveFolderKeepsSameAsset(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)

	sub1 := filepath.Join(rootDir, "sub1")
	if err := os.MkdirAll(sub1, 0o755); err != nil {
		t.Fatal(err)
	}
	path1 := filepath.Join(sub1, "clip.mp4")
	scanWriteVideo(t, path1)

	first := movedScan(t, repo, root)
	if len(first.ChangedAssetIDs) != 1 {
		t.Fatalf("first scan changed=%v, want exactly the new clip", first.ChangedAssetIDs)
	}
	assetID := first.ChangedAssetIDs[0]

	sub2 := filepath.Join(rootDir, "sub2")
	if err := os.Rename(sub1, sub2); err != nil {
		t.Fatal(err)
	}
	path2 := filepath.Join(sub2, "clip.mp4")

	second := movedScan(t, repo, root)
	if second.Discovered != 0 || second.Linked != 1 {
		t.Fatalf("rescan after folder move discovered=%d linked=%d, want 0 and 1", second.Discovered, second.Linked)
	}
	if len(second.ChangedAssetIDs) != 1 || second.ChangedAssetIDs[0] != assetID {
		t.Fatalf("rescan after folder move changed=%v, want only the moved asset %q", second.ChangedAssetIDs, assetID)
	}
	if got := countAssets(t, repo); got != 1 {
		t.Fatalf("a folder move minted a second asset: %d assets", got)
	}
	loc, err := repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if loc.AbsolutePath != path2 {
		t.Fatalf("primary location after folder move = %q, want %q", loc.AbsolutePath, path2)
	}
	if got := countLocationsWithExists(t, repo, assetID, 0); got != 1 {
		t.Fatalf("dead locations = %d, want 1 (the old sub1 path must die)", got)
	}
}

// A hardlink is the same inode under two names: same fingerprint, same size,
// same mtime. Both paths must land on the same asset with two live locations.
func TestHardlinkSameAsset(t *testing.T) {
	repo, root, rootDir := newScanRepo(t)

	path1 := filepath.Join(rootDir, "clip1.mp4")
	scanWriteVideo(t, path1)
	path2 := filepath.Join(rootDir, "clip2.mp4")
	if err := os.Link(path1, path2); err != nil {
		t.Fatal(err)
	}

	result := movedScan(t, repo, root)
	if result.Discovered != 1 || result.Linked != 1 {
		t.Fatalf("hardlink scan discovered=%d linked=%d, want 1 and 1", result.Discovered, result.Linked)
	}
	if got := countAssets(t, repo); got != 1 {
		t.Fatalf("a hardlink minted a second asset: %d assets", got)
	}
	if len(result.ChangedAssetIDs) != 1 {
		t.Fatalf("hardlink scan changed=%v, want exactly one asset", result.ChangedAssetIDs)
	}
	assetID := result.ChangedAssetIDs[0]
	if got := countLocationsWithExists(t, repo, assetID, 1); got != 2 {
		t.Fatalf("live locations = %d, want 2 (both hardlink names)", got)
	}
}

// Two paths with byte-identical content are the same asset: the fingerprint
// matches, so the second path links onto the first asset instead of minting
// a duplicate.
func TestByteIdenticalDuplicateSingleAsset(t *testing.T) {
	repo, root, rootDir := newScanRepo(t)

	path1 := filepath.Join(rootDir, "original.mp4")
	scanWriteVideo(t, path1)
	data, err := os.ReadFile(path1)
	if err != nil {
		t.Fatal(err)
	}
	path2 := filepath.Join(rootDir, "copy.mp4")
	if err := os.WriteFile(path2, data, 0o600); err != nil {
		t.Fatal(err)
	}

	result := movedScan(t, repo, root)
	if result.Discovered != 1 || result.Linked != 1 {
		t.Fatalf("duplicate scan discovered=%d linked=%d, want 1 and 1", result.Discovered, result.Linked)
	}
	if got := countAssets(t, repo); got != 1 {
		t.Fatalf("a byte-identical copy minted a second asset: %d assets", got)
	}
	if len(result.ChangedAssetIDs) != 1 {
		t.Fatalf("duplicate scan changed=%v, want exactly one asset", result.ChangedAssetIDs)
	}
	assetID := result.ChangedAssetIDs[0]
	if got := countLocationsWithExists(t, repo, assetID, 1); got != 2 {
		t.Fatalf("live locations = %d, want 2 (original + copy)", got)
	}
}

func TestByteIdenticalCopyAfterCommittedAnalysisKeepsProbeIdentity(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)
	path1 := filepath.Join(rootDir, "original.mp4")
	scanWriteVideo(t, path1)
	first := movedScan(t, repo, root)
	if len(first.ChangedAssetIDs) != 1 {
		t.Fatalf("first scan changed=%v", first.ChangedAssetIDs)
	}
	assetID := first.ChangedAssetIDs[0]
	loc1, err := repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	probeHash := ingest.StableAssetKey(loc1.QuickFingerprint, loc1.FileSize, loc1.ProbeModifiedNS)
	if err := repo.EnqueueJob(ctx, assetID, domain.JobProbe, probeHash, 100); err != nil {
		t.Fatal(err)
	}
	runID, _, err := repo.CreateModelRun(ctx, assetID, "video_analysis", "fixture", "model", "analysis-input", "prompt", "schema", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, runID, "{}", `{}`); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitAnalysis(ctx, assetID, runID, "schema", domain.StructuredAnalysis{AssetType: "b_roll", ShotSize: "wide", Summary: "fixture"}); err != nil {
		t.Fatal(err)
	}
	var runs int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM model_runs WHERE asset_id=?`, assetID).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("model_runs before copy=%d err=%v, want 1", runs, err)
	}

	path2 := filepath.Join(rootDir, "copy.mp4")
	data, err := os.ReadFile(path1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path2, data, 0o600); err != nil {
		t.Fatal(err)
	}
	newTime := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(path2, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	second := movedScan(t, repo, root)
	if len(second.ChangedAssetIDs) != 1 || second.ChangedAssetIDs[0] != assetID || countAssets(t, repo) != 1 {
		t.Fatalf("copy scan changed=%v assets=%d, want same asset %q", second.ChangedAssetIDs, countAssets(t, repo), assetID)
	}
	loc2, err := repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if loc2.ProbeModifiedNS != loc1.ProbeModifiedNS {
		t.Fatalf("probe mtime changed across duplicate copy: %d -> %d", loc1.ProbeModifiedNS, loc2.ProbeModifiedNS)
	}
	if err := repo.EnqueueJob(ctx, assetID, domain.JobProbe, probeHash, 100); err != nil {
		t.Fatal(err)
	}
	if got := countProbeJobs(t, repo, assetID); got != 1 {
		t.Fatalf("probe jobs after duplicate copy=%d, want 1", got)
	}
	if err := repo.EnqueueJob(ctx, assetID, domain.JobProbe, probeHash, 100); err != nil {
		t.Fatal(err)
	}
	if got := probeHash; got != ingest.StableAssetKey(loc2.QuickFingerprint, loc2.FileSize, loc2.ProbeModifiedNS) {
		t.Fatalf("probe hash changed: %q -> %q", got, ingest.StableAssetKey(loc2.QuickFingerprint, loc2.FileSize, loc2.ProbeModifiedNS))
	}
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM model_runs WHERE asset_id=?`, assetID).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("model_runs after copy=%d err=%v, want 1", runs, err)
	}
}

// A case-only rename is one rename on a case-insensitive filesystem — the NAS
// reality this covers — and even on a case-sensitive one the fingerprint keeps
// identity. Either way: same asset, old location dies, no duplicate.
func TestCaseChangeSameAsset(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)

	path1 := filepath.Join(rootDir, "CLIP.MOV")
	scanWriteVideo(t, path1)

	first := movedScan(t, repo, root)
	if len(first.ChangedAssetIDs) != 1 {
		t.Fatalf("first scan changed=%v, want exactly the new clip", first.ChangedAssetIDs)
	}
	assetID := first.ChangedAssetIDs[0]

	path2 := filepath.Join(rootDir, "clip.mov")
	if err := os.Rename(path1, path2); err != nil {
		t.Fatal(err)
	}

	second := movedScan(t, repo, root)
	if second.Discovered != 0 || second.Linked != 1 {
		t.Fatalf("rescan after case change discovered=%d linked=%d, want 0 and 1", second.Discovered, second.Linked)
	}
	if len(second.ChangedAssetIDs) != 1 || second.ChangedAssetIDs[0] != assetID {
		t.Fatalf("rescan after case change changed=%v, want only the moved asset %q", second.ChangedAssetIDs, assetID)
	}
	if got := countAssets(t, repo); got != 1 {
		t.Fatalf("a case change minted a second asset: %d assets", got)
	}
	loc, err := repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if loc.AbsolutePath != path2 {
		t.Fatalf("primary location after case change = %q, want %q", loc.AbsolutePath, path2)
	}
	if got := countLocationsWithExists(t, repo, assetID, 0); got != 1 {
		t.Fatalf("dead locations = %d, want 1 (the old casing must die)", got)
	}
}

// The paid-re-analysis fix, pinned at the database: a pure move keeps
// fingerprint, size and mtime, so the probe job's input hash (EnqueueAsset's
// key, computed exactly as the pipeline computes it) is unchanged, and the
// post-move re-enqueue dedups to nothing. The pre-fix path-based hash, shown
// here to differ after the move, would have minted a second probe job and
// re-paid the whole chain.
func TestProbeHashStableAcrossMove(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)

	path1 := filepath.Join(rootDir, "clip.mp4")
	scanWriteVideo(t, path1)

	first := movedScan(t, repo, root)
	if len(first.ChangedAssetIDs) != 1 {
		t.Fatalf("first scan changed=%v, want exactly the new clip", first.ChangedAssetIDs)
	}
	assetID := first.ChangedAssetIDs[0]

	loc1, err := repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if loc1.QuickFingerprint == "" || loc1.FileSize == 0 {
		t.Fatalf("primary location must carry the asset's fingerprint and size for the stable key: %+v", loc1)
	}
	key1 := ingest.StableAssetKey(loc1.QuickFingerprint, loc1.FileSize, loc1.ProbeModifiedNS)
	if err := repo.EnqueueJob(ctx, assetID, domain.JobProbe, key1, 100); err != nil {
		t.Fatal(err)
	}

	path2 := filepath.Join(rootDir, "moved.mp4")
	if err := os.Rename(path1, path2); err != nil {
		t.Fatal(err)
	}
	second := movedScan(t, repo, root)
	if len(second.ChangedAssetIDs) != 1 || second.ChangedAssetIDs[0] != assetID {
		t.Fatalf("rescan after move changed=%v, want only the moved asset %q", second.ChangedAssetIDs, assetID)
	}

	loc2, err := repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if loc2.QuickFingerprint != loc1.QuickFingerprint || loc2.FileSize != loc1.FileSize || loc2.ModifiedNS != loc1.ModifiedNS {
		t.Fatalf("a pure move changed the identity inputs: fingerprint %q->%q size %d->%d mtime %d->%d",
			loc1.QuickFingerprint, loc2.QuickFingerprint, loc1.FileSize, loc2.FileSize, loc1.ModifiedNS, loc2.ModifiedNS)
	}
	key2 := ingest.StableAssetKey(loc2.QuickFingerprint, loc2.FileSize, loc2.ProbeModifiedNS)
	if key2 != key1 {
		t.Fatalf("probe input hash changed across a pure move: %q -> %q", key1, key2)
	}

	// What EnqueueAsset does after the move: enqueue the (unchanged) hash.
	// INSERT OR IGNORE must make it a no-op.
	if err := repo.EnqueueJob(ctx, assetID, domain.JobProbe, key2, 100); err != nil {
		t.Fatal(err)
	}
	if got := countProbeJobs(t, repo, assetID); got != 1 {
		t.Fatalf("probe jobs after move = %d, want 1 (stable key dedups the re-enqueue)", got)
	}

	// The old path-based key would have minted a second job: prove the fix
	// changes something observable, or this whole test proves nothing.
	old1 := pathMtimeHash(path1, loc1.ModifiedNS)
	old2 := pathMtimeHash(path2, loc2.ModifiedNS)
	if old1 == old2 {
		t.Fatal("test premise broken: path-based hash unchanged by the move")
	}
	if err := repo.EnqueueJob(ctx, assetID, domain.JobProbe, old2, 100); err != nil {
		t.Fatal(err)
	}
	if got := countProbeJobs(t, repo, assetID); got != 2 {
		t.Fatalf("path-based hash after move produced %d jobs, want 2 (proving the old key re-enqueued the chain)", got)
	}
}
