package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// newScanRepo opens a real repository with one empty library root on disk, so
// the scan-enqueue contract is exercised against the real SQL rather than a
// hand-written fake.
func newScanRepo(t *testing.T) (*Repository, domain.LibraryRoot, string) {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "scan-enqueue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Join(t.TempDir(), "footage")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, rootDir)
	if err != nil {
		t.Fatal(err)
	}
	return repo, root, rootDir
}

func scanWriteVideo(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("fake video bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The whole point of the changed set: a revisit of an untouched file must not
// be reported as changed, or the 15-minute rescan would re-enqueue the library
// every pass.
func TestUpsertScannedFileUnchangedRevisitIsNotChanged(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)
	path := filepath.Join(rootDir, "clip.mp4")
	scanWriteVideo(t, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.UpsertScannedFile(ctx, root, "clip.mp4", path, info, "fp-clip")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || !first.Changed || first.AssetID == "" {
		t.Fatalf("first scan created=%v changed=%v asset=%q, want both true and an id", first.Created, first.Changed, first.AssetID)
	}
	second, err := repo.UpsertScannedFile(ctx, root, "clip.mp4", path, info, "fp-clip")
	if err != nil {
		t.Fatal(err)
	}
	if second.Created {
		t.Fatalf("rescan of the same file reported Created")
	}
	if second.Changed {
		t.Fatalf("rescan of an unchanged file reported Changed")
	}
	if second.AssetID != first.AssetID {
		t.Fatalf("rescan resolved a different asset %q vs %q", second.AssetID, first.AssetID)
	}
}

// A mtime change is exactly the signal the probe input hash keys on, so it must
// surface as Changed.
func TestUpsertScannedFileMtimeChangeIsChanged(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)
	path := filepath.Join(rootDir, "clip.mp4")
	scanWriteVideo(t, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertScannedFile(ctx, root, "clip.mp4", path, info, "fp-clip"); err != nil {
		t.Fatal(err)
	}
	moved := info.ModTime().Add(2 * time.Hour)
	if err := os.Chtimes(path, moved, moved); err != nil {
		t.Fatal(err)
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := repo.UpsertScannedFile(ctx, root, "clip.mp4", path, info2, "fp-clip")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatalf("rescan after a mtime change reported Changed=false")
	}
	if result.Created {
		t.Fatalf("mtime change must not mint a new asset")
	}
}

// A brand-new file in an existing root is created and, being new, is always
// changed: nothing could have been derived from it yet.
func TestUpsertScannedFileBrandNewFileIsCreatedAndChanged(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir := newScanRepo(t)
	path := filepath.Join(rootDir, "new.mp4")
	scanWriteVideo(t, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := repo.UpsertScannedFile(ctx, root, "new.mp4", path, info, "fp-new")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created {
		t.Fatalf("new file reported Created=false")
	}
	if !result.Changed {
		t.Fatalf("new file reported Changed=false")
	}
}

// AssetsWithoutProbeJob must return exactly the live assets in a root that have
// no probe job yet: an asset whose probe job already exists drops out, and an
// asset in a different root is never in scope.
func TestAssetsWithoutProbeJobScopesToRootAndSkipsProbedAssets(t *testing.T) {
	ctx := context.Background()
	repo, rootA, rootADir := newScanRepo(t)
	rootBDir := filepath.Join(t.TempDir(), "footage-b")
	if err := os.MkdirAll(rootBDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootB, err := repo.CreateLibraryRoot(ctx, rootBDir)
	if err != nil {
		t.Fatal(err)
	}

	pathA := filepath.Join(rootADir, "a.mp4")
	scanWriteVideo(t, pathA)
	infoA, err := os.Stat(pathA)
	if err != nil {
		t.Fatal(err)
	}
	assetA, err := repo.UpsertScannedFile(ctx, rootA, "a.mp4", pathA, infoA, "fp-a")
	if err != nil {
		t.Fatal(err)
	}

	pathB := filepath.Join(rootADir, "b.mp4")
	scanWriteVideo(t, pathB)
	infoB, err := os.Stat(pathB)
	if err != nil {
		t.Fatal(err)
	}
	assetB, err := repo.UpsertScannedFile(ctx, rootA, "b.mp4", pathB, infoB, "fp-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, assetB.AssetID, domain.JobProbe, "probe-b", 100); err != nil {
		t.Fatal(err)
	}

	pathC := filepath.Join(rootBDir, "c.mp4")
	scanWriteVideo(t, pathC)
	infoC, err := os.Stat(pathC)
	if err != nil {
		t.Fatal(err)
	}
	assetC, err := repo.UpsertScannedFile(ctx, rootB, "c.mp4", pathC, infoC, "fp-c")
	if err != nil {
		t.Fatal(err)
	}

	got, err := repo.AssetsWithoutProbeJob(ctx, rootA.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != assetA.AssetID {
		t.Fatalf("AssetsWithoutProbeJob(rootA)=%v, want exactly the unprobed asset %q", got, assetA.AssetID)
	}
	if got[0] == assetB.AssetID || got[0] == assetC.AssetID {
		t.Fatalf("probe job or other-root asset leaked into the result: %v", got)
	}

	// Once the last unprobed asset gets its probe job, the root is clean.
	if err := repo.EnqueueJob(ctx, assetA.AssetID, domain.JobProbe, "probe-a", 100); err != nil {
		t.Fatal(err)
	}
	got, err = repo.AssetsWithoutProbeJob(ctx, rootA.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("root A should have no unprobed assets after enqueuing probe-a, got %v", got)
	}
}

// A change in one root must never pull another root's assets into the scan's
// changed set.
func TestUpsertScannedFileChangeInOneRootDoesNotAffectAnother(t *testing.T) {
	ctx := context.Background()
	repo, rootA, rootADir := newScanRepo(t)
	rootBDir := filepath.Join(t.TempDir(), "footage-b")
	if err := os.MkdirAll(rootBDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootB, err := repo.CreateLibraryRoot(ctx, rootBDir)
	if err != nil {
		t.Fatal(err)
	}
	pathA := filepath.Join(rootADir, "a.mp4")
	scanWriteVideo(t, pathA)
	infoA, err := os.Stat(pathA)
	if err != nil {
		t.Fatal(err)
	}
	assetA, err := repo.UpsertScannedFile(ctx, rootA, "a.mp4", pathA, infoA, "fp-a")
	if err != nil {
		t.Fatal(err)
	}
	pathB := filepath.Join(rootBDir, "b.mp4")
	scanWriteVideo(t, pathB)
	infoB, err := os.Stat(pathB)
	if err != nil {
		t.Fatal(err)
	}
	assetB, err := repo.UpsertScannedFile(ctx, rootB, "b.mp4", pathB, infoB, "fp-b")
	if err != nil {
		t.Fatal(err)
	}

	moved := infoA.ModTime().Add(2 * time.Hour)
	if err := os.Chtimes(pathA, moved, moved); err != nil {
		t.Fatal(err)
	}
	infoA2, err := os.Stat(pathA)
	if err != nil {
		t.Fatal(err)
	}
	result, err := repo.UpsertScannedFile(ctx, rootA, "a.mp4", pathA, infoA2, "fp-a")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatalf("touched root A file reported unchanged")
	}
	if result.AssetID != assetA.AssetID {
		t.Fatalf("root A scan returned %q, want %q", result.AssetID, assetA.AssetID)
	}
	if assetB.AssetID == assetA.AssetID {
		t.Fatalf("fixture collapsed both roots onto one asset")
	}
}
