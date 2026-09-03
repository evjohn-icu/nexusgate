package app

// The probe job's input hash is the dedup key for the entire pipeline chain,
// so it decides what a moved file costs. These tests pin EnqueueAsset's hash
// to content identity (fingerprint+size+mtime) against the real repository:
// a pure rename must enqueue the SAME hash, which makes the post-move
// re-enqueue a no-op instead of a fresh paid analysis.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/ingest"
	"github.com/evjohn-icu/nexusslate/internal/media"
	sqlite "github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
)

// newProbeAsset builds a real repository with one library root and one
// scanned asset, returning the root's dir for the move step.
func newProbeAsset(t *testing.T) (*sqlite.Repository, domain.LibraryRoot, string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dir, "probe.db"))
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
	root, err := repo.CreateLibraryRoot(ctx, rootDir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rootDir, "clip.mp4")
	scanWriteVideoFile(t, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := ingest.QuickFingerprint(path, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertScannedFile(ctx, root, "clip.mp4", path, info, fingerprint); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v, want exactly one", assets, err)
	}
	return repo, root, rootDir, assets[0].ID
}

func TestEnqueueAssetProbeHashContentDerivedAndMoveStable(t *testing.T) {
	ctx := context.Background()
	repo, root, rootDir, assetID := newProbeAsset(t)
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, nil, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)

	path1 := filepath.Join(rootDir, "clip.mp4")
	info, err := os.Stat(path1)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := ingest.QuickFingerprint(path1, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	expected := ingest.StableAssetKey(fingerprint, info.Size(), info.ModTime().UnixNano())

	if err := pipeline.EnqueueAsset(ctx, assetID); err != nil {
		t.Fatal(err)
	}
	jobs, err := repo.ListJobs(ctx, 10)
	if err != nil || len(jobs) != 1 || jobs[0].Type != domain.JobProbe {
		t.Fatalf("jobs=%+v err=%v, want exactly one probe job", jobs, err)
	}
	if jobs[0].InputHash != expected {
		t.Fatalf("probe input hash = %q, want content-derived %q", jobs[0].InputHash, expected)
	}
	// The pre-fix format was sha256(path, mtime); if the path leaked back
	// into the hash the values could not be equal, so a match here means the
	// path is in the key again.
	if jobs[0].InputHash == hashStrings(path1, fmt.Sprint(info.ModTime().UnixNano())) {
		t.Fatal("probe input hash still derived from the path (old format)")
	}

	// A pure rename keeps content and mtime: the second EnqueueAsset must
	// enqueue the same hash, and the queue must not grow.
	path2 := filepath.Join(rootDir, "moved.mp4")
	if err := os.Rename(path1, path2); err != nil {
		t.Fatal(err)
	}
	info2, err := os.Stat(path2)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint2, err := ingest.QuickFingerprint(path2, info2.Size())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertScannedFile(ctx, root, "moved.mp4", path2, info2, fingerprint2); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.EnqueueAsset(ctx, assetID); err != nil {
		t.Fatal(err)
	}
	jobs, err = repo.ListJobs(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("after a pure move jobs=%+v err=%v, want still exactly one probe job", jobs, err)
	}
	if jobs[0].InputHash != expected {
		t.Fatalf("probe input hash changed across a pure move: %q -> %q", expected, jobs[0].InputHash)
	}
}
