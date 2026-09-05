package app

import (
	"context"
	"errors"
	"os"

	"github.com/evjohn-icu/nexusgate/internal/cache"
)

// StorageOverview is the settings page's read-only disk picture. Every number
// is a byte count with a defined source; none of them touch original media —
// the one estimate (OriginalEstimateBytes) comes from the scanner's ingestion
// records, and the rest describe Hub-owned storage (cache dir, database file,
// free disk). It carries no filenames or paths, which is what makes it a
// trusted read rather than an administrative one.
type StorageOverview struct {
	// OriginalEstimateBytes is the SUM of assets.file_size — what the scanner
	// saw when it fingerprinted each file, not a walk of the roots.
	OriginalEstimateBytes int64 `json:"original_estimate_bytes"`
	// DerivedBytes is the rebuildable derive cache: thumbnails + proxies +
	// audio extracts. All of it can be recreated from original media.
	DerivedBytes int64 `json:"derived_bytes"`
	// ThumbnailBytes / ProxyBytes / AudioBytes are the classification of the
	// per-asset cache directories (thumbnail-<mode>.jpg / proxy-<mode>.mp4 /
	// audio.m4a). The split exists so the page can show what a clean-up would
	// actually remove.
	ThumbnailBytes int64 `json:"thumbnail_bytes"`
	ProxyBytes     int64 `json:"proxy_bytes"`
	AudioBytes     int64 `json:"audio_bytes"`
	// SourceStagingBytes is cache/sources/ — the copy-mode staging area that
	// mirrors NAS originals locally so every derive does not re-read them over
	// the network. It is a copy, so removing it loses nothing but the speed.
	SourceStagingBytes int64 `json:"source_staging_bytes"`
	// ScratchBytes is transient analysis scratch (analysis-frames/,
	// analysis-windows/, tmp/), which the pipeline is supposed to remove
	// itself; anything still here is litter.
	ScratchBytes int64 `json:"scratch_bytes"`
	// DatabaseBytes is the size of the SQLite file (nexusgate.db).
	DatabaseBytes int64 `json:"database_bytes"`
	// TemporaryBytes is everything transient: scratch plus unclassified files
	// (in-progress uploads, stray writes). Safe to delete at any time.
	TemporaryBytes int64 `json:"temporary_bytes"`
	// FreeDiskBytes is free space on the filesystem holding the cache dir.
	// Zero (never an error) on Windows, where the probe is unsupported.
	FreeDiskBytes int64 `json:"free_disk_bytes"`
	// RebuildableBytes is what the operator can safely release: derived
	// artifacts and scratch, which a pipeline re-run regenerates. Sources are
	// excluded because staging copies are what derives read until eviction.
	RebuildableBytes int64 `json:"rebuildable_bytes"`
}

// StorageOverview aggregates the storage picture. The cache classification is
// internal/cache.Inspect's (J1a): the CLI's `cache inspect` and this overview
// count the same bytes by the same rules, and the settings page must never
// contradict what the CLI reports. A fresh install has no cache dir yet; that
// is "nothing cached", not an error.
func (s *Service) StorageOverview(ctx context.Context) (StorageOverview, error) {
	var overview StorageOverview
	total, err := s.repo.TotalSourceBytes(ctx)
	if err != nil {
		return overview, err
	}
	overview.OriginalEstimateBytes = total

	stats, err := cache.Inspect(s.cfg.CacheDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return overview, err
	}
	overview.ThumbnailBytes = stats.ThumbnailBytes
	overview.ProxyBytes = stats.ProxyBytes
	overview.AudioBytes = stats.AudioBytes
	overview.DerivedBytes = stats.ThumbnailBytes + stats.ProxyBytes + stats.AudioBytes
	overview.SourceStagingBytes = stats.SourceStagingBytes
	overview.ScratchBytes = stats.ScratchBytes
	// Scratch and unclassified leftovers are all transient; the page collapses
	// them into one 临时文件 row rather than pretending the walk can name what
	// it cannot classify.
	overview.TemporaryBytes = stats.ScratchBytes + stats.OrphanBytes
	overview.RebuildableBytes = stats.RebuildableBytes

	if info, err := os.Stat(s.cfg.DatabasePath); err == nil && info.Mode().IsRegular() {
		overview.DatabaseBytes = info.Size()
	}
	// A probe failure (cache dir absent on a fresh install, Windows) leaves
	// FreeDiskBytes at zero; the page shows the row as unknown rather than
	// inventing a number.
	if free, err := freeBytes(s.cfg.CacheDir); err == nil {
		overview.FreeDiskBytes = free
	}
	return overview, nil
}
