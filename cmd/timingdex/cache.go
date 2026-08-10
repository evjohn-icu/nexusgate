package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/evjohn-icu/timingdex/internal/cache"
	"github.com/evjohn-icu/timingdex/internal/cachecoord"
	"github.com/evjohn-icu/timingdex/internal/config"
	sqliterepo "github.com/evjohn-icu/timingdex/internal/repository/sqlite"
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
		return errors.New("usage: timingdex cache inspect|gc|verify|repair-derived")
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
		return errors.New("usage: timingdex cache inspect|gc|verify|repair-derived")
	}
}

func runCacheRepairDerived(ctx context.Context, repo *sqliterepo.Repository, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("cache repair-derived", flag.ContinueOnError)
	invalidate := flags.Bool("invalidate-hardware-profiles", false, "remove hardware-produced thumbnail/proxy files and rows")
	yes := flags.Bool("yes", false, "delete and enqueue re-derive jobs")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*invalidate {
		return errors.New("usage: timingdex cache repair-derived --invalidate-hardware-profiles [--yes]")
	}
	prefixes := []string{"thumb-hw-", "proxy-720-hw-"}
	var files []string
	var bytes int64
	err := filepath.WalkDir(cfg.CacheDir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "sources" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !(strings.HasPrefix(name, "thumbnail-") && strings.HasSuffix(name, ".jpg") || strings.HasPrefix(name, "proxy-") && strings.HasSuffix(name, ".mp4")) {
			return nil
		}
		mode := strings.TrimSuffix(strings.TrimPrefix(name, "thumbnail-"), ".jpg")
		if strings.HasPrefix(name, "proxy-") {
			mode = strings.TrimSuffix(strings.TrimPrefix(name, "proxy-"), ".mp4")
		}
		if mode == "software" || mode == "" {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		files = append(files, p)
		bytes += info.Size()
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("scan derived cache: %w", err)
	}
	matching, err := repo.MatchingDerivedArtifactsByProfilePrefixes(ctx, prefixes)
	if err != nil {
		return fmt.Errorf("find hardware artifact rows: %w", err)
	}
	assets := make([]string, 0, len(matching))
	seen := make(map[string]bool)
	for _, artifact := range matching {
		if !seen[artifact.AssetID] {
			seen[artifact.AssetID] = true
			assets = append(assets, artifact.AssetID)
		}
	}
	if !*yes {
		fmt.Printf("would remove %d hardware-derived file(s), %s, and %d DB row(s) across %d asset(s)\n", len(files), humanBytes(bytes), len(matching), len(assets))
		fmt.Println("dry run: nothing deleted; re-run with --yes to repair")
		return nil
	}
	if _, err := repo.DeleteDerivedArtifactsByProfilePrefixes(ctx, prefixes); err != nil {
		return fmt.Errorf("delete hardware artifact rows: %w", err)
	}
	for _, p := range files {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	var enqueued int
	for _, assetID := range assets {
		ok, err := repo.EnqueueRederive(ctx, assetID)
		if err != nil {
			return err
		}
		if ok {
			enqueued++
		}
	}
	fmt.Printf("removed %d hardware-derived file(s), %s; enqueued %d re-derive job(s)\n", len(files), humanBytes(bytes), enqueued)
	return nil
}

func runCacheInspect(ctx context.Context, repo *sqliterepo.Repository, cfg config.Config, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: timingdex cache inspect")
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
	fmt.Printf("%-14s %-8s %6s\n", "source staging", countLabel(stats.SourceStagingCount, "files"), humanBytes(stats.SourceStagingBytes))
	fmt.Printf("%-14s %-8s %6s\n", "scratch", countLabel(stats.ScratchCount, "files"), humanBytes(stats.ScratchBytes))
	fmt.Printf("%-14s %-8s %6s\n", "orphans", countLabel(len(orphans), "dirs"), humanBytes(orphanBytes))
	fmt.Printf("%-14s %-8s %6s\n", "database", "-", humanBytes(dbBytes))
	fmt.Printf("%-14s %-8s %6s\n", "total derived", "-", humanBytes(stats.TotalBytes))
	// Everything except the database is regenerable: derived artifacts come
	// back from a pipeline re-run, staging copies re-arrive on demand, and
	// orphan directories are stale by definition.
	fmt.Printf("可安全释放（可重建）: %s\n", humanBytes(stats.TotalBytes))
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
// original media and cache/sources/ staging are unreachable by construction:
// the walk never leaves the cache dir and never enters sources/.
func runCacheGC(ctx context.Context, repo *sqliterepo.Repository, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("cache gc", flag.ContinueOnError)
	scratch := flags.Bool("scratch", false, "delete transient analysis scratch (analysis-frames/, analysis-windows/, tmp/, scratch/)")
	rebuildable := flags.Bool("rebuildable", false, "delete rebuildable derived artifacts (thumbnail-*.jpg, proxy-*.mp4, audio.m4a)")
	orphans := flags.Bool("orphans", false, "delete asset cache directories with no assets row")
	yes := flags.Bool("yes", false, "actually delete; without it the run only reports what would be freed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	lock, err := cachecoord.AcquireExclusive(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("acquire cache maintenance lock: %w", err)
	}
	defer lock.Release()
	live, err := repo.ListLiveJobAssetIDs(ctx, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("list live job assets: %w", err)
	}
	protected := make(map[string]struct{}, len(live))
	for _, id := range live {
		protected[id] = struct{}{}
	}
	selected := *scratch || *rebuildable || *orphans
	if !selected {
		// No category named: report-only dry run of everything. --yes with no
		// category deletes nothing — the operator must say what goes.
		*scratch, *rebuildable, *orphans = true, true, true
	}
	var orphanDirs []cache.OrphanDir
	if *orphans {
		var err error
		orphanDirs, err = cache.OrphanDirectories(ctx, repo, cfg.CacheDir)
		if err != nil {
			return fmt.Errorf("detect orphan cache directories: %w", err)
		}
	}
	result, err := cache.GC(cfg.CacheDir, cache.GCOptions{
		DryRun:            !*yes || !selected,
		RemoveScratch:     *scratch,
		RemoveRebuildable: *rebuildable,
		RemoveOrphans:     orphanDirs,
		ProtectedAssetIDs: protected,
	})
	if err != nil {
		return err
	}
	// The GC promise is "the pipeline re-derives the files": a real run of
	// --rebuildable enqueues a JobDerive per affected asset whose analysis is
	// already committed, so the deleted thumbnails/proxies/audio come back on
	// the next pipeline pass — and the derive stage skips the paid chain for
	// them, so a cache clean-up never re-bills a model run. Assets without
	// committed analysis are left to their own in-flight chain.
	var enqueued, skipped int
	if *rebuildable && *yes && selected {
		for _, assetID := range result.RemovedRebuildableAssetIDs {
			enqueuedJob, enqueueErr := repo.EnqueueRederive(ctx, assetID)
			if enqueueErr != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("enqueue re-derive %s: %v", assetID, enqueueErr))
				continue
			}
			if enqueuedJob {
				enqueued++
			} else {
				skipped++
			}
		}
	}
	fmt.Printf("cache: %s\n", cfg.CacheDir)
	if len(result.Removed) == 0 {
		fmt.Printf("nothing to remove\n")
	} else {
		verb := "would free"
		if *yes && selected {
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
	if !*yes || !selected {
		fmt.Println("dry run: nothing deleted; re-run with --yes to delete")
	} else if enqueued > 0 {
		fmt.Printf("已为 %d 个已完成分析的素材重新排队派生（跳过 %d 个未完成分析的素材）；运行 pipeline 后自动重建。\n", enqueued, skipped)
	}
	if len(result.Errors) > 0 {
		fmt.Printf("warnings: %d removal(s) failed; remaining work continued\n", len(result.Errors))
		for _, e := range result.Errors {
			fmt.Printf("  %s\n", e)
		}
	}
	fmt.Println("安全提示：可重建内容可安全删除；数据库/密钥/原片不受影响。")
	return nil
}

// runCacheVerify reports DB-row-versus-file consistency: derived_artifacts
// rows whose file is missing are rebuildable gaps (the pipeline re-derives
// them on demand), and cache files with no row are orphan content.
// cache/sources/ staging is excluded from orphan accounting — the copy has
// no artifact row by design.
func runCacheVerify(ctx context.Context, repo *sqliterepo.Repository, cfg config.Config, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: timingdex cache verify")
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
