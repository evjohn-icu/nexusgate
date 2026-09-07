package ingest

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

type ScanRepository interface {
	UpsertScannedFile(ctx context.Context, root domain.LibraryRoot, relativePath, absolutePath string, info fs.FileInfo, fingerprint string) (domain.ScannedFile, error)
	// KnownFile returns what the previous scan recorded for this path, and
	// whether there was one. It is declared here, at the consumer, for the
	// same reason PipelineRepository is declared in internal/app: the scanner
	// names the one question it asks, and the sqlite package answers it.
	KnownFile(ctx context.Context, rootID, relativePath string) (domain.KnownFile, bool, error)
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
	return s.scan(ctx, root, false)
}

// ScanDeep is Scan with the fingerprint cache turned off: every file is read
// and re-fingerprinted even when its size and mtime are unchanged. It exists
// because the cache trusts mtime, and there is exactly one situation mtime
// cannot describe — content rewritten in place with the timestamp restored,
// which a restore-from-backup or an rsync --times can produce. That is rare
// enough not to pay 12 MiB per file per scan for, and real enough to need a
// way out. There is deliberately no browser control and no config setting: it
// is an operator recovery action (`nexusgate root scan <id> --deep`), not a
// mode a library can be left in.
func (s *Scanner) ScanDeep(ctx context.Context, root domain.LibraryRoot) (domain.ScanResult, error) {
	return s.scan(ctx, root, true)
}

func (s *Scanner) scan(ctx context.Context, root domain.LibraryRoot, deep bool) (domain.ScanResult, error) {
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
		var fingerprint string
		cached := false
		if !deep {
			fingerprint, cached = s.cachedFingerprint(ctx, root.ID, relative, info)
		}
		if !cached {
			fingerprint, err = QuickFingerprint(path, info.Size())
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("fingerprint file: %s: %v", path, err))
				return nil
			}
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

// cachedFingerprint returns the identity a previous scan computed for this
// path, and whether it may be used: true only when the file on disk is still,
// by size and mtime, the file that identity was computed from. Every other
// answer — no row, a stored identity that is empty, a lookup that failed — is
// false, and false costs only the read it would have cost anyway.
//
// The hit is reported separately rather than by returning "" because the empty
// string is a value the cache can legitimately hold (an assets row written
// before this cache existed, or a corrupted one), and handing it on as an
// identity would collapse every such file onto a single asset row.
//
// Both facts must match. Size alone is far too weak for footage, where a
// re-render of the same timeline is routinely byte-identical in length; mtime
// alone would trust a filesystem that reports it with second granularity over
// a copy that finished within the same second.
//
// A location marked exists_now=0 is still trusted. A file that went missing
// and came back with the same size and the same mtime is the same bytes: the
// mtime is the filesystem's own statement that nothing wrote to it, and the
// disappearance was the mount, not the file.
//
// A lookup error is swallowed on purpose rather than recorded in
// result.Errors. This is a cache read, and a cache read that fails must not
// be able to mark a scan incomplete — which is what an entry in result.Errors
// does, since it blocks the reconciliation gate in internal/app. If the
// database is genuinely broken, the UpsertScannedFile call a few lines below
// fails too and reports itself.
func (s *Scanner) cachedFingerprint(ctx context.Context, rootID, relativePath string, info fs.FileInfo) (string, bool) {
	known, ok, err := s.repo.KnownFile(ctx, rootID, relativePath)
	if err != nil || !ok {
		return "", false
	}
	if known.Fingerprint == "" {
		return "", false
	}
	if known.Size != info.Size() || known.ModifiedNS != info.ModTime().UnixNano() {
		return "", false
	}
	return known.Fingerprint, true
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
