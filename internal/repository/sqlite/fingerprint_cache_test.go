package sqlite

// Real-DB fixtures for the scan fingerprint cache. The claim under test is
// about what the database hands back and what the scanner does with it, so
// it belongs here rather than in internal/ingest, where the repository is a
// map and would agree with whatever the scanner asked it.
//
// Every fixture works the same way: rewrite a file's bytes while holding its
// size and mtime constant, then look at which identity ends up in the assets
// row. The old identity means the file was never opened.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/ingest"
)

func assetFingerprint(t *testing.T, repo *Repository, assetID string) string {
	t.Helper()
	var fingerprint string
	if err := repo.db.QueryRow(`SELECT quick_fingerprint FROM assets WHERE id=?`, assetID).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

// rewriteInPlace replaces a file's contents with equally long, different bytes
// and restores its mtime — the one change the cache is designed not to see.
// The replacement is derived from what is already there, so no caller has to
// count characters to keep the size fixed: one byte is inverted, which moves
// the fingerprint and cannot move the length.
func rewriteInPlace(t *testing.T, path string) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) == 0 {
		t.Fatal("an empty file has no byte to change, so the fixture would rewrite it\nto the same fingerprint and prove nothing")
	}
	content[0] ^= 0xff
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || after.ModTime() != before.ModTime() {
		t.Fatalf("the rewrite moved the size or the mtime (%d/%v -> %d/%v), so the cache\nwould miss for a reason these fixtures are not about", before.Size(), before.ModTime(), after.Size(), after.ModTime())
	}
}

func TestKnownFileReturnsWhatTheLastScanRecorded(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)
	path := filepath.Join(rootDir, "clip.mp4")
	scanWriteVideo(t, path)

	first := movedScan(t, repo, root)
	if first.Discovered != 1 {
		t.Fatalf("first scan discovered=%d, want 1", first.Discovered)
	}
	assetID := first.ChangedAssetIDs[0]

	known, ok, err := repo.KnownFile(ctx, root.ID, "clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the row the scan just wrote is not visible to KnownFile, so the cache\ncan never hit and every scan re-reads every file")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if known.Fingerprint != assetFingerprint(t, repo, assetID) {
		t.Fatalf("fingerprint = %q, want the one on the asset row", known.Fingerprint)
	}
	if known.Size != info.Size() {
		t.Fatalf("size = %d, want %d — the size comes from the asset row and must still\ndescribe the file at this path", known.Size, info.Size())
	}
	if known.ModifiedNS != info.ModTime().UnixNano() {
		t.Fatalf("mtime = %d, want %d", known.ModifiedNS, info.ModTime().UnixNano())
	}
}

func TestKnownFileMissesOnAPathNoScanHasSeen(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)
	scanWriteVideo(t, filepath.Join(rootDir, "clip.mp4"))
	movedScan(t, repo, root)

	if _, ok, err := repo.KnownFile(ctx, root.ID, "never-scanned.mp4"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("KnownFile reported a hit for a path with no location row")
	}
	// Same path, different root. The lookup is keyed by both, and a cache that
	// ignored the root would hand one root's identity to another's file.
	other, err := repo.CreateLibraryRoot(ctx, filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repo.KnownFile(ctx, other.ID, "clip.mp4"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("KnownFile matched a path in a different root")
	}
}

func TestRescanOfAnUntouchedFileDoesNotReadItAgain(t *testing.T) {
	repo, root, rootDir := newScanRepo(t)
	path := filepath.Join(rootDir, "clip.mp4")
	scanWriteVideo(t, path)

	first := movedScan(t, repo, root)
	assetID := first.ChangedAssetIDs[0]
	original := assetFingerprint(t, repo, assetID)

	rewriteInPlace(t, path)

	second := movedScan(t, repo, root)
	if second.Discovered != 0 || second.Linked != 1 {
		t.Fatalf("rescan discovered=%d linked=%d, want 0 and 1", second.Discovered, second.Linked)
	}
	if got := countAssets(t, repo); got != 1 {
		t.Fatalf("the rescan minted a second asset: %d assets — the file's bytes were\nread, so it fingerprinted differently and deduplicated to nothing", got)
	}
	if got := assetFingerprint(t, repo, assetID); got != original {
		t.Fatalf("fingerprint changed from %q to %q: the rescan read a file whose size\nand mtime had not moved, which is the whole cost this cache removes", original, got)
	}
	if len(second.ChangedAssetIDs) != 0 {
		t.Fatalf("rescan reported changed=%v, want none", second.ChangedAssetIDs)
	}
}

func TestDeepRescanReadsTheFileTheOrdinaryRescanTrusts(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)
	path := filepath.Join(rootDir, "clip.mp4")
	scanWriteVideo(t, path)

	first := movedScan(t, repo, root)
	original := assetFingerprint(t, repo, first.ChangedAssetIDs[0])

	rewriteInPlace(t, path)

	result, err := ingest.NewScanner(repo).ScanDeep(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkUnseenLocationsMissing(ctx, root.ID, result.SeenRelativePaths); err != nil {
		t.Fatal(err)
	}
	// The rewritten bytes fingerprint differently, and identity is
	// (quick_fingerprint, file_size) — so a deep scan of rewritten content is
	// a different asset, and the location moves onto it. That is the correct
	// answer: the bytes really are different footage.
	if result.Discovered != 1 {
		t.Fatalf("deep rescan discovered=%d, want 1 — it did not read the file", result.Discovered)
	}
	known, ok, err := repo.KnownFile(ctx, root.ID, "clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the location row vanished")
	}
	if known.Fingerprint == original {
		t.Fatalf("after --deep the path still carries the stale identity %q", original)
	}
}

func TestKnownFileStillAnswersForALocationMarkedMissing(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)
	path := filepath.Join(rootDir, "clip.mp4")
	scanWriteVideo(t, path)
	movedScan(t, repo, root)

	// Reconcile against an empty seen list, exactly as a scan of an unmounted
	// root would: the location survives with exists_now=0.
	if _, err := repo.MarkUnseenLocationsMissing(ctx, root.ID, nil); err != nil {
		t.Fatal(err)
	}
	var existsNow int
	if err := repo.db.QueryRow(`SELECT exists_now FROM asset_locations WHERE root_id=? AND relative_path=?`, root.ID, "clip.mp4").Scan(&existsNow); err != nil {
		t.Fatal(err)
	}
	if existsNow != 0 {
		t.Fatalf("exists_now = %d, want 0 — this fixture is not testing what it says", existsNow)
	}

	// A share that comes back with the file untouched must hit the cache. If
	// exists_now were part of the lookup, every remount would re-read the
	// whole library — the exact case a NAS user hits most often.
	if _, ok, err := repo.KnownFile(ctx, root.ID, "clip.mp4"); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("KnownFile refused a location marked missing, so remounting a share\nre-reads every file in it")
	}

	rewriteInPlace(t, path)
	original := assetFingerprint(t, repo, movedScanAssetID(t, repo))
	movedScan(t, repo, root)
	if got := assetFingerprint(t, repo, movedScanAssetID(t, repo)); got != original {
		t.Fatalf("the remount scan re-read the file: fingerprint %q became %q", original, got)
	}
	if got := countAssets(t, repo); got != 1 {
		t.Fatalf("the remount scan minted %d assets, want 1", got)
	}
}

// movedScanAssetID returns the id of the only asset in the fixture database.
func movedScanAssetID(t *testing.T, repo *Repository) string {
	t.Helper()
	var id string
	if err := repo.db.QueryRow(`SELECT id FROM assets`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestKnownFileDescribesThePathNotJustTheAsset is the case the size field
// invites: assets.file_size is per ASSET while asset_locations.modified_ns is
// per PATH, and one asset can be reached through several paths. Reading a size
// off a row shared by two files looks like it could describe the wrong one.
//
// It cannot, and the reason sits lower than the dedup key: QuickFingerprint
// hashes the size into the digest before it reads a single byte
// (internal/ingest/fingerprint.go:22), so two files of different sizes can
// never share a fingerprint, and an asset row can never describe two different
// sizes. The `AND file_size = ?` in the dedup lookup is belt-and-braces over
// that, not the thing that makes it true.
func TestKnownFileDescribesThePathNotJustTheAsset(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)
	first := filepath.Join(rootDir, "take-a.mp4")
	second := filepath.Join(rootDir, "take-b.mp4")
	scanWriteVideo(t, first)
	scanWriteVideo(t, second)

	scan := movedScan(t, repo, root)
	if got := countAssets(t, repo); got != 1 {
		t.Fatalf("two byte-identical files produced %d assets, want 1 — this fixture is\nnot testing what it says", got)
	}
	assetID := scan.ChangedAssetIDs[0]
	original := assetFingerprint(t, repo, assetID)

	// take-b now holds different bytes of a different length. take-a is
	// untouched. If KnownFile handed take-b the shared row's size and that
	// size still matched, the changed file would keep the old identity.
	if err := os.WriteFile(second, []byte("a longer set of video bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	known, ok, err := repo.KnownFile(ctx, root.ID, "take-b.mp4")
	if err != nil || !ok {
		t.Fatalf("KnownFile(take-b) ok=%v err=%v", ok, err)
	}
	info, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if known.Size == info.Size() {
		t.Fatalf("the stored size (%d) still matches the rewritten file, so the cache\nwould hand back take-a's identity for take-b's bytes", known.Size)
	}

	second2 := movedScan(t, repo, root)
	if got := countAssets(t, repo); got != 2 {
		t.Fatalf("after rewriting one of the two copies there are %d assets, want 2", got)
	}
	if got := assetFingerprint(t, repo, assetID); got != original {
		t.Fatalf("the untouched file's asset changed identity: %q -> %q", original, got)
	}
	if len(second2.ChangedAssetIDs) != 1 {
		t.Fatalf("rescan reported %d changed assets, want exactly the rewritten one", len(second2.ChangedAssetIDs))
	}
	// take-a must still resolve, and to the identity it always had.
	stillA, ok, err := repo.KnownFile(ctx, root.ID, "take-a.mp4")
	if err != nil || !ok {
		t.Fatalf("KnownFile(take-a) ok=%v err=%v", ok, err)
	}
	if stillA.Fingerprint != original {
		t.Fatalf("take-a's cached identity became %q, want %q", stillA.Fingerprint, original)
	}

	// The invariant the whole cache leans on, checked against the real
	// database rather than argued: for every live location, the asset row's
	// file_size is the size of the file at that path. That is what lets
	// KnownFile hand a per-asset size to a per-path question.
	rows, err := repo.db.Query(`SELECT l.absolute_path, a.file_size FROM asset_locations l JOIN assets a ON a.id=l.asset_id WHERE l.exists_now=1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var path string
		var stored int64
		if err := rows.Scan(&path, &stored); err != nil {
			t.Fatal(err)
		}
		onDisk, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if stored != onDisk.Size() {
			t.Fatalf("%s: asset row says %d bytes, the file is %d", path, stored, onDisk.Size())
		}
		checked++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if checked != 2 {
		t.Fatalf("checked %d live locations, want 2 — a passing result above would not\nmean the invariant was tested", checked)
	}
}
