package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/cache"
	"github.com/evjohn-icu/nexusgate/internal/cachecoord"
	"github.com/evjohn-icu/nexusgate/internal/config"
	sqliterepo "github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

// runCacheCommand is the cache-maintenance surface: `inspect` reports the
// artifact-class footprint, which directories no longer map to any asset in
// the database, and how much of the volume a pipeline re-run would
// regenerate. `gc` deletes (or dry-runs the deletion of) exactly the
// rebuildable, scratch and orphan content an operator names — never the
// database, provider secrets, original media or cache/sources/ staging.
// `verify` checks DB-row-versus-file consistency both ways. Inspect and gc
// consume the same cache.CacheStats classification, so the two cannot
// disagree about what exists.
func runCacheCommand(ctx context.Context, repo *sqliterepo.Repository, cfg config.Config, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: nexusgate cache inspect|gc|verify|repair-derived")
	}
	switch args[0] {
	case "inspect":
		return runCacheInspect(ctx, repo, cfg, args[1:])
	case "gc":
		return runCacheGC(ctx, repo, cfg, args[1:])
	case "verify":
		return runCacheVerify(ctx, repo, cfg, args[1:])
	case "repair-derived":
		return runCacheRepairDerived(ctx, repo, cfg, args[1:])
	default:
		return errors.New("usage: nexusgate cache inspect|gc|verify|repair-derived")
	}
}

// runCacheRepairDerived removes hardware-produced preview artifacts and their
// exact rows, and re-enqueues the software render. The ordering is the safety
// property, not ceremony: for every affected asset a durable re-derive repair
// is recorded in the queue BEFORE any file or row is deleted, and only assets
// with a durable repair are touched at all. An asset whose media root is
// offline still gets its repair recorded (the re-derive job queues with the
// `cache-rederive:` maintenance key and the lease queries hold it back until
// the root is online again), while an asset whose repair cannot be durably
// recorded keeps its files. Files are removed before their rows, so a crash
// between the two leaves a row whose missing file the derive stage
// re-creates; rows are deleted by exact local path, never by profile prefix,
// so a row whose file was never selected survives. Files belonging to assets
// with schedulable pipeline work are preserved outright.
func runCacheRepairDerived(ctx context.Context, repo *sqliterepo.Repository, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("cache repair-derived", flag.ContinueOnError)
	invalidate := flags.Bool("invalidate-hardware-profiles", false, "remove hardware-produced thumbnail/proxy files and rows")
	yes := flags.Bool("yes", false, "delete and enqueue re-derive jobs")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*invalidate {
		return errors.New("usage: nexusgate cache repair-derived --invalidate-hardware-profiles [--yes]")
	}
	prefixes := []string{"thumb-hw-", "proxy-720-hw-"}

	// The exclusive cache maintenance lock is held across the entire scan,
	// enqueue and delete: a derive publishing an artifact concurrently must
	// not see its file vanish between its SaveArtifact and its rename.
	lock, err := cachecoord.AcquireExclusive(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("acquire cache maintenance lock: %w", err)
	}
	defer lock.Release()

	matching, err := repo.MatchingDerivedArtifactsByProfilePrefixes(ctx, prefixes)
	if err != nil {
		return fmt.Errorf("find hardware artifact rows: %w", err)
	}
	protected, err := protectedAssets(ctx, repo)
	if err != nil {
		return err
	}

	// Select the manageable rows: inside the cache dir, not owned by a
	// protected asset, and keyed to a file this invocation can account for —
	// either present on disk, or already absent with the row still recorded.
	type selectedRow struct {
		path   string
		size   int64
		absent bool
	}
	selected := make([]selectedRow, 0, len(matching))
	affected := make(map[string]struct{})
	skippedProtected, skippedOutside := 0, 0
	var scanBytes int64
	for _, artifact := range matching {
		if _, ok := protected[artifact.AssetID]; ok {
			skippedProtected++
			continue
		}
		rel, relErr := filepath.Rel(cfg.CacheDir, filepath.Clean(artifact.LocalPath))
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			skippedOutside++
			continue
		}
		row := selectedRow{path: filepath.Clean(artifact.LocalPath)}
		info, statErr := os.Stat(row.path)
		switch {
		case statErr == nil && info.Mode().IsRegular():
			row.size = info.Size()
		case statErr == nil:
			// A non-regular entry at a managed destination is not touched;
			// report it and leave it to cache verify.
			skippedOutside++
			continue
		case errors.Is(statErr, fs.ErrNotExist):
			// Already absent with a known matching row: eligible for the
			// row-only half of the repair.
			row.absent = true
		default:
			// A stat failure that is not "missing" proves nothing about the
			// file; leave it and its row alone rather than delete blind.
			fmt.Printf("warning: stat %s: %v\n", row.path, statErr)
			continue
		}
		selected = append(selected, row)
		affected[artifact.AssetID] = struct{}{}
		scanBytes += row.size
	}

	if !*yes {
		fmt.Printf("would remove %d hardware-derived file(s), %s, and %d DB row(s) across %d asset(s)\n", len(selected), humanBytes(scanBytes), len(selected), len(affected))
		if skippedProtected > 0 {
			fmt.Printf("would skip %d row(s) on assets with scheduled pipeline work\n", skippedProtected)
		}
		if skippedOutside > 0 {
			fmt.Printf("would skip %d row(s) whose file is outside the managed cache\n", skippedOutside)
		}
		fmt.Println("dry run: nothing deleted; re-run with --yes to repair")
		return nil
	}

	// Durable repair first: enqueue a re-derive for every affected asset
	// before a single file or row is removed, and keep a set of the assets
	// whose repair is recorded — this invocation's or one still queued from an
	// earlier run. Errors here are per-asset partial errors; the repair task
	// for the assets that did enqueue stays queued no matter what happens
	// below.
	durable := make(map[string]struct{}, len(affected))
	for assetID := range affected {
		enqueued, enqueueErr := repo.EnqueueRederive(ctx, assetID)
		if enqueueErr != nil {
			fmt.Printf("warning: enqueue re-derive %s: %v\n", assetID, enqueueErr)
			continue
		}
		if enqueued {
			durable[assetID] = struct{}{}
			continue
		}
		stillQueued, durableErr := repo.HasDurableRederive(ctx, assetID)
		if durableErr != nil {
			fmt.Printf("warning: check queued re-derive %s: %v\n", assetID, durableErr)
			continue
		}
		if stillQueued {
			durable[assetID] = struct{}{}
		} else {
			fmt.Printf("warning: no durable repair recordable for %s; its files are preserved\n", assetID)
		}
	}

	// File first, then the exact rows: a crash between the two leaves a row
	// whose missing file the derive stage re-creates, never a file with no
	// row. Only rows whose asset has a durable repair are touched.
	rowAsset := make(map[string]string, len(selected))
	for _, artifact := range matching {
		if _, ok := durable[artifact.AssetID]; !ok {
			continue
		}
		rowAsset[filepath.Clean(artifact.LocalPath)] = artifact.AssetID
	}
	var removedFiles, removedRows int
	var freedBytes int64
	removedPaths := make([]string, 0, len(selected))
	for _, row := range selected {
		if _, ok := rowAsset[row.path]; !ok {
			continue
		}
		if !row.absent {
			if err := os.Remove(row.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Printf("warning: remove %s: %v\n", row.path, err)
				continue
			}
		}
		removedFiles++
		freedBytes += row.size
		removedPaths = append(removedPaths, row.path)
	}
	if len(removedPaths) > 0 {
		if _, err := repo.DeleteDerivedArtifactsByPaths(ctx, removedPaths); err != nil {
			return fmt.Errorf("delete hardware artifact rows: %w", err)
		}
		removedRows = len(removedPaths)
	}
	fmt.Printf("removed %d hardware-derived file(s), %s; deleted %d exact row(s); re-derive queued for %d asset(s)\n", removedFiles, humanBytes(freedBytes), removedRows, len(durable))
	if skippedProtected > 0 || skippedOutside > 0 {
		fmt.Printf("preserved: %d protected row(s), %d row(s) outside the managed cache\n", skippedProtected, skippedOutside)
	}
	return nil
}

// protectedAssets builds the protection set for a cache maintenance run:
// assets with schedulable pipeline work — nonterminal pending, running, or
// failed jobs with attempts remaining, plus running jobs holding a live lease
// even on their final attempt.
func protectedAssets(ctx context.Context, repo *sqliterepo.Repository) (map[string]struct{}, error) {
	ids, err := repo.ListCacheProtectedAssetIDs(ctx, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("list protected job assets: %w", err)
	}
	protected := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		protected[id] = struct{}{}
	}
	return protected, nil
}

func runCacheInspect(ctx context.Context, repo *sqliterepo.Repository, cfg config.Config, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: nexusgate cache inspect")
	}
	stats, err := cache.Inspect(cfg.CacheDir)
	if err != nil {
		return fmt.Errorf("inspect cache: %w", err)
	}
	orphans, err := cache.OrphanDirectories(ctx, repo, cfg.CacheDir)
	if err != nil {
		return fmt.Errorf("detect orphan cache directories: %w", err)
	}
	var orphanBytes int64
	for _, orphan := range orphans {
		orphanBytes += orphan.Bytes
	}
	var dbBytes int64
	if info, err := os.Stat(cfg.DatabasePath); err == nil {
		dbBytes = info.Size()
	}
	// The orphan row is the DB-aware number (directories with no asset row),
	// not the FS-only stray-file count from Inspect: stale artifact dirs are
	// exactly the case a name pattern cannot see.
	fmt.Printf("%-14s %-8s %6s\n", "thumbnails", countLabel(stats.ThumbnailCount, "files"), humanBytes(stats.ThumbnailBytes))
	fmt.Printf("%-14s %-8s %6s\n", "proxies", countLabel(stats.ProxyCount, "files"), humanBytes(stats.ProxyBytes))
	fmt.Printf("%-14s %-8s %6s\n", "audio", countLabel(stats.AudioCount, "files"), humanBytes(stats.AudioBytes))
	// Source staging is the one row with a budget, so it is the one row that
	// says what its budget is. Without the cap printed here an operator has no
	// way to tell "this is large" from "this is over the line and being
	// evicted", and eviction happens inside Stage where nothing else reports.
	// The used figure counts finished entries only, because that is what the
	// cap is measured against — printing a total that included copies in
	// flight would show a ratio nothing enforces.
	stagingLabel := humanBytes(stats.SourceStagingBytes)
	if cfg.SourceStaging.MaxBytes > 0 {
		stagingLabel += " / " + humanBytes(cfg.SourceStaging.MaxBytes)
	} else {
		stagingLabel += " / unbounded"
	}
	fmt.Printf("%-14s %-8s %6s\n", "source staging", countLabel(stats.SourceStagingCount, "files"), stagingLabel)
	// Only when there are any. A copy in flight is normal and transient; a
	// count that persists across runs is a crashed copy's leftovers, which
	// nothing reclaims — eviction skips this shape by name and cache gc skips
	// the whole tree. Saying so here is the only place an operator can find it.
	if stats.SourceStagingTemporaryCount > 0 {
		fmt.Printf("%-14s %-8s %6s\n", "  in flight", countLabel(stats.SourceStagingTemporaryCount, "files"), humanBytes(stats.SourceStagingTemporaryBytes))
	}
	fmt.Printf("%-14s %-8s %6s\n", "scratch", countLabel(stats.ScratchCount, "files"), humanBytes(stats.ScratchBytes))
	// Managed query images and inference scratch are their own rows because
	// they are their own lifecycle: references expire on a TTL their reaper
	// owns, and the scratch tree belongs to a runtime that may be writing to
	// it right now. Neither is ever cleanup candidates, so neither is folded
	// into a "rebuildable" figure that reads as deletable.
	fmt.Printf("%-14s %-8s %6s\n", "references", countLabel(stats.SearchReferenceCount, "files"), humanBytes(stats.SearchReferenceBytes))
	fmt.Printf("%-14s %-8s %6s\n", "infer scratch", countLabel(stats.InferenceScratchCount, "files"), humanBytes(stats.InferenceScratchBytes))
	fmt.Printf("%-14s %-8s %6s\n", "waveforms", countLabel(stats.WaveformCount, "files"), humanBytes(stats.WaveformBytes))
	fmt.Printf("%-14s %-8s %6s\n", "orphans", countLabel(len(orphans), "dirs"), humanBytes(orphanBytes))
	fmt.Printf("%-14s %-8s %6s\n", "database", "-", humanBytes(dbBytes))
	fmt.Printf("%-14s %-8s %6s\n", "total cache", countLabel(stats.TotalFiles, "files"), humanBytes(stats.TotalBytes))
	fmt.Printf("%-14s %-8s %6s\n", "rebuildable", "-", humanBytes(stats.RebuildableBytes))
	fmt.Printf("%-14s %-8s %6s\n", "unclassified", countLabel(stats.OrphanCount, "files"), humanBytes(stats.OrphanBytes))
	fmt.Printf("可安全释放（仅可重建）: %s\n", humanBytes(stats.RebuildableBytes))
	return nil
}

func countLabel(count int, unit string) string {
	return fmt.Sprintf("%d %s", count, unit)
}

// humanBytes renders a byte count the way the inspect table wants it read:
// whole numbers once a value reaches 10 in its unit ("84 MB"), one decimal
// below that ("1.2 GB").
func humanBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	value := float64(n)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if value < 10 {
		return fmt.Sprintf("%.1f %s", value, units[unit])
	}
	return fmt.Sprintf("%.0f %s", value, units[unit])
}

// runCacheGC deletes — or, without --yes, dry-runs the deletion of — exactly
// the categories an operator names: transient analysis scratch (--scratch),
// rebuildable derived artifacts (--rebuildable), and per-asset directories
// with no assets row (--orphans). The safety shape is fixed: with no category
// flags the command reports what a full run would free and deletes nothing,
// and deletion always requires --yes. The database, provider secrets,
// original media, cache/sources/ staging, managed search references and
// inference scratch are unreachable by construction: the walk never leaves
// the cache dir and never enters those trees.
//
// The exclusive cache maintenance lock is held for the whole deleting run —
// both passes on a real run — so the protection, eligibility and accounting
// cannot race a derive publishing under the shared lock. A report-only dry
// run takes no lock and mutates nothing. A real --rebuildable run is
// two passes by construction: a dry pass discovers the affected assets, a
// durable re-derive repair is enqueued for each of them BEFORE the second
// pass deletes anything, and only assets whose repair was recorded — now or
// still queued from an earlier run — have their files deleted. Assets whose
// repair cannot be durably recorded keep their files.
func runCacheGC(ctx context.Context, repo *sqliterepo.Repository, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("cache gc", flag.ContinueOnError)
	scratch := flags.Bool("scratch", false, "delete transient analysis scratch (analysis-frames/, analysis-windows/, tmp/, scratch/)")
	rebuildable := flags.Bool("rebuildable", false, "delete rebuildable derived artifacts (thumbnail-*.jpg, proxy-*.mp4, audio.m4a, waveform pairs)")
	orphans := flags.Bool("orphans", false, "delete asset cache directories with no assets row")
	yes := flags.Bool("yes", false, "actually delete; without it the run only reports what would be freed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	selected := *scratch || *rebuildable || *orphans
	if !selected {
		// No category named: report-only dry run of everything. --yes with no
		// category deletes nothing — the operator must say what goes.
		*scratch, *rebuildable, *orphans = true, true, true
	}
	// The exclusive cache maintenance lock is held for the whole run — both
	// passes on a real run — so the protection, eligibility and accounting a
	// deleting run reports cannot race a derive publishing under the shared
	// lock. A report-only dry run takes no lock and mutates nothing: it is
	// pure by contract, down to not creating the lock file.
	real := *yes && selected
	if real {
		lock, err := cachecoord.AcquireExclusive(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("acquire cache maintenance lock: %w", err)
		}
		defer lock.Release()
	}
	protected, err := protectedAssets(ctx, repo)
	if err != nil {
		return err
	}
	var orphanDirs []cache.OrphanDir
	if *orphans {
		orphanDirs, err = cache.OrphanDirectories(ctx, repo, cfg.CacheDir)
		if err != nil {
			return fmt.Errorf("detect orphan cache directories: %w", err)
		}
	}
	run := func(dryRun bool) (cache.GCResult, error) {
		return cache.GC(cfg.CacheDir, cache.GCOptions{
			DryRun:            dryRun,
			RemoveScratch:     *scratch,
			RemoveRebuildable: *rebuildable,
			RemoveOrphans:     orphanDirs,
			EligibleAssetIDs:  nil, // only the real pass gates on durable repairs
			ProtectedAssetIDs: protected,
		})
	}
	if !real {
		result, err := run(true)
		if err != nil {
			return err
		}
		reportGCResult(cfg, result, false, 0, 0)
		return nil
	}
	// Pass one: discover which assets have rebuildable artifacts, so a durable
	// repair can be recorded for each of them before anything is deleted.
	first, err := run(true)
	if err != nil {
		return err
	}
	eligible := make(map[string]struct{}, len(first.RemovedRebuildableAssetIDs))
	var enqueued, skipped int
	for _, assetID := range first.RemovedRebuildableAssetIDs {
		enqueuedJob, enqueueErr := repo.EnqueueRederive(ctx, assetID)
		if enqueueErr != nil {
			first.Errors = append(first.Errors, fmt.Sprintf("enqueue re-derive %s: %v", assetID, enqueueErr))
			continue
		}
		if enqueuedJob {
			eligible[assetID] = struct{}{}
			enqueued++
			continue
		}
		stillQueued, durableErr := repo.HasDurableRederive(ctx, assetID)
		if durableErr != nil {
			first.Errors = append(first.Errors, fmt.Sprintf("check queued re-derive %s: %v", assetID, durableErr))
			continue
		}
		if stillQueued {
			// A repair for this asset is already recorded and still queued;
			// the derive re-creates whatever is missing when it runs.
			eligible[assetID] = struct{}{}
			skipped++
			continue
		}
		// Not eligible (analysis uncommitted, metadata missing) or not
		// recordable: the asset's files are preserved.
		skipped++
	}
	// Pass two: the real deletion, gated per asset on the durable repairs.
	result, err := cache.GC(cfg.CacheDir, cache.GCOptions{
		DryRun:            false,
		RemoveScratch:     *scratch,
		RemoveRebuildable: *rebuildable,
		RemoveOrphans:     orphanDirs,
		EligibleAssetIDs:  eligible,
		ProtectedAssetIDs: protected,
	})
	if err != nil {
		return err
	}
	result.Errors = append(result.Errors, first.Errors...)
	reportGCResult(cfg, result, true, enqueued, skipped)
	return nil
}

// reportGCResult prints one GC run. The rebuildable promise is "the pipeline
// re-derives the files": a real run enqueued a JobDerive per affected asset
// whose analysis is already committed, so the deleted artifacts come back on
// the next pipeline pass — and the derive stage skips the paid chain for
// them, so a cache clean-up never re-bills a model run. Assets without a
// durable repair kept their files.
func reportGCResult(cfg config.Config, result cache.GCResult, real bool, enqueued, skipped int) {
	fmt.Printf("cache: %s\n", cfg.CacheDir)
	if len(result.Removed) == 0 {
		fmt.Printf("nothing to remove\n")
	} else {
		verb := "would free"
		if real {
			verb = "freed"
		}
		fmt.Printf("%s %s across %d item(s)\n", verb, humanBytes(result.FreedBytes), result.RemovedFiles)
		for _, rel := range result.Removed {
			fmt.Printf("  %s\n", rel)
		}
		if result.RemovedTruncated {
			fmt.Printf("  showing first 20 of %d\n", result.RemovedFiles)
		}
	}
	if result.SkippedActiveFiles > 0 {
		fmt.Printf("skipped active: %d file(s), %s\n", result.SkippedActiveFiles, humanBytes(result.SkippedActiveBytes))
	}
	if result.SkippedYoungFiles > 0 {
		fmt.Printf("skipped young: %d file(s), %s\n", result.SkippedYoungFiles, humanBytes(result.SkippedYoungBytes))
	}
	if result.SkippedUnrepairedFiles > 0 {
		fmt.Printf("skipped without durable repair: %d file(s), %s\n", result.SkippedUnrepairedFiles, humanBytes(result.SkippedUnrepairedBytes))
	}
	if real {
		if enqueued > 0 {
			fmt.Printf("已为 %d 个已完成分析的素材重新排队派生（跳过 %d 个）；运行 pipeline 后自动重建。\n", enqueued, skipped)
		} else if len(result.Removed) > 0 {
			fmt.Printf("派生重建任务已在队列中（跳过 %d 个）；运行 pipeline 后自动重建。\n", skipped)
		}
	} else {
		fmt.Println("dry run: nothing deleted; re-run with --yes to delete")
	}
	if len(result.Errors) > 0 {
		fmt.Printf("warnings: %d removal(s) failed; remaining work continued\n", len(result.Errors))
		for _, e := range result.Errors {
			fmt.Printf("  %s\n", e)
		}
	}
	fmt.Println("安全提示：可重建内容可安全删除；数据库/密钥/原片不受影响。")
}

// runCacheVerify reports DB-row-versus-file consistency: derived_artifacts
// rows whose file is missing are rebuildable gaps (the pipeline re-derives
// them on demand), and cache files with no row are orphan content.
// cache/sources/ staging is excluded from orphan accounting — the copy has
// no artifact row by design — and a waveform row is checked as a pair: a
// manifest whose named payload is missing is one missing artifact, and the
// payload the manifest names is never an orphan.
func runCacheVerify(ctx context.Context, repo *sqliterepo.Repository, cfg config.Config, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: nexusgate cache verify")
	}
	result, err := cache.Verify(ctx, repo, cfg.CacheDir)
	if err != nil {
		return fmt.Errorf("verify cache: %w", err)
	}
	fmt.Printf("cache: %s\n", cfg.CacheDir)
	fmt.Printf("%-20s %s\n", "artifact rows", countLabel(result.ArtifactRows, "rows"))
	fmt.Printf("%-20s %s\n", "missing files", countLabel(result.MissingFiles, "files"))
	fmt.Printf("%-20s %s\n", "orphan files", countLabel(result.OrphanFiles, "files"))
	fmt.Printf("%-20s %s\n", "cache files", countLabel(result.TotalCacheFiles, "files"))
	fmt.Printf("%-20s %s\n", "cache size", humanBytes(result.TotalCacheBytes))
	for _, path := range result.MissingPaths {
		fmt.Printf("  missing: %s\n", path)
	}
	for _, path := range result.OrphanPaths {
		fmt.Printf("  orphan: %s\n", path)
	}
	fmt.Println("安全提示：缺失文件可由 pipeline 重新派生；可重建内容可安全删除，数据库/密钥/原片不受影响。")
	return nil
}
