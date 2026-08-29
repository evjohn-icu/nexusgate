package app

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/mount"
	"github.com/evjohn-icu/timingdex/internal/providers"
	"github.com/evjohn-icu/timingdex/internal/remote"
)

// DoctorReport collects every fact `timingdex doctor` renders into one
// JSON-safe, secret-free snapshot. It never fails on advisory checks: a
// missing helper binary, an unreadable mount table or a vanished cache
// subtree is recorded in the report itself (Present=false, empty,
// FreeDiskBytes=-1) because none of those is a verdict about the database.
// Repository errors are hard failures — they mean the Hub's own state cannot
// be read — and return the partial report alongside the error so a caller
// that wants the data anyway (the --json flag) still gets what was collected.
func (s *Service) DoctorReport(ctx context.Context) (domain.DoctorReport, error) {
	report := domain.DoctorReport{
		System: domain.SystemInfo{
			Version: domain.Version,
			GoOS:    runtime.GOOS,
			GoArch:  runtime.GOARCH,
		},
	}
	report.System.Hostname, _ = os.Hostname()
	if report.System.Hostname == "" {
		report.System.Hostname = "unknown"
	}

	report.DB.Path = s.cfg.DatabasePath
	report.DB.IntegrityOK = s.repo.IntegrityCheck(ctx) == nil

	if err := s.collectDBInfo(ctx, &report); err != nil {
		return report, err
	}
	if err := s.collectStorage(&report); err != nil {
		return report, err
	}
	if err := s.collectRoots(ctx, &report); err != nil {
		return report, err
	}
	s.collectTools(ctx, &report)

	var gpu strings.Builder
	media.FormatHardwareReport(&gpu, s.HardwareReport())
	report.GPU.HardwareReport = strings.TrimRight(gpu.String(), "\n")

	if err := s.collectProviders(ctx, &report); err != nil {
		return report, err
	}
	if err := s.collectSearchIndex(ctx, &report); err != nil {
		return report, err
	}
	if err := s.collectWorkers(ctx, &report); err != nil {
		return report, err
	}
	if err := s.collectQueue(ctx, &report); err != nil {
		return report, err
	}
	return report, nil
}

func (s *Service) collectDBInfo(ctx context.Context, report *domain.DoctorReport) error {
	migrations, err := s.repo.MigrationStatus(ctx)
	if err != nil {
		return err
	}
	report.DB.MigrationsApplied = migrations.Applied
	report.DB.MigrationsTotal = migrations.Total
	report.DB.SchemaVersion = migrations.LastApplied
	if s.cfg.DatabasePath == "" {
		return nil
	}
	info, err := os.Stat(s.cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("stat database: %w", err)
	}
	report.Storage.DBSizeBytes = info.Size()
	return nil
}

// collectStorage walks the cache directory once for both the total size and
// the stale-scratch census. There is no single scratch directory: source
// staging writes "<version>-*.partial" files beside its destinations
// (cache/sources/...), multiframe analysis renders "analysis-frames/"
// directories next to proxies, and TLS rotation writes ".tmp-*" files. All
// three are removed by their writer at job end, so a 24-hour-old instance is
// a crashed run's debris, not an active one.
func (s *Service) collectStorage(report *domain.DoctorReport) error {
	report.Storage.CachePath = s.cfg.CacheDir
	free, err := freeBytes(s.cfg.CacheDir)
	if err != nil {
		report.Storage.FreeDiskBytes = -1
	} else {
		report.Storage.FreeDiskBytes = free
	}
	_ = filepath.WalkDir(s.cfg.CacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		report.Storage.CacheBytes += info.Size()
		if isStaleScratchFile(path, info.ModTime()) {
			report.TempFiles.Files++
			report.TempFiles.Bytes += info.Size()
		}
		return nil
	})
	return nil
}

func isStaleScratchFile(path string, modified time.Time) bool {
	if time.Since(modified) < 24*time.Hour {
		return false
	}
	base := filepath.Base(path)
	if strings.Contains(base, ".partial") || strings.Contains(base, ".tmp") {
		return true
	}
	return filepath.Base(filepath.Dir(path)) == "analysis-frames"
}

func (s *Service) collectRoots(ctx context.Context, report *domain.DoctorReport) error {
	roots, err := s.repo.ListLibraryRoots(ctx)
	if err != nil {
		return err
	}
	table := mount.ReadMountTable()
	for _, root := range roots {
		info := domain.RootInfo{
			Path:          root.Path,
			State:         string(root.HealthState),
			LastHealthyAt: root.LastHealthyAt,
			// Doctor's root advice always treats the root as registered, so
			// the unmounted-share warning is offered here exactly as it is on
			// the command line (see TestListLibraryRootsDoctorAlwaysPassesRegistered).
			Warnings: s.RootWarnings(root.Path, true),
		}
		if filesystem, known := mount.FilesystemFor(root.Path, table); known {
			info.FilesystemType = filesystem.Type
			if filesystem.Network {
				info.FilesystemType = filesystem.Label + " (" + filesystem.Type + ")"
			}
		}
		report.Roots = append(report.Roots, info)
	}
	return nil
}

func (s *Service) collectTools(ctx context.Context, report *domain.DoctorReport) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	report.FFmpeg = domain.FFmpegInfo{Path: ffmpegPath, Present: err == nil}
	if err == nil {
		report.FFmpeg.Version = ffmpegVersionLine(ctx, ffmpegPath)
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	report.FFprobe = domain.ToolInfo{Path: ffprobePath, Present: err == nil}
	exiftoolPath, err := exec.LookPath("exiftool")
	report.ExifTool = domain.ToolInfo{Path: exiftoolPath, Present: err == nil}
}

// ffmpegVersionLine returns the first line of `ffmpeg -version` — the build
// name and version in one string — or "" when the binary is present but will
// not answer. The timeout keeps a wedged helper from hanging doctor forever.
func ffmpegVersionLine(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

func (s *Service) collectProviders(ctx context.Context, report *domain.DoctorReport) error {
	// The pre-dispatch fail-fast is skipped for `doctor` so a broken legacy
	// providers.* config can be diagnosed; carry the secret-free reason in the
	// report instead of aborting collection. ValidateProviderConfig only
	// reports configuration facts (selected, enabled, keyed) — never secrets.
	if err := providers.ValidateProviderConfig(s.cfg.Providers); err != nil {
		report.Providers.ConfigError = err.Error()
	} else {
		report.Providers.ConfigValid = true
	}
	channels, err := s.ListProviderChannels(ctx, "")
	if err != nil {
		return err
	}
	report.Providers.Channels = len(channels)
	for _, channel := range channels {
		for _, member := range channel.Members {
			if member.SecretReady {
				report.Providers.MembersWithSecrets++
			}
		}
	}
	// Degraded comes from the runtime snapshot, not the configuration rows:
	// a key the pool has retired (401/402/403), parked in cooldown, or left
	// half-open is invisible to configuration and is exactly what an operator
	// needs told. A capability with no runtime data yet is not degraded.
	for _, status := range s.ProviderChannelRuntimeStatus(ctx) {
		if !status.HasRuntimeData || !status.HasRoute || status.Snapshot == nil {
			continue
		}
		for _, channel := range status.Snapshot.Channels {
			if !channel.Enabled {
				continue
			}
			for _, member := range channel.Members {
				if member.Retired || member.HalfOpen || !member.CooldownUntil.IsZero() {
					report.Providers.Degraded++
					break
				}
			}
		}
	}
	return nil
}

func (s *Service) collectSearchIndex(ctx context.Context, report *domain.DoctorReport) error {
	health, err := s.repo.SearchIndexHealth(ctx)
	if err != nil {
		return err
	}
	report.SearchIndex = domain.SearchIndexInfo{
		ShotCount:           health.ShotCount,
		FTSReady:            health.FTSState == "ready",
		EmbeddingsCount:     health.EmbeddingsCount,
		EmbeddingsShotCount: health.EmbeddingsShotCount,
	}
	return nil
}

func (s *Service) collectWorkers(ctx context.Context, report *domain.DoctorReport) error {
	workers, err := s.ListWorkers(ctx)
	if err != nil {
		return err
	}
	for _, worker := range workers {
		lastSeen := worker.LastSeenAt
		report.Workers = append(report.Workers, domain.WorkerInfo{
			Name:       worker.Name,
			Status:     string(worker.Status),
			Version:    worker.Version,
			LastSeenAt: &lastSeen,
		})
	}
	return nil
}

func (s *Service) collectQueue(ctx context.Context, report *domain.DoctorReport) error {
	summary, err := s.JobSummary(ctx)
	if err != nil {
		return err
	}
	report.Queue = domain.QueueInfo{
		Pending:   summary.Pending,
		Running:   summary.Running,
		Succeeded: summary.Succeeded,
		Failed:    summary.Failed,
		Terminal:  summary.Terminal,
		Deferred:  summary.Deferred,
	}
	return nil
}

// printDoctorReport renders the report as the sectioned operator console.
// Marks are: ✓ healthy, ⚠ advisory, ✗ needs action. Only a broken integrity
// check (and a collection failure, surfaced as a returned error by Doctor)
// makes the command exit non-zero.
func printDoctorReport(w io.Writer, report domain.DoctorReport) {
	fmt.Fprintln(w, "SYSTEM")
	fmt.Fprintf(w, "✓ host %s · go %s/%s · version %s\n", report.System.Hostname, report.System.GoOS, report.System.GoArch, report.System.Version)

	fmt.Fprintln(w, "DB")
	schema := report.DB.SchemaVersion
	if schema == "" {
		schema = "none"
	}
	if report.DB.IntegrityOK {
		fmt.Fprintf(w, "✓ integrity ok · schema %s (%d/%d applied)\n", schema, report.DB.MigrationsApplied, report.DB.MigrationsTotal)
	} else {
		fmt.Fprintf(w, "✗ integrity check failed · schema %s (%d/%d applied)\n", schema, report.DB.MigrationsApplied, report.DB.MigrationsTotal)
	}

	fmt.Fprintln(w, "STORAGE")
	mark, free := "✓", "unknown"
	if report.Storage.FreeDiskBytes >= 0 {
		free = formatBytes(report.Storage.FreeDiskBytes)
		if report.Storage.FreeDiskBytes < 10<<30 {
			mark = "⚠"
		}
	}
	fmt.Fprintf(w, "%s cache %s (%s used) · %s free · db %s\n", mark, report.Storage.CachePath, formatBytes(report.Storage.CacheBytes), free, formatBytes(report.Storage.DBSizeBytes))

	fmt.Fprintln(w, "MEDIA ROOTS")
	if len(report.Roots) == 0 {
		fmt.Fprintln(w, "✓ no library roots configured")
	}
	for _, root := range report.Roots {
		mark, state := "⚠", "unknown"
		switch root.State {
		case string(domain.RootHealthHealthy):
			mark, state = "✓", "healthy"
		case string(domain.RootHealthUnavailable):
			mark, state = "✗", "unavailable"
		}
		last := "never scanned"
		if root.LastHealthyAt != nil {
			last = root.LastHealthyAt.UTC().Format("2006-01-02 15:04 UTC")
		}
		filesystem := root.FilesystemType
		if filesystem == "" {
			filesystem = "unknown filesystem"
		}
		fmt.Fprintf(w, "%s %s · %s · %s · last healthy %s\n", mark, root.Path, state, filesystem, last)
		for _, warning := range root.Warnings {
			fmt.Fprintf(w, "  ⚠ %s\n", warning)
		}
	}

	fmt.Fprintln(w, "FFMPEG")
	if report.FFmpeg.Present {
		fmt.Fprintf(w, "✓ %s", report.FFmpeg.Path)
		if report.FFmpeg.Version != "" {
			fmt.Fprintf(w, " · %s", report.FFmpeg.Version)
		}
		fmt.Fprintln(w)
	} else {
		fmt.Fprintln(w, "✗ ffmpeg not found on PATH; probe/derive stages cannot run")
	}
	fmt.Fprintln(w, "FFPROBE")
	if report.FFprobe.Present {
		fmt.Fprintf(w, "✓ %s\n", report.FFprobe.Path)
	} else {
		fmt.Fprintln(w, "✗ ffprobe not found on PATH; probe stage cannot run")
	}
	fmt.Fprintln(w, "EXIFTOOL")
	if report.ExifTool.Present {
		fmt.Fprintf(w, "✓ %s\n", report.ExifTool.Path)
	} else {
		fmt.Fprintln(w, "⚠ exiftool not found on PATH; camera metadata stays empty")
	}

	fmt.Fprintln(w, "GPU")
	fmt.Fprintln(w, report.GPU.HardwareReport)

	fmt.Fprintln(w, "PROVIDERS")
	if report.Providers.ConfigError != "" {
		fmt.Fprintf(w, "✗ legacy provider config: %s\n", report.Providers.ConfigError)
	}
	if report.Providers.Channels == 0 {
		fmt.Fprintln(w, "⚠ no provider channels configured (legacy providers.* config may still serve)")
	} else {
		fmt.Fprintf(w, "✓ %d channel(s) · %d member(s) with secrets\n", report.Providers.Channels, report.Providers.MembersWithSecrets)
		if report.Providers.Degraded > 0 {
			fmt.Fprintf(w, "⚠ %d channel route(s) degraded (retired or cooldown key)\n", report.Providers.Degraded)
		}
	}

	fmt.Fprintln(w, "SEARCH INDEX")
	if report.SearchIndex.FTSReady {
		fmt.Fprintf(w, "✓ %d shot(s) · FTS ready · %d/%d shot(s) embedded\n", report.SearchIndex.ShotCount, report.SearchIndex.EmbeddingsShotCount, report.SearchIndex.ShotCount)
	} else {
		fmt.Fprintf(w, "⚠ FTS rebuild pending · %d shot(s) · %d/%d shot(s) embedded\n", report.SearchIndex.ShotCount, report.SearchIndex.EmbeddingsShotCount, report.SearchIndex.ShotCount)
	}

	fmt.Fprintln(w, "WORKERS")
	if len(report.Workers) == 0 {
		fmt.Fprintln(w, "✓ no workers enrolled")
	}
	for _, worker := range report.Workers {
		mark := "✓"
		if worker.Status != string(remote.WorkerOnline) {
			mark = "⚠"
		}
		last := "never"
		if worker.LastSeenAt != nil {
			last = worker.LastSeenAt.UTC().Format("2006-01-02 15:04 UTC")
		}
		fmt.Fprintf(w, "%s %s · %s · %s · seen %s\n", mark, worker.Name, worker.Status, worker.Version, last)
	}

	fmt.Fprintln(w, "QUEUE")
	fmt.Fprintf(w, "✓ pending %d · running %d · deferred %d · failed %d · terminal %d · succeeded %d\n",
		report.Queue.Pending, report.Queue.Running, report.Queue.Deferred, report.Queue.Failed, report.Queue.Terminal, report.Queue.Succeeded)

	fmt.Fprintln(w, "TEMP FILES")
	if report.TempFiles.Files == 0 {
		fmt.Fprintln(w, "✓ no stale scratch files")
	} else {
		fmt.Fprintf(w, "⚠ %d stale scratch file(s) (%s) older than 24h; likely a crashed run\n", report.TempFiles.Files, formatBytes(report.TempFiles.Bytes))
	}
}

func formatBytes(bytes int64) string {
	switch {
	case bytes < 1<<10:
		return fmt.Sprintf("%d B", bytes)
	case bytes < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(bytes)/(1<<10))
	case bytes < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/(1<<20))
	default:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30))
	}
}
