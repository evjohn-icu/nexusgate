package ingest

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/domain"
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

func (s *Scanner) Scan(ctx context.Context, root domain.LibraryRoot) (domain.ScanResult, error) {
	result := domain.ScanResult{}
	seen := make([]string, 0, 1024)
	// The same asset can be reached through two paths in one walk (its
	// fingerprint matches twice, e.g. a copy inside the root), so deduplicate
	// while keeping first-seen order for a deterministic result.
	changedAssets := make(map[string]struct{})

	err := filepath.WalkDir(root.Path, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Errors = append(result.Errors, walkErr.Error())
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !supportedVideo(path) {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("stat %s: %v", path, err))
			return nil
		}
		relative, err := filepath.Rel(root.Path, path)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("relative path %s: %v", path, err))
			return nil
		}
		relative = filepath.ToSlash(relative)
		seen = append(seen, relative)

		fingerprint, err := QuickFingerprint(path, info.Size())
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("fingerprint %s: %v", path, err))
			return nil
		}

		scanned, err := s.repo.UpsertScannedFile(ctx, root, relative, path, info, fingerprint)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("persist %s: %v", path, err))
			return nil
		}
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
	return result, nil
}

func supportedVideo(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mov", ".mp4", ".m4v", ".mxf", // common camera containers
		".braw", ".r3d", ".ari", ".crm", ".dng", ".nev", // camera RAW sources
		".insv": // Insta360 source media
		return true
	default:
		return false
	}
}
