// Package staging keeps an optional, disposable local copy of a source file
// before media processing. It is designed for mounted NAS folders: source
// files remain read-only and all processing happens on the local machine.
package staging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Mode string

const (
	ModeNone Mode = "none"
	ModeCopy Mode = "copy"
)

type SourceStager struct {
	mode     Mode
	cacheDir string
	maxBytes int64
	// remove and walk are the two filesystem calls eviction makes, injectable
	// so their failure paths can be tested. Permissions cannot stand in for
	// them: CAP_DAC_OVERRIDE in an ambient capability set — a container
	// default — lets an ordinary process delete inside a directory it has no
	// write bit for, so a permissions-based fixture proves nothing wherever
	// that is set. internal/media's probeEncoderFunc is the same pattern.
	remove func(string) error
	walk   func(string, filepath.WalkFunc) error
}

func New(mode, cacheDir string) (*SourceStager, error) {
	return NewCapped(mode, cacheDir, 0)
}

func NewCapped(mode, cacheDir string, maxBytes int64) (*SourceStager, error) {
	selected := Mode(strings.TrimSpace(strings.ToLower(mode)))
	if selected == "" {
		selected = ModeNone
	}
	if selected != ModeNone && selected != ModeCopy {
		return nil, fmt.Errorf("unsupported source staging mode %q", mode)
	}
	if selected == ModeCopy && strings.TrimSpace(cacheDir) == "" {
		return nil, fmt.Errorf("source staging cache directory is required for copy mode")
	}
	return &SourceStager{mode: selected, cacheDir: cacheDir, maxBytes: maxBytes, remove: os.Remove, walk: filepath.Walk}, nil
}

// Stage returns sourcePath unchanged in none mode. Copy mode writes a local,
// immutable-by-convention cache entry scoped by assetID and inputVersion. The
// source itself is only opened for reading.
func (s *SourceStager) Stage(ctx context.Context, sourcePath, assetID, inputVersion string) (string, error) {
	if s == nil || s.mode == ModeNone {
		return sourcePath, nil
	}
	if err := validateSegment("asset ID", assetID); err != nil {
		return "", err
	}
	if err := validateSegment("input version", inputVersion); err != nil {
		return "", err
	}
	ext := filepath.Ext(sourcePath)
	if ext == "" {
		ext = ".media"
	}
	destination := filepath.Join(s.cacheDir, "sources", assetID, inputVersion+ext)
	if cached, err := os.Lstat(destination); err == nil {
		if cached.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing symlink source cache destination: %s", destination)
		}
		if cached.Mode().IsRegular() {
			now := time.Now()
			_ = os.Chtimes(destination, now, now)
			return destination, nil
		}
		return "", fmt.Errorf("source cache destination is not a regular file: %s", destination)
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		return "", fmt.Errorf("stat source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("source is not a regular file: %s", sourcePath)
	}
	s.evictForIncoming(info.Size(), assetID)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", fmt.Errorf("create source cache directory: %w", err)
	}

	source, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("open source: %w", err)
	}
	defer source.Close()
	temporary, err := os.CreateTemp(filepath.Dir(destination), "."+inputVersion+"-*.partial")
	if err != nil {
		return "", fmt.Errorf("create staged source: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := copyWithContext(ctx, temporary, source); err != nil {
		temporary.Close()
		return "", fmt.Errorf("stage source locally: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", fmt.Errorf("sync staged source: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close staged source: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return "", fmt.Errorf("publish staged source: %w", err)
	}
	return destination, nil
}

// evictForIncoming makes room under maxBytes for a file of incoming bytes
// about to be staged. It returns nothing on purpose: every outcome here is
// advisory. This is a cache of files that can all be re-copied from the
// original, so a walk that cannot read a directory, or a delete another
// process wins, must leave the staging itself untouched — turning a
// reclaim problem into a failed job would be strictly worse than running one
// file over the cap.
func (s *SourceStager) evictForIncoming(incoming int64, keepAsset string) {
	if s.maxBytes <= 0 {
		return
	}

	sourcesDir := filepath.Join(s.cacheDir, "sources")
	entries := make([]sourceCacheEntry, 0)
	var total int64
	// There is deliberately no live-job check: a derive can run for minutes
	// across several ffmpeg invocations, while every cache hit refreshes mtime
	// and makes its staged source newest, so it is evicted last. If that source
	// nevertheless disappears, ENOENT is retryable here; nothing marks it
	// domain.Permanent, so the losing job re-stages it on its next attempt.
	walk := s.walk
	if walk == nil {
		walk = filepath.Walk
	}
	err := walk(sourcesDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		base := filepath.Base(path)
		// This leaves a known gap: an orphan .partial from a crashed copy can
		// never be reclaimed, because this walk skips it and cache gc skips the
		// whole sources tree. Do not "fix" this by deleting partial files here.
		if strings.HasPrefix(base, ".") || strings.HasSuffix(base, ".partial") {
			return nil
		}
		relative, relErr := filepath.Rel(sourcesDir, path)
		if relErr != nil {
			return relErr
		}
		assetID := strings.Split(relative, string(filepath.Separator))[0]
		entries = append(entries, sourceCacheEntry{path: path, assetID: assetID, size: info.Size(), modtime: info.ModTime()})
		total += info.Size()
		return nil
	})
	if err != nil {
		// A sources directory that does not exist yet is the ordinary state of
		// a fresh cache, not a problem worth a log line. Anything else is
		// reported and then dropped: see this function's doc for why it cannot
		// propagate.
		if !os.IsNotExist(err) {
			slog.Warn("source cache walk failed; skipping eviction for this stage", "cache_dir", sourcesDir, "error", err)
		}
		return
	}
	if !cacheUsageExceeds(total, incoming, s.maxBytes) {
		return
	}

	// The trigger and target intentionally use different numbers: the cap
	// decides when to evict, while the 90% target provides hysteresis so one
	// file is not evicted on every Stage near the configured limit.
	target := s.maxBytes * 9 / 10
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].modtime.Equal(entries[j].modtime) {
			return entries[i].path < entries[j].path
		}
		return entries[i].modtime.Before(entries[j].modtime)
	})
	remove := s.remove
	if remove == nil {
		remove = os.Remove
	}
	for _, entry := range entries {
		if entry.assetID == keepAsset {
			continue
		}
		if !cacheUsageExceeds(total, incoming, target) {
			return
		}
		if err := remove(entry.path); err != nil {
			continue
		}
		total -= entry.size
		_ = remove(filepath.Dir(entry.path))
	}
	if cacheUsageExceeds(total, incoming, s.maxBytes) {
		slog.Warn("source cache remains over capacity; incoming source will proceed", "incoming_bytes", incoming, "max_bytes", s.maxBytes)
	}
}

type sourceCacheEntry struct {
	path    string
	assetID string
	size    int64
	modtime time.Time
}

// cacheUsageExceeds is the one comparison this file makes, written once so
// the trigger and the target cannot drift apart: they differ only in `limit`.
// int64 is not at risk here — overflowing it would take an exabyte of cache.
func cacheUsageExceeds(total, incoming, limit int64) bool {
	return total+incoming > limit
}

func validateSegment(label, value string) error {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value || strings.ContainsRune(value, filepath.Separator) {
		return fmt.Errorf("invalid source staging %s %q", label, value)
	}
	return nil
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) error {
	buffer := make([]byte, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			if _, err := destination.Write(buffer[:count]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
