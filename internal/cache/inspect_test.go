package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	sqliterepo "github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
)

func writeFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The FS-only pass must classify every file the pipeline writes into the
// right bucket purely from its name and location: thumbnails, proxies,
// audio tracks, NAS staging copies under sources/, transient analysis
// frames, and anything unrecognized as an orphan.
func TestInspectClassifiesCacheLayout(t *testing.T) {
	cacheDir := t.TempDir()
	writeFile(t, filepath.Join(cacheDir, "asset-aaa", "thumbnail-software.jpg"), 1000)
	writeFile(t, filepath.Join(cacheDir, "asset-aaa", "thumbnail-nvenc.jpg"), 4000)
	writeFile(t, filepath.Join(cacheDir, "asset-aaa", "proxy-software.mp4"), 2000)
	writeFile(t, filepath.Join(cacheDir, "asset-aaa", "proxy-nvenc.mp4"), 5000)
	writeFile(t, filepath.Join(cacheDir, "asset-aaa", "audio.m4a"), 300)
	writeFile(t, filepath.Join(cacheDir, "asset-aaa", "analysis-frames", "frame-0001.jpg"), 50)
	writeFile(t, filepath.Join(cacheDir, "asset-aaa", "analysis-windows", "window-0001.jpg"), 60)
	writeFile(t, filepath.Join(cacheDir, "asset-aaa", "stray.bin"), 7)
	writeFile(t, filepath.Join(cacheDir, "sources", "asset-bbb", "copy-v1.mp4"), 9000)
	writeFile(t, filepath.Join(cacheDir, "tmp", "partial-proxy.mp4"), 11)

	stats, err := Inspect(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ThumbnailCount != 2 || stats.ThumbnailBytes != 5000 {
		t.Fatalf("thumbnails = %d files, %d bytes; want 2 files, 5000 bytes", stats.ThumbnailCount, stats.ThumbnailBytes)
	}
	if stats.ProxyCount != 2 || stats.ProxyBytes != 7000 {
		t.Fatalf("proxies = %d files, %d bytes; want 2 files, 7000 bytes", stats.ProxyCount, stats.ProxyBytes)
	}
	if stats.AudioCount != 1 || stats.AudioBytes != 300 {
		t.Fatalf("audio = %d files, %d bytes; want 1 file, 300 bytes", stats.AudioCount, stats.AudioBytes)
	}
	if stats.SourceStagingCount != 1 || stats.SourceStagingBytes != 9000 {
		t.Fatalf("source staging = %d files, %d bytes; want 1 file, 9000 bytes", stats.SourceStagingCount, stats.SourceStagingBytes)
	}
	if stats.ScratchBytes != 50+60+11 {
		t.Fatalf("scratch = %d bytes; want %d", stats.ScratchBytes, 50+60+11)
	}
	if stats.OrphanCount != 1 || stats.OrphanBytes != 7 {
		t.Fatalf("orphans = %d files, %d bytes; want 1 file, 7 bytes", stats.OrphanCount, stats.OrphanBytes)
	}
	if stats.TotalBytes != 5000+7000+300+9000+50+60+11+7 {
		t.Fatalf("total = %d bytes; want %d", stats.TotalBytes, 5000+7000+300+9000+50+60+11+7)
	}
	if stats.RebuildableBytes != 5000+7000+300+50+60+11 {
		t.Fatalf("rebuildable = %d bytes; want %d", stats.RebuildableBytes, 5000+7000+300+50+60+11)
	}
}

type fakeFileInfo struct {
	name string
	size int64
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o600 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

// Orphan detection is the one pass that needs the database: a directory for
// a known asset is not an orphan even though its files would be classified
// as normal thumbnails by name, and a directory whose assetID the assets
// table has never seen is stale. Recognized category subtrees (sources/,
// scratch dirs) must never be reported as orphan candidates.
func TestOrphanDirectoriesAgainstRepository(t *testing.T) {
	ctx := context.Background()
	repo, err := sqliterepo.Open(filepath.Join(t.TempDir(), "cache-health.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, "/staging/root")
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := repo.UpsertScannedFile(ctx, root, "clip.mp4", "/staging/root/clip.mp4", fakeFileInfo{name: "clip.mp4", size: 42}, "fp-orphan-test")
	if err != nil {
		t.Fatal(err)
	}

	cacheDir := t.TempDir()
	writeFile(t, filepath.Join(cacheDir, scanned.AssetID, "thumbnail-software.jpg"), 100)
	writeFile(t, filepath.Join(cacheDir, "asset-ghost", "thumbnail-software.jpg"), 200)
	writeFile(t, filepath.Join(cacheDir, "asset-ghost", "proxy-software.mp4"), 300)
	writeFile(t, filepath.Join(cacheDir, "sources", "asset-stale", "copy-v1.mp4"), 400)
	writeFile(t, filepath.Join(cacheDir, "tmp", "partial.mp4"), 500)

	orphans, err := OrphanDirectories(ctx, repo, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphan dirs = %d, want 1", len(orphans))
	}
	if filepath.Base(orphans[0].Path) != "asset-ghost" {
		t.Fatalf("orphan dir = %s, want asset-ghost", filepath.Base(orphans[0].Path))
	}
	if orphans[0].Bytes != 200+300 {
		t.Fatalf("orphan dir bytes = %d, want %d", orphans[0].Bytes, 200+300)
	}
}

func TestInspectMissingCacheDirIsEmpty(t *testing.T) {
	// A fresh install has no cache volume yet; inspection must report zeros
	// rather than fail, or tooling could not run before the first scan.
	stats, err := Inspect(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalFiles != 0 || stats.TotalBytes != 0 {
		t.Fatalf("missing cache dir = %d files, %d bytes; want empty", stats.TotalFiles, stats.TotalBytes)
	}
}
