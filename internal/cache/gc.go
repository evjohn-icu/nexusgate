package cache

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// reportLimit caps the paths carried in GCResult.Removed and
// VerifyResult.MissingPaths/OrphanPaths: the CLI report prints the first few,
// never the full inventory.
const reportLimit = 20

// GCOptions selects what GC may delete. The safe default is an all-empty
// options struct: nothing is deleted. Every category is opt-in, DryRun is
// on-by-default at the CLI layer, and the cache dir itself is never given
// paths outside it — the database, config and provider-secrets live outside
// cacheDir and are unreachable by construction, and cache/sources/ is skipped
// by the walk itself.
type GCOptions struct {
	// DryRun counts what would be freed without deleting anything.
	DryRun bool
	// RemoveScratch deletes transient analysis scratch directories
	// (analysis-frames/, analysis-windows/, tmp/, scratch/ — see
	// IsScratchDirName), which the pipeline is supposed to remove itself.
	RemoveScratch bool
	// RemoveRebuildable deletes thumbnail-*.jpg / proxy-*.mp4 / audio.m4a
	// files (see IsRebuildableArtifactName). Their derived_artifacts rows
	// survive; the CLI re-enqueues a JobDerive per affected committed asset
	// after a real run (see cmd/nexusslate/cache.go), and the derive stage
	// re-creates the files while skipping the paid chain.
	RemoveRebuildable bool
	// RemoveOrphans deletes exactly these per-asset directories (as returned
	// by OrphanDirectories). An asset directory is only an orphan when its
	// assets row is gone; a listed path outside cacheDir is an error, never a
	// target.
	RemoveOrphans []OrphanDir
	// ProtectedAssetIDs are assets with active pipeline leases. Their artifacts
	// remain available to the running job.
	ProtectedAssetIDs map[string]struct{}
	MinAge            time.Duration
	Now               time.Time
}

// GCResult reports what GC removed (or, in a dry run, would remove).
type GCResult struct {
	RemovedFiles int
	FreedBytes   int64
	// Removed holds the first reportLimit removed paths, for the report.
	Removed                    []string
	RemovedRebuildableAssetIDs []string
	RemovedTruncated           bool
	SkippedActiveFiles         int
	SkippedActiveBytes         int64
	SkippedYoungFiles          int
	SkippedYoungBytes          int64
	// Errors holds non-fatal failures. GC deletes file by file and never
	// aborts mid-way: an unremovable entry is recorded and the rest of the
	// walk continues, so the report reflects the partial result.
	Errors []string
}

// GC walks cacheDir and deletes exactly what GCOptions selects. The walk
// never descends into cache/sources/ (copy-mode staging is canonical until
// eviction), and every other entry is classified by the same patterns
// Inspect uses, so a dry run's accounting is what a real run frees.
func GC(cacheDir string, opts GCOptions) (GCResult, error) {
	var result GCResult
	if opts.MinAge <= 0 {
		opts.MinAge = 5 * time.Minute
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}

	// A missing cache dir (fresh install, no cache volume yet) is an empty
	// success, matching Inspect: `cache gc` must run before the first scan.
	if _, err := os.Stat(cacheDir); err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, err
	}

	orphans := make(map[string]struct{}, len(opts.RemoveOrphans))
	for _, orphan := range opts.RemoveOrphans {
		orphanPath := orphan.Path
		if !filepath.IsAbs(orphanPath) {
			orphanPath = filepath.Join(cacheDir, orphanPath)
		}
		orphanPath = filepath.Clean(orphanPath)
		// Safety boundary, not convenience: an orphan directory is a
		// per-asset cache directory, and nothing outside cacheDir is ever
		// eligible for deletion through this command.
		rel, err := filepath.Rel(cacheDir, orphanPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || IsSourceStagingPath(filepath.ToSlash(rel)) {
			return result, fmt.Errorf("orphan path %q is outside the cache directory", orphanPath)
		}
		orphans[orphanPath] = struct{}{}
	}

	err := filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("walk %s: %v", path, err))
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "sources" {
				// Copy-mode staging is canonical until eviction; the walk
				// must not even count it as deletable.
				return filepath.SkipDir
			}
			if IsScratchDirName(name) && opts.RemoveScratch {
				if shouldSkipDirectory(&result, cacheDir, path, opts) {
					return filepath.SkipDir
				}
				return removeDir(&result, cacheDir, path, opts.DryRun)
			}
			if _, ok := orphans[filepath.Clean(path)]; ok {
				if shouldSkipDirectory(&result, cacheDir, path, opts) {
					return filepath.SkipDir
				}
				return removeDir(&result, cacheDir, path, opts.DryRun)
			}
			return nil
		}
		if opts.RemoveRebuildable && IsRebuildableArtifactName(d.Name()) {
			assetID, reason := ClassifyRebuildableArtifact(cacheDir, path, opts.ProtectedAssetIDs, opts.MinAge, opts.Now)
			if reason != ArtifactEligible {
				info, infoErr := d.Info()
				if infoErr == nil {
					switch reason {
					case ArtifactActive:
						result.SkippedActiveFiles++
						result.SkippedActiveBytes += info.Size()
					case ArtifactYoung:
						result.SkippedYoungFiles++
						result.SkippedYoungBytes += info.Size()
					}
				}
				return nil
			}
			return removeFile(&result, cacheDir, path, d, assetID, opts.DryRun)
		}
		return nil
	})
	return result, err
}

func shouldSkipDirectory(result *GCResult, cacheDir, path string, opts GCOptions) bool {
	rel, err := filepath.Rel(cacheDir, path)
	if err != nil {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	assetID := ""
	if len(parts) > 1 && !IsScratchDirName(parts[0]) && parts[0] != "sources" {
		assetID = parts[0]
	}
	if _, ok := opts.ProtectedAssetIDs[assetID]; assetID != "" && ok {
		size, _ := dirSize(path)
		result.SkippedActiveFiles++
		result.SkippedActiveBytes += size
		return true
	}
	info, err := os.Stat(path)
	if err == nil && opts.Now.Sub(info.ModTime()) < opts.MinAge {
		size, _ := dirSize(path)
		result.SkippedYoungFiles++
		result.SkippedYoungBytes += size
		return true
	}
	return false
}

// removeDir deletes (or, in a dry run, sizes) a whole directory and prevents
// the walk from descending into it, so its children are neither double-counted
// nor visited after removal. Size is measured before deletion so FreedBytes
// stays accurate in both modes.
func removeDir(result *GCResult, cacheDir, path string, dryRun bool) error {
	size, err := dirSize(path)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("size %s: %v", path, err))
		return nil
	}
	if !dryRun {
		if err := os.RemoveAll(path); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("remove %s: %v", path, err))
			return nil
		}
	}
	recordRemoved(result, cacheDir, path, size, "")
	return filepath.SkipDir
}

// removeFile deletes one file after stat'ing its size, so the dry-run and
// real-run accounting agree.
func removeFile(result *GCResult, cacheDir, path string, d fs.DirEntry, assetID string, dryRun bool) error {
	size := int64(0)
	if info, err := d.Info(); err == nil {
		size = info.Size()
	}
	if !dryRun {
		if err := os.Remove(path); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("remove %s: %v", path, err))
			return nil
		}
	}
	recordRemoved(result, cacheDir, path, size, assetID)
	return nil
}

func recordRemoved(result *GCResult, cacheDir, path string, size int64, assetID string) {
	result.RemovedFiles++
	result.FreedBytes += size
	if len(result.Removed) < reportLimit {
		if rel, err := filepath.Rel(cacheDir, path); err == nil {
			result.Removed = append(result.Removed, rel)
		} else {
			result.Removed = append(result.Removed, path)
		}
	} else {
		result.RemovedTruncated = true
	}
	if assetID != "" {
		for _, existing := range result.RemovedRebuildableAssetIDs {
			if existing == assetID {
				return
			}
		}
		result.RemovedRebuildableAssetIDs = append(result.RemovedRebuildableAssetIDs, assetID)
	}
}

type ArtifactSkipReason uint8

const (
	ArtifactEligible ArtifactSkipReason = iota
	ArtifactActive
	ArtifactYoung
	ArtifactInvalid
)

// ClassifyRebuildableArtifact applies the safety gates before deletion and
// returns the owning top-level asset directory when eligible.
func ClassifyRebuildableArtifact(cacheDir, artifactPath string, protected map[string]struct{}, minAge time.Duration, now time.Time) (string, ArtifactSkipReason) {
	rel, err := filepath.Rel(cacheDir, artifactPath)
	if err != nil || filepath.Base(filepath.Dir(rel)) == "." || strings.Contains(filepath.ToSlash(rel), "/") && strings.Count(filepath.ToSlash(rel), "/") != 1 {
		return "", ArtifactInvalid
	}
	assetID := filepath.Base(filepath.Dir(rel))
	if _, ok := protected[assetID]; ok {
		return assetID, ArtifactActive
	}
	info, err := os.Stat(artifactPath)
	if err != nil || now.Sub(info.ModTime()) < minAge {
		return assetID, ArtifactYoung
	}
	return assetID, ArtifactEligible
}

// dirSize sums the sizes of the regular files under path. It follows no
// symlinks, so a scratch or orphan directory that grew a symlink cannot
// redirect the accounting outside the cache dir. Unreadable entries are
// counted as their own error and skipped.
func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// ArtifactLister is the slice of the repository Verify needs: every derived
// artifact row. Declared here, at the consumer, so this package stays free of
// any repository import.
type ArtifactLister interface {
	ListDerivedArtifacts(context.Context) ([]domain.DerivedArtifact, error)
}

// VerifyResult is the DB-row-versus-file consistency picture: rows whose file
// is missing are rebuildable gaps (the pipeline re-derives them), and files
// with no row are orphan cache content.
type VerifyResult struct {
	ArtifactRows    int
	MissingFiles    int
	MissingPaths    []string // first reportLimit
	OrphanFiles     int
	OrphanPaths     []string // first reportLimit
	TotalCacheFiles int
	TotalCacheBytes int64
}

// Verify checks every derived_artifacts row with a local_path against the
// file system, and every file under cacheDir against the row set. Rows
// pointing outside cacheDir (worker-uploaded artifacts live under
// DataDir/derived/) are still checked for existence; they simply never match
// a cache walk. cache/sources/ is excluded from orphan accounting for the
// same reason GC never touches it: the staging copy has no artifact row by
// design.
func Verify(ctx context.Context, repo ArtifactLister, cacheDir string) (VerifyResult, error) {
	var result VerifyResult
	rows, err := repo.ListDerivedArtifacts(ctx)
	if err != nil {
		return result, err
	}
	result.ArtifactRows = len(rows)

	known := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row.LocalPath == "" {
			continue
		}
		known[filepath.Clean(row.LocalPath)] = struct{}{}
		if _, err := os.Stat(row.LocalPath); err != nil {
			if os.IsNotExist(err) {
				result.MissingFiles++
				if len(result.MissingPaths) < reportLimit {
					result.MissingPaths = append(result.MissingPaths, row.LocalPath)
				}
			}
		}
	}

	stats, err := Inspect(cacheDir)
	if err != nil {
		return result, err
	}
	result.TotalCacheFiles = stats.TotalFiles
	result.TotalCacheBytes = stats.TotalBytes

	_ = filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(cacheDir, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if IsSourceStagingPath(relSlash) {
			return nil
		}
		if _, ok := known[filepath.Clean(path)]; ok {
			return nil
		}
		result.OrphanFiles++
		if len(result.OrphanPaths) < reportLimit {
			result.OrphanPaths = append(result.OrphanPaths, rel)
		}
		return nil
	})
	return result, nil
}
