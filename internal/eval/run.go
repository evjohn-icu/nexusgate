package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/cachecoord"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/providers"
	"github.com/evjohn-icu/nexusgate/internal/providers/shotdetect"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
	"github.com/evjohn-icu/nexusgate/internal/staging"
)

// Run drives one provider configuration over the whole corpus: every clip
// through the real pipeline (probe → derive → analyze → index) into its own
// isolated database, one clip at a time so each asset's processing time is
// measured cleanly. The report the score step needs is written to
// <dataDir>/runs/<label>/report.json.
func Run(ctx context.Context, cfg config.Config, corpus *Corpus, dataDir, label string) (*RunReport, error) {
	dir, err := runDirectory(dataDir, label)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := cachecoord.AcquirePath(filepath.Join(dir, ".run.lock"), true)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	repo, err := sqlite.Open(filepath.Join(dir, "nexusgate.db"))
	if err != nil {
		return nil, err
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		return nil, err
	}
	cfg.CacheDir = filepath.Join(dir, "cache")
	pipeline, providerName, modelName, err := evalPipeline(repo, cfg)
	if err != nil {
		return nil, err
	}
	pipeline.SetCacheLockDir(dir)
	report := &RunReport{Label: label, Provider: providerName, Model: modelName, StartedAt: time.Now().UTC(), IsolatedDatabase: true}

	// The corpus clips are linked into one root so the pipeline sees them as
	// a real library without copying the footage (which could be many GB).
	rootPath := filepath.Join(dir, "corpus-root")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		return nil, err
	}
	root, err := corpusRoot(ctx, repo, rootPath)
	if err != nil {
		return nil, err
	}
	previous, err := LoadReport(dataDir, label)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	completed := previous != nil && previous.IsolatedDatabase
	if completed && len(previous.Assets) != len(corpus.Clips) {
		return nil, fmt.Errorf("corpus has changed; use a new run label")
	}
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		return nil, err
	}
	clipNames := make(map[string]bool, len(corpus.Clips))
	for _, clip := range corpus.Clips {
		if clipNames[clip.Name] {
			return nil, fmt.Errorf("duplicate corpus clip name %q", clip.Name)
		}
		clipNames[clip.Name] = true
	}
	for _, entry := range entries {
		if !clipNames[entry.Name()] {
			return nil, fmt.Errorf("corpus has changed; use a new run label")
		}
	}
	assetIDs := make(map[string]string, len(corpus.Clips))
	for _, clip := range corpus.Clips {
		assetID, err := seedClip(ctx, repo, root, clip, completed)
		if err != nil {
			return nil, fmt.Errorf("seed %s: %w", clip.Name, err)
		}
		assetIDs[clip.Name] = assetID
	}

	for _, clip := range corpus.Clips {
		assetID := assetIDs[clip.Name]
		if err := pipeline.EnqueueAsset(ctx, assetID); err != nil {
			return nil, err
		}
		started := time.Now()
		// One clip's whole chain at a time: the queue only ever holds this
		// asset's work, so the wall-clock delta is that asset's cost.
		if _, err := pipeline.RunUntilIdle(ctx); err != nil {
			return nil, err
		}
		elapsed := time.Since(started)
		metadata, err := repo.GetMediaMetadata(ctx, assetID)
		if err != nil {
			return nil, err
		}
		durationMS := int64(0)
		if metadata != nil {
			durationMS = metadata.DurationMS
		}
		shots, err := repo.ListAssetShots(ctx, assetID)
		if err != nil {
			return nil, err
		}
		frames := 0
		for _, shot := range shots {
			frames += len(media.PlanShotFrames(shot.StartMS, shot.EndMS, media.FrameSamplingBoundaryAware))
		}
		rt := 0.0
		if durationMS > 0 {
			rt = float64(elapsed.Milliseconds()) / float64(durationMS)
		}
		report.Assets = append(report.Assets, AssetReport{
			Clip: clip.Name, AssetID: assetID, DurationMS: durationMS,
			ProcessingMS: elapsed.Milliseconds(), RealTimeFactor: rt,
			Shots: len(shots), FramesProcessed: frames, Analyzed: len(shots) > 0,
		})
	}
	if err := report.Save(dataDir, label); err != nil {
		return nil, err
	}
	return report, nil
}

// evalPipeline mirrors NewService's provider wiring without the HTTP surface:
// same factories, same pipeline, nothing else. ASR and alignment are skipped
// — the eval corpus is visual — but vision and shot detection come straight
// from the evaluated config.
func evalPipeline(repo *sqlite.Repository, cfg config.Config) (*app.Pipeline, string, string, error) {
	videoProvider, err := providers.NewVideoUnderstandingProvider(cfg.Providers.VisionPrimary, cfg.Providers.VisionFallback, cfg.Providers)
	if err != nil {
		return nil, "", "", err
	}
	providerName, modelName := "none", "none"
	if videoProvider != nil {
		providerName, modelName = videoProvider.Name(), videoProvider.Model()
	}
	var detector shotdetect.Detector
	if videoProvider != nil {
		detector, err = providers.NewShotDetector(cfg.Providers)
		if err != nil {
			return nil, "", "", err
		}
	}
	_, plan := media.DetectHardware(context.Background(), cfg.Hardware)
	var stager *staging.SourceStager
	if stager, err = staging.New(cfg.SourceStaging.Mode, cfg.CacheDir); err != nil {
		return nil, "", "", err
	}
	deferral := time.Duration(cfg.Pipeline.ProviderRouteDeferralMinutes) * time.Minute
	return app.NewPipeline(repo, cfg.CacheDir, nil, nil, videoProvider, nil, detector, plan, stager, deferral, cfg.Pipeline.MinimumFreeSpaceBytes), providerName, modelName, nil
}

func corpusRoot(ctx context.Context, repo *sqlite.Repository, path string) (domain.LibraryRoot, error) {
	roots, err := repo.ListLibraryRoots(ctx)
	if err != nil {
		return domain.LibraryRoot{}, err
	}
	for _, root := range roots {
		if root.Path == path {
			return root, nil
		}
	}
	return repo.CreateLibraryRoot(ctx, path)
}

func seedClip(ctx context.Context, repo *sqlite.Repository, root domain.LibraryRoot, clip Clip, completed bool) (string, error) {
	if !filepath.IsLocal(clip.Name) || clip.Name == "." || filepath.Base(clip.Name) != clip.Name {
		return "", fmt.Errorf("invalid corpus clip name %q", clip.Name)
	}
	source, err := filepath.Abs(clip.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(source)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("source is not a regular file")
	}
	digest, err := HashFile(source)
	if err != nil {
		return "", err
	}
	// Preserve a separate asset per corpus name even for byte-identical clips.
	fingerprint := digest + "/" + clip.Name
	known, exists, err := repo.KnownFile(ctx, root.ID, clip.Name)
	if err != nil {
		return "", err
	}
	if (exists && (known.Fingerprint != fingerprint || known.Size != info.Size())) || (!exists && completed) {
		return "", fmt.Errorf("corpus clip identity has changed; use a new run label")
	}
	link := filepath.Join(root.Path, clip.Name)
	linked, err := os.Stat(link)
	switch {
	case err == nil:
		if !os.SameFile(info, linked) {
			return "", fmt.Errorf("corpus clip points to a different source; use a new run label")
		}
	case errors.Is(err, os.ErrNotExist):
		if _, linkErr := os.Lstat(link); !errors.Is(linkErr, os.ErrNotExist) {
			return "", fmt.Errorf("corpus clip link is unavailable: %w", err)
		}
		if err := os.Symlink(source, link); err != nil {
			// Hard links also work on systems where symlink creation is restricted.
			if err := os.Link(source, link); err != nil {
				return "", fmt.Errorf("link %s into the corpus root: %w", clip.Name, err)
			}
		}
	default:
		return "", err
	}

	scanned, err := repo.UpsertScannedFile(ctx, root, clip.Name, source, info, fingerprint)
	if err != nil {
		return "", err
	}
	return scanned.AssetID, nil
}
