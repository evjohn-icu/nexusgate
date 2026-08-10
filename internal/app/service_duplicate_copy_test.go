package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/ingest"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// This stays at the Service boundary: the scan discovers the copy, and the
// service's pipeline enqueue path resolves the asset-level probe identity.
func TestServiceDuplicateCopyDoesNotCreateProbeOrModelRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	repo, err := sqlite.Open(filepath.Join(dir, "duplicate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Join(dir, "footage")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, rootDir)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, config.Config{DataDir: dir, CacheDir: filepath.Join(dir, "cache"), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(rootDir, "original.mp4")
	if err := os.WriteFile(original, []byte("byte-identical footage"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := service.ScanLibraryRoot(ctx, root.ID)
	if err != nil || len(first.ChangedAssetIDs) != 1 {
		t.Fatalf("first scan result=%+v err=%v", first, err)
	}
	assetID := first.ChangedAssetIDs[0]
	if err := service.pipeline.EnqueueAsset(ctx, assetID); err != nil {
		t.Fatal(err)
	}
	loc, err := repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	probeHash := ingest.StableAssetKey(loc.QuickFingerprint, loc.FileSize, loc.ProbeModifiedNS)
	// Commit one analysis through the real model-run boundary before rescanning.
	runID, _, err := repo.CreateModelRun(ctx, assetID, "video_analysis", "fixture", "fixture-model", "analysis-input", "prompt", "schema", "{}", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, runID, "{}", `{}`, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitAnalysis(ctx, assetID, runID, "schema", domain.StructuredAnalysis{AssetType: "b_roll", ShotSize: "wide", Summary: "fixture"}, "", ""); err != nil {
		t.Fatal(err)
	}

	copyPath := filepath.Join(rootDir, "copy.mp4")
	bytes, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	newMtime := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(copyPath, newMtime, newMtime); err != nil {
		t.Fatal(err)
	}
	second, err := service.ScanLibraryRoot(ctx, root.ID)
	if err != nil || len(second.ChangedAssetIDs) != 1 || second.ChangedAssetIDs[0] != assetID {
		t.Fatalf("copy scan result=%+v err=%v, want original asset %q", second, err, assetID)
	}
	if err := service.pipeline.EnqueueAsset(ctx, assetID); err != nil {
		t.Fatal(err)
	}
	var jobs, runs int
	if err := repo.DB().QueryRow(`SELECT COUNT(*) FROM jobs WHERE asset_id=? AND job_type=?`, assetID, string(domain.JobProbe)).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := repo.DB().QueryRow(`SELECT COUNT(*) FROM model_runs WHERE asset_id=?`, assetID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || runs != 1 {
		t.Fatalf("after duplicate copy probe jobs=%d model_runs=%d, want 1/1; hash=%q", jobs, runs, probeHash)
	}
}
