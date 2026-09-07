// Package cache owns Hub cache-directory inspection and safe deletion. The
// cache directory holds only derived and transient content: thumbnails,
// proxies, audio extracts, analysis scratch, and per-asset directories. It
// never holds canonical data — the database, provider secrets and original
// media live outside it, and cache/sources/ (copy-mode NAS staging) is
// excluded from every deletion path here because it is the local copy a derive
// reads from. Its own reclaim lives elsewhere and always has: internal/staging
// evicts it inside Stage, under the configured byte cap, before it copies. The
// two must not both delete from that tree — this package's job is to be able
// to say "nothing here touched sources".
package cache

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// CacheStats is the result of Inspect: one count and byte total per
// classification, plus the walk totals. It is read-only information; nothing
// here deletes anything. The classification is the same one the settings
// page's disk picture uses (see inspectCacheDir in
// internal/app/storage_overview.go): swapping one walk for the other must not
// change a number, only where the code lives.
type CacheStats struct {
	ThumbnailCount int
	ThumbnailBytes int64
	ProxyCount     int
	ProxyBytes     int64
	AudioCount     int
	AudioBytes     int64

	SourceStagingCount int
	SourceStagingBytes int64

	ScratchCount int
	ScratchBytes int64

	OrphanCount int
	OrphanBytes int64

	TotalFiles int
	TotalBytes int64

	// RebuildableBytes is everything a gc of rebuildable content frees:
	// thumbnails, proxies, audio extracts and analysis scratch all
	// re-derive from original media.
	RebuildableBytes int64
}

// IsScratchDirName reports whether name is one of the transient analysis
// scratch directories: analysis-frames/ and analysis-windows/ inside the
// per-asset cache directories, plus top-level tmp/ and scratch/. They are
// cheap to recreate from the proxy and are supposed to be removed by the
// pipeline itself; anything still present is litter.
func IsScratchDirName(name string) bool {
	switch name {
	case "analysis-frames", "analysis-windows", "tmp", "scratch":
		return true
	}
	return false
}

// IsRebuildableArtifactName reports whether name is a derived artifact that a
// `cache gc --rebuildable` run deletes and the pipeline re-creates: the CLI
// enqueues a re-derive job for each affected committed asset (see the derive
// stage's committed check in internal/app/pipeline.go), and the job's
// os.IsNotExist guard renders only what is missing.
func IsRebuildableArtifactName(name string) bool {
	return (strings.HasPrefix(name, "thumbnail-") && strings.HasSuffix(name, ".jpg")) ||
		(strings.HasPrefix(name, "proxy-") && strings.HasSuffix(name, ".mp4")) ||
		(strings.HasPrefix(name, "audio") && strings.HasSuffix(name, ".m4a"))
}

// IsSourceStagingPath reports whether rel (a slash-separated path relative to
// the cache dir) lies under cache/sources/. Copy-mode staging mirrors NAS
// originals so derives do not re-read them over the network, and the copy is
// what a derive actually opens, so no deletion path in this package may touch
// it. Reclaiming that tree belongs to the one component that knows which entry
// is in use: internal/staging, which evicts least-recently-used entries inside
// Stage when the configured cap would be exceeded.
func IsSourceStagingPath(rel string) bool {
	return rel == "sources" || strings.HasPrefix(rel, "sources/")
}

// isScratchPath reports whether a slash-separated relative path lies inside a
// scratch directory: analysis-frames/ or analysis-windows/ at any depth, or a
// top-level tmp/ or scratch/.
func isScratchPath(rel string) bool {
	for _, component := range strings.Split(rel, "/") {
		if component == "analysis-frames" || component == "analysis-windows" {
			return true
		}
	}
	first, _, _ := strings.Cut(rel, "/")
	return first == "tmp" || first == "scratch"
}

// Inspect walks cacheDir once and classifies every file. A missing cache dir
// (fresh install) yields an empty CacheStats and no error — tooling must be
// runnable before the first scan — while individual unreadable entries are
// skipped rather than failing the whole walk, because one stuck file must not
// hide the rest of the disk picture.
func Inspect(cacheDir string) (CacheStats, error) {
	var stats CacheStats
	// WalkDir hands a root stat failure to the callback (which ignores
	// errors), so without this check a missing cache dir could not be told
	// apart from an empty one.
	if _, err := os.Stat(cacheDir); err != nil {
		if os.IsNotExist(err) {
			return stats, nil
		}
		return stats, err
	}
	err := filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		size := info.Size()
		rel, err := filepath.Rel(cacheDir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		name := filepath.Base(rel)
		stats.TotalFiles++
		stats.TotalBytes += size
		switch {
		case IsSourceStagingPath(rel):
			stats.SourceStagingCount++
			stats.SourceStagingBytes += size
		case isScratchPath(rel):
			stats.ScratchCount++
			stats.ScratchBytes += size
		case strings.HasPrefix(name, "thumbnail-") && strings.HasSuffix(name, ".jpg"):
			stats.ThumbnailCount++
			stats.ThumbnailBytes += size
		case strings.HasPrefix(name, "proxy-") && strings.HasSuffix(name, ".mp4"):
			stats.ProxyCount++
			stats.ProxyBytes += size
		case strings.HasPrefix(name, "audio") && strings.HasSuffix(name, ".m4a"):
			stats.AudioCount++
			stats.AudioBytes += size
		default:
			stats.OrphanCount++
			stats.OrphanBytes += size
		}
		return nil
	})
	if err != nil {
		return stats, err
	}
	stats.RebuildableBytes = stats.ThumbnailBytes + stats.ProxyBytes + stats.AudioBytes + stats.ScratchBytes
	return stats, nil
}

// AssetIDLister is the slice of the repository that OrphanDirectories needs:
// every asset id the assets table currently knows. Declaring it here, at the
// consumer, keeps this package free of any repository import.
type AssetIDLister interface {
	ListAllAssetIDs(context.Context) ([]string, error)
}

// OrphanDir is one stale per-asset cache directory: its name matches no asset
// in the assets table (asset deleted, database restored). Bytes is what
// removing it frees, measured the same way dirSize measures.
type OrphanDir struct {
	Path  string
	Bytes int64
}

// OrphanDirectories lists the top-level directories under cacheDir whose name
// matches no asset in the assets table. Recognized category subtrees —
// sources/ (staging) and scratch directories (tmp/, scratch/,
// analysis-frames/, analysis-windows/) — are never asset directories and are
// never reported as orphan candidates. A missing cache dir is no orphans.
func OrphanDirectories(ctx context.Context, repo AssetIDLister, cacheDir string) ([]OrphanDir, error) {
	known := map[string]struct{}{}
	ids, err := repo.ListAllAssetIDs(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		known[id] = struct{}{}
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var orphans []OrphanDir
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "sources" || IsScratchDirName(entry.Name()) {
			continue
		}
		if _, ok := known[entry.Name()]; ok {
			continue
		}
		path := filepath.Join(cacheDir, entry.Name())
		bytes, err := dirSize(path)
		if err != nil {
			bytes = 0
		}
		orphans = append(orphans, OrphanDir{Path: path, Bytes: bytes})
	}
	return orphans, nil
}
