package app

import (
	"context"
	"os"
	"os/exec"

	"github.com/evjohn-icu/timingdex/internal/domain"
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
	}
	// An empty capability lists every channel row — the count of channels, not
	// members: one channel per capability is the ordinary setup, and the guide
	// asks "is any provider configured?", which one row answers.
	if channels, err := s.repo.ListProviderChannels(ctx, ""); err == nil {
		status.ProviderCount = len(channels)
	}
	// ListAssets is the existing interface's count vehicle (no CountAssets
	// exists); 500 is a cap for an existence check, not an exact count.
	if assets, err := s.repo.ListAssets(ctx, 500, 0); err == nil {
		status.AssetCount = len(assets)
	}

	status.NextStep = setupNextStep(status)
	// Ready means the loop is actually usable end to end: binaries, writable
	// storage, a healthy DB, and at least one scanned asset to search. A hub
	// with a root and a provider but nothing scanned is not ready to search —
	// the step line says scan_or_process, so the pill must not claim 已完成.
	status.Ready = status.FFmpeg && status.FFprobe && status.ExifTool &&
		status.DataDirWritable && status.CacheWritable &&
		status.FreeDiskOK && status.DBHealthy &&
		status.RootCount > 0 && status.ProviderCount > 0 && status.AssetCount > 0
	return status
}

// setupNextStep implements the documented ordering: missing roots come first,
// then a root with no provider, then a root+provider with nothing scanned,
// then a library with assets (the ordinary end state, which the guide calls
// "search"). Ready is a separate boolean on SetupStatus — a hub is never
// "done" in the sense of having no next step, because scanning is continuous.
func setupNextStep(status domain.SetupStatus) string {
	switch {
	case status.RootCount == 0:
		return SetupNextStepAddFootage
	case status.ProviderCount == 0:
		return SetupNextStepConfigureProviders
	case status.AssetCount == 0:
		return SetupNextStepScanOrProcess
	default:
		return SetupNextStepSearch
	}
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
