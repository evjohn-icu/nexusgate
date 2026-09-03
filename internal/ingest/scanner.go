package ingest

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

type ScanRepository interface {
	UpsertScannedFile(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error)
}

type Scanner struct {
	repo ScanRepository
}

func NewScanner(repo ScanRepository) *Scanner {
	return &Scanner{repo: repo}
}

// Scan walks a library root and reports what it found. Every regular non-video
// file is skipped and counted (bounded to the first 20 distinct extension
// types) so an operator can see why a root produced no footage. The seen list
// is handed back, never applied: reconciliation waits for the app-layer
// root-health gate.
func (s *Scanner) Scan(ctx context.Context, root domain.LibraryRoot) (domain.ScanResult, error) {
	result := domain.ScanResult{}
	result.SupportedExtensions = supportedVideoExtensions
	seen := make([]string, 0, 1024)
	// The same asset can be reached through two paths in one walk (its
	// fingerprint matches twice, e.g. a copy inside the root), so deduplicate
	// while keeping first-seen order for a deterministic result.
	changedAssets := make(map[string]struct{})
	// Skipped-file census: at most maxSkippedExtensionTypes distinct extension
	// types are named; every further distinct type folds into SkippedOther so
	// a hostile directory cannot grow an unbounded map.
	skipped := skippedCensus{seen: make(map[string]struct{}, maxSkippedExtensionTypes), folded: make(map[string]struct{}, maxSkippedExtensionTypes)}

	err := filepath.WalkDir(root.Path, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root.Path {
				result.Errors = append(result.Errors, fmt.Sprintf("walk root: %v", walkErr))
			} else if supportedVideo(path) {
				result.Errors = append(result.Errors, fmt.Sprintf("stat file: %s: %v", path, walkErr))
			} else {
				result.Errors = append(result.Errors, fmt.Sprintf("walk entry: %s: %v", path, walkErr))
			}
			return nil
		}
		if path == root.Path {
			result.RootReachable = true
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !supportedVideo(path) {
			// Only regular files are counted: a symlink or socket in a root is
			// not footage but is also not something the operator can convert.
			if entry.Type().IsRegular() {
				skipped.count(path, &result)
			}
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("stat file: %s: %v", path, err))
			return nil
		}
		relative, err := filepath.Rel(root.Path, path)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("relative path %s: %v", path, err))
			return nil
		}
		relative = filepath.ToSlash(relative)
		fingerprint, err := QuickFingerprint(path, info.Size())
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("fingerprint file: %s: %v", path, err))
			return nil
		}

		scanned, err := s.repo.UpsertScannedFile(ctx, root, relative, path, info, fingerprint)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("persist file: %s: %v", path, err))
			return nil
		}
		seen = append(seen, relative)
		if scanned.Created {
			result.Discovered++
		} else {
			result.Linked++
		}
		if scanned.Changed {
			if _, already := changedAssets[scanned.AssetID]; !already {
				changedAssets[scanned.AssetID] = struct{}{}
				result.ChangedAssetIDs = append(result.ChangedAssetIDs, scanned.AssetID)
			}
		}
		return nil
	})
	if err != nil {
		return result, err
	}

	// The walk's seen list is handed back rather than applied here: marking
	// unseen files missing must not happen until the app-layer gate has
	// decided the root was actually reachable, or an unmounted NAS root would
	// have every asset in it marked missing by the walk of an empty
	// directory. The scanner reports; the service reconciles.
	result.SeenRelativePaths = seen
	result.SkippedExtensions = skipped.extensions
	result.SkippedOther = skipped.otherTypes
	result.Complete = result.RootReachable && len(result.Errors) == 0
	return result, nil
}

// supportedVideoExtensions is the single declaration-ordered list of source
// extensions the scanner recognizes. A file whose extension is not here is
// skipped (and counted) rather than treated as footage. The order is what a
// scan report renders as the supported list, so keep it stable.
var supportedVideoExtensions = []string{
	".mov", ".mp4", ".m4v", ".mxf", // common camera containers
	".braw", ".r3d", ".ari", ".crm", ".dng", ".nev", // camera RAW sources
	".insv", // Insta360 source media
}

// maxSkippedExtensionTypes bounds the skipped-file extension census: the first
// 20 distinct normalized extension types are named in the report, every
// further distinct type folds into SkippedOther, so a hostile directory with
// thousands of invented extensions cannot grow an unbounded map.
const maxSkippedExtensionTypes = 20

// skippedCensus accumulates the skipped non-video file totals without ever
// growing past the bounded sets of named and folded extension types.
type skippedCensus struct {
	extensions []string
	seen       map[string]struct{}
	// folded tracks the distinct extension types beyond the named set so
	// SkippedOther counts distinct types, not files — a hostile directory
	// cannot grow it past maxSkippedExtensionTypes entries.
	folded     map[string]struct{}
	otherTypes int
}

// count records one skipped regular file. The first maxSkippedExtensionTypes
// distinct normalized extensions are named; every further distinct extension
// type folds into otherTypes exactly once, so the report says how many further
// types the walk ignored, never how many files share them.
func (c *skippedCensus) count(path string, result *domain.ScanResult) {
	result.SkippedFiles++
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return
	}
	if _, known := c.seen[ext]; known {
		return
	}
	if len(c.extensions) < maxSkippedExtensionTypes {
		c.seen[ext] = struct{}{}
		c.extensions = append(c.extensions, ext)
		return
	}
	if _, folded := c.folded[ext]; folded {
		return
	}
	if len(c.folded) < maxSkippedExtensionTypes {
		c.folded[ext] = struct{}{}
		c.otherTypes++
	}
}

func supportedVideo(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, supported := range supportedVideoExtensions {
		if ext == supported {
			return true
		}
	}
	return false
}
