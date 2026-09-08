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
			// This is the whole LRU signal. Rewriting the mtime here turns it
			// from "when this was copied" into "when a job last asked for it",
			// which is what evictForIncoming sorts on — and it is why the file
			// a long derive is working through is the newest in the cache, and
			// so the last one evicted.
			//
			// The error is discarded rather than handled, and there is no seam
			// to test that with, because there is no branch: a stage must never
			// fail over a timestamp. The cost of a failed touch is that this
			// entry looks older than it is and gets evicted early, which costs
			// exactly one re-copy of a file that was always re-copyable.
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
	// This is the only code that reclaims completed entries under sources/ —
	// internal/cache's gc and inspect skip the tree entirely. Stage's own
	// deferred os.Remove also deletes there, but only the temp file it just
	// created, which is a different job from reclaiming.
	//
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
		// See IsTemporaryStageName. Both conditions, not either: Stage's temp file is created as
		// os.CreateTemp(dir, "."+inputVersion+"-*.partial"), so it always
		// carries the dot AND the suffix; requiring both is exactly that name
		// and nothing else. Either-condition was wider than the thing it
		// describes: validateSegment permits an inputVersion beginning with a
		// dot, and the destination's extension comes from the source file, so
		// a real completed entry could match one half and become permanently
		// invisible to eviction — a cache entry no cap can ever reclaim.
		//
		// This leaves a known gap: an orphan .partial from a crashed copy can
		// never be reclaimed, because this walk skips it and cache gc skips the
		// whole sources tree. Do not "fix" this by deleting partial files here.
		if IsTemporaryStageName(base) {
			return nil
		}
		relative, relErr := filepath.Rel(sourcesDir, path)
		if relErr != nil {
			return relErr
		}
		// A regular file sitting directly under sources/ belongs to no asset:
		// Stage only ever writes sources/<assetID>/<file>. Taking the first
		// path segment as its asset id would hand it whatever name it happens
		// to carry, and a stray literally named after the asset being staged
		// would inherit that asset's protection — leaving the cache over cap
		// and, worse, leaving a file where Stage needs to create a directory.
		// An empty id matches no keepAsset, so it is always evictable.
		assetID := ""
		if segments := strings.Split(relative, string(filepath.Separator)); len(segments) > 1 {
			assetID = segments[0]
		}
		entries = append(entries, sourceCacheEntry{path: path, assetID: assetID, size: info.Size(), modtime: info.ModTime()})
		// Summed as int64, so a tree whose files REPORT more than 8 EiB
		// between them would wrap and read as under the cap. That is stated
		// rather than coded around: Stage copies byte for byte, so an entry it
		// wrote is exactly as large as its source, and reaching the wrap needs
		// sparse files placed here by something other than Stage — which is
		// already write access to a directory it could simply empty instead.
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
	target := evictionTarget(s.maxBytes)
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
		// Prune the asset directory once its last entry is gone. No emptiness
		// check is needed: os.Remove declines a non-empty directory. Nor is a
		// guard against aiming this at sources/ itself, which a stray file
		// directly beneath it does — sources/ can only be removed when it is
		// already empty, and the MkdirAll a few lines later in Stage recreates
		// it. Checked rather than assumed: a guard here provably cannot change
		// any outcome, so it would be a line nobody could ever test.
		_ = remove(filepath.Dir(entry.path))
	}
	if cacheUsageExceeds(total, incoming, s.maxBytes) {
		slog.Warn("source cache remains over capacity; incoming source will proceed", "asset_id", keepAsset, "incoming_bytes", incoming, "max_bytes", s.maxBytes)
	}
}

type sourceCacheEntry struct {
	path    string
	assetID string
	size    int64
	modtime time.Time
}

// IsTemporaryStageName reports whether a base file name under cache/sources is
// a copy in flight rather than a finished cache entry. It is exported because
// two packages have to agree on it and disagreeing is a bug that shows as a
// number: internal/cache's inspect counts these toward the source-staging
// total, while eviction does not, so an operator reading "48 GiB / 50 GiB"
// would be looking at a total the cap is not measured against.
//
// The shape is exactly what Stage creates —
// os.CreateTemp(dir, "."+inputVersion+"-*.partial") — so both halves are
// required. Either half alone is wider than the thing it names: an
// inputVersion may begin with a dot and a destination's extension comes from
// the source file, so a finished entry could match one half and become a cache
// entry no cap could ever reclaim.
func IsTemporaryStageName(base string) bool {
	return strings.HasPrefix(base, ".") && strings.HasSuffix(base, ".partial")
}

// evictionTarget is 90% of cap, and exists as a named function because it
// cannot be tested through Stage: tripping the trigger at a cap large enough
// to expose the bug would need a fixture holding exabytes.
//
// The arithmetic looks roundabout because `cap * 9 / 10` wraps for a cap above
// roughly one exabyte, and a wrapped target is not slightly wrong — at 2^62 it
// comes out around a tenth of the cap, so eviction would empty a cache it was
// asked to trim by a tenth. Dividing first cannot wrap; the remainder term is
// the precision dividing first would otherwise round away.
func evictionTarget(cap int64) int64 {
	return cap/10*9 + (cap%10)*9/10
}

// cacheUsageExceeds is the one comparison this file makes, written once so the
// trigger and the target cannot drift apart: they differ only in `limit`.
//
// It is deliberately not the obvious `total+incoming > limit`. `total` is a
// sum of sizes read off the filesystem, and a sparse file reports a size it
// does not occupy, so the sum is attacker-influenced in a way a cap typed by
// an operator is not. Adding first can wrap to a negative and read as "under
// the cap" — eviction would then return with the cache unbounded, which is the
// one outcome this function exists to prevent. Subtracting cannot underflow
// once incoming <= limit, and the branch above covers the rest.
func cacheUsageExceeds(total, incoming, limit int64) bool {
	if incoming > limit {
		return true
	}
	return total > limit-incoming
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
