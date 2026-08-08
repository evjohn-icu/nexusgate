package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/providers"
	"github.com/evjohn-icu/timingdex/internal/providers/shotdetect"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
	"github.com/evjohn-icu/timingdex/internal/staging"
)

// Run drives one provider configuration over the whole corpus: every clip
// through the real pipeline (probe → derive → analyze → index) into its own
// isolated database, one clip at a time so each asset's processing time is
// measured cleanly. The report the score step needs is written to
// <dataDir>/runs/<label>/report.json.
func Run(ctx context.Context, cfg config.Config, corpus *Corpus, dataDir, label string) (*RunReport, error) {
	if label == "" {
		return nil, fmt.Errorf("label is required")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	repo, err := sqlite.Open(filepath.Join(dataDir, "timingdex.db"))
	if err != nil {
		return nil, err
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		return nil, err
	}
	pipeline, providerName, modelName, err := evalPipeline(repo, cfg)
	if err != nil {
		return nil, err
	}
	report := &RunReport{Label: label, Provider: providerName, Model: modelName, StartedAt: time.Now().UTC()}

	// The corpus clips are linked into one root so the pipeline sees them as
	// a real library without copying the footage (which could be many GB).
	rootPath := filepath.Join(dataDir, "corpus-root")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		return nil, err
	}
	root, err := repo.CreateLibraryRoot(ctx, rootPath)
	if err != nil {
		return nil, err
	}
	assetIDs := make(map[string]string, len(corpus.Clips))
	for _, clip := range corpus.Clips {
		link := filepath.Join(rootPath, clip.Name)
		// Re-running the harness into the same data dir (e.g. after a failed
		// run) must not die on the existing links.
		if _, err := os.Lstat(link); err == nil {
			// Already linked; leave it.
		} else if err := os.Symlink(clip.Path, link); err != nil {
			// Some filesystems refuse symlinks (Windows without privileges);
			// a hard link is the next-best thing and equally cheap.
			if err := os.Link(clip.Path, link); err != nil {
				return nil, fmt.Errorf("link %s into the corpus root: %w", clip.Name, err)
			}
		}
		assetID, err := seedClip(ctx, repo, root, clip)
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
	return app.NewPipeline(repo, cfg.CacheDir, nil, nil, videoProvider, nil, detector, plan, stager, deferral), providerName, modelName, nil
}

func seedClip(ctx context.Context, repo *sqlite.Repository, root domain.LibraryRoot, clip Clip) (string, error) {
	info, err := os.Stat(clip.Path)
	if err != nil {
		return "", err
	}
	scanned, err := repo.UpsertScannedFile(ctx, root, clip.Name, clip.Path, info, "eval-fp-"+clip.Name)
	if err != nil {
		return "", err
	}
	return scanned.AssetID, nil
}
