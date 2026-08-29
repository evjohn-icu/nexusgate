package app

import (
	"context"
	"os"
	"os/exec"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/providerchannels"
	"github.com/evjohn-icu/timingdex/internal/providers"
)

// SetupNextStep* name the single most useful action for a fresh Hub, in the
// order the guide should present them. The ordering is a heuristic, not a
// gate: it assumes the ordinary first-run shape (roots before providers before
// processing) and only points at the missing piece. "ready" is the fallback
// when every piece of the ordinary shape exists.
const (
	SetupNextStepAddFootage         = "add_footage"
	SetupNextStepConfigureProviders = "configure_providers"
	SetupNextStepScanOrProcess      = "scan_or_process"
	SetupNextStepSearch             = "search"
	// SetupNextStepReady is reserved for a future "everything processed" test;
	// the current heuristic treats search as the terminal guidance because a
	// footage library is never finished scanning. Kept so the page's
	// step→action map does not need to know which states exist.
	SetupNextStepReady = "ready"
)

// SetupStatus builds the first-run environment snapshot behind the /setup
// guide. Every probe degrades to the pessimistic value on error — a missing
// binary, an unwritable directory or an unreadable count reports itself
// rather than failing the whole call — so the page always has an answer.
func (s *Service) SetupStatus(ctx context.Context) domain.SetupStatus {
	status := domain.SetupStatus{}
	// Same binary set as Doctor: these three are what the pipeline needs on
	// PATH. Doctor prints them; here each becomes a boolean the guide can
	// check one by one.
	for _, binary := range []string{"ffmpeg", "ffprobe", "exiftool"} {
		_, err := exec.LookPath(binary)
		switch binary {
		case "ffmpeg":
			status.FFmpeg = err == nil
		case "ffprobe":
			status.FFprobe = err == nil
		default:
			status.ExifTool = err == nil
		}
	}
	status.DataDirWritable = dirWritable(s.cfg.DataDir)
	status.CacheWritable = dirWritable(s.cfg.CacheDir)
	// freeBytes is the same Statfs-based probe the pipeline's disk preflight
	// uses; 1 GiB is the floor for a scan's worth of thumbnails and proxies.
	// On Windows it degrades to "unknown", which reports not-OK — conservative,
	// like the preflight's own skip-never-block posture inverted.
	if bytes, err := freeBytes(s.cfg.CacheDir); err == nil {
		status.FreeDiskBytes = bytes
		status.FreeDiskOK = bytes >= 1<<30
	}
	status.DBHealthy = s.repo.IntegrityCheck(ctx) == nil

	// Each count degrades to zero on error: a catalogued library that cannot
	// be read is a different problem than first-run setup, and the guide
	// should not claim progress it cannot verify.
	if roots, err := s.repo.ListLibraryRoots(ctx); err == nil {
		status.RootCount = len(roots)
		for _, root := range roots {
			if root.HealthState == domain.RootHealthHealthy {
				status.HealthyRootCount++
			}
		}
	}
	// ListProviderChannels is the secret-redacting view, so ProviderReady never
	// depends on a member's raw key — only on whether a stored secret resolves.
	// A saved-but-disabled or keyless channel keeps provider_ready false.
	if channels, err := s.ListProviderChannels(ctx, ""); err == nil {
		status.ProviderCount = len(channels)
		status.ProviderReady = channelVideoRouteReady(channels) || legacyVisionRouteReady(s.ProviderSummary())
	}
	// ListAssets is the existing interface's count vehicle (no CountAssets
	// exists); 500 is a cap for an existence check, not an exact count.
	if assets, err := s.repo.ListAssets(ctx, 500, 0); err == nil {
		status.AssetCount = len(assets)
	}
	// SearchIndexHealth is the same repository view doctor renders: canonical
	// shot rows and the FTS flag. "missing" (no index ever needed) counts as
	// ready exactly as doctor treats it; only "pending" blocks search.
	if health, err := s.repo.SearchIndexHealth(ctx); err == nil {
		status.SearchableShotCount = health.ShotCount
		status.SearchIndexReady = health.FTSState == "ready" || health.FTSState == "missing"
	}

	status.NextStep = setupNextStep(status)
	// Ready means the loop is actually usable end to end: binaries, writable
	// storage, a healthy DB, a healthy root, a runnable video Provider, at
	// least one canonical shot, and a ready search index. ExifTool stays
	// optional (the page labels it so), and a raw asset row or a disabled/
	// keyless channel must not make the pill claim 已完成.
	status.Ready = status.FFmpeg && status.FFprobe &&
		status.DataDirWritable && status.CacheWritable &&
		status.FreeDiskOK && status.DBHealthy &&
		status.HealthyRootCount > 0 && status.ProviderReady &&
		status.SearchableShotCount > 0 && status.SearchIndexReady
	return status
}

// setupNextStep implements the documented ordering: missing roots come first,
// then no runnable video route, then a healthy root with nothing searchable
// (or a pending index), then the ordinary end state the guide calls "search".
// Ready is a separate boolean on SetupStatus — a hub is never "done" in the
// sense of having no next step, because scanning is continuous.
func setupNextStep(status domain.SetupStatus) string {
	switch {
	case status.RootCount == 0:
		return SetupNextStepAddFootage
	case !status.ProviderReady:
		return SetupNextStepConfigureProviders
	case status.HealthyRootCount == 0 || status.SearchableShotCount == 0 || !status.SearchIndexReady:
		return SetupNextStepScanOrProcess
	default:
		return SetupNextStepSearch
	}
}

// channelVideoRouteReady reports whether any persisted channel gives
// video_analysis a runnable route: the channel is enabled, its provider is
// runtime-supported, and at least one enabled member has a stored secret. A
// saved-but-disabled or keyless channel is deliberately not ready.
func channelVideoRouteReady(channels []domain.ProviderChannel) bool {
	for _, ch := range channels {
		if ch.Capability != string(providerchannels.CapabilityVideoAnalysis) || !ch.Enabled {
			continue
		}
		if !supportedChannelProvider(providerchannels.CapabilityVideoAnalysis, ch.ProviderName) {
			continue
		}
		for _, member := range ch.Members {
			if member.Enabled && member.SecretReady {
				return true
			}
		}
	}
	return false
}

// legacyVisionRouteReady reports whether the resolved providers.* config
// selects an enabled vision block as its primary or a fallback — the legacy
// path that predates provider channels. SummarizeProviders lists only enabled
// blocks under Configured, so a selected-but-disabled provider is not ready.
func legacyVisionRouteReady(summary providers.ProviderSummary) bool {
	enabled := make(map[string]bool, len(summary.Vision.Configured))
	for _, p := range summary.Vision.Configured {
		enabled[p.Name] = true
	}
	if enabled[summary.Vision.Primary] {
		return true
	}
	for _, fallback := range summary.Vision.Fallbacks {
		if enabled[fallback] {
			return true
		}
	}
	return false
}

// dirWritable probes a directory by creating and removing a temporary file.
// Permission bits lie about ACLs, read-only mounts and FUSE layers; an actual
// create+remove does not. The probe file is always removed, even on failure.
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".setup-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return false
	}
	return os.Remove(name) == nil
}
