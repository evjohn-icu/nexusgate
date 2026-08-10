package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	sqliterepo "github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

func TestCacheRepairDerivedDryRunPreservesRowsFilesAndJobs(t *testing.T) {
	fx := newRepairDerivedFixture(t)
	if err := runCacheRepairDerived(context.Background(), fx.repo, fx.cfg, []string{"--invalidate-hardware-profiles"}); err != nil {
		t.Fatal(err)
	}
	fx.assertHardwarePresent(t)
	jobs, err := fx.repo.ListJobs(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("dry run jobs=%d, want 0", len(jobs))
	}
}

func TestCacheRepairDerivedYesRemovesHardwarePreservesSoftwareAndEnqueues(t *testing.T) {
	fx := newRepairDerivedFixture(t)
	if err := runCacheRepairDerived(context.Background(), fx.repo, fx.cfg, []string{"--invalidate-hardware-profiles", "--yes"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := fx.repo.GetArtifact(ctx, fx.assetID, "thumbnail"); err != nil {
		t.Fatal(err)
	}
	thumb, err := fx.repo.GetArtifact(ctx, fx.assetID, "thumbnail")
	if err != nil {
		t.Fatal(err)
	}
	if thumb == nil || thumb.ProfileHash != "thumb-software-h264-x264-v1" {
		t.Fatalf("thumbnail=%+v", thumb)
	}
	proxy, err := fx.repo.GetArtifact(ctx, fx.assetID, "proxy")
	if err != nil {
		t.Fatal(err)
	}
	if proxy == nil || proxy.ProfileHash != "proxy-720-software-h264-x264-v1" {
		t.Fatalf("proxy=%+v", proxy)
	}
	if _, err := fx.repo.GetArtifact(ctx, fx.assetID, "audio"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fx.softwareThumb, fx.softwareProxy, fx.audio, fx.source} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("preserved file %s: %v", path, err)
		}
	}
	for _, path := range []string{fx.hardwareThumb, fx.hardwareProxy} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("hardware file %s: %v", path, err)
		}
	}
	jobs, err := fx.repo.ListJobs(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Type != domain.JobDerive {
		t.Fatalf("jobs=%+v, want one derive", jobs)
	}
}

type repairDerivedFixture struct {
	repo                                                                               *sqliterepo.Repository
	cfg                                                                                config.Config
	assetID, source, hardwareThumb, hardwareProxy, softwareThumb, softwareProxy, audio string
}

func newRepairDerivedFixture(t *testing.T) repairDerivedFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := sqliterepo.Open(filepath.Join(dir, "timingdex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Join(dir, "footage")
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(rootDir, "clip.mp4")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, rootDir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := repo.UpsertScannedFile(ctx, root, "clip.mp4", source, info, "repair")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := repo.SaveMediaMetadata(ctx, asset.AssetID, domain.MediaMetadata{DurationMS: 1000}, "test"); err != nil {
		t.Fatal(err)
	}
	runID, _, err := repo.CreateModelRun(ctx, asset.AssetID, "analysis", "hash", "provider", "model", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StageModelRun(ctx, runID, `{}`, `{"summary":"committed"}`); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitAnalysis(ctx, asset.AssetID, runID, "v1", domain.StructuredAnalysis{Summary: "committed"}); err != nil {
		t.Fatal(err)
	}
	assetDir := filepath.Join(dir, "cache", asset.AssetID)
	if err := os.MkdirAll(filepath.Join(dir, "cache", "sources"), 0o700); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(assetDir, "thumbnail-cuda.jpg"), filepath.Join(assetDir, "proxy-cuda.mp4"), filepath.Join(assetDir, "thumbnail-software.jpg"), filepath.Join(assetDir, "proxy-software.mp4"), filepath.Join(assetDir, "audio.m4a")}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("artifact"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staged := filepath.Join(dir, "cache", "sources", asset.AssetID, "1.mp4")
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, row := range []struct{ typ, profile, path string }{{"thumbnail", "thumb-hw-cuda-h264-v1", paths[0]}, {"proxy", "proxy-720-hw-cuda-h264-v1", paths[1]}, {"thumbnail", "thumb-software-h264-x264-v1", paths[2]}, {"proxy", "proxy-720-software-h264-x264-v1", paths[3]}, {"audio", "audio-16k-v1", paths[4]}} {
		if _, err := repo.DB().ExecContext(ctx, `INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) VALUES(?,?,?,?,?,?,?)`, filepath.Base(paths[i]), asset.AssetID, row.typ, row.profile, row.path, 8, now); err != nil {
			t.Fatal(err)
		}
	}
	return repairDerivedFixture{repo: repo, cfg: config.Config{CacheDir: filepath.Join(dir, "cache"), DataDir: dir}, assetID: asset.AssetID, source: source, hardwareThumb: paths[0], hardwareProxy: paths[1], softwareThumb: paths[2], softwareProxy: paths[3], audio: paths[4]}
}

func (fx repairDerivedFixture) assertHardwarePresent(t *testing.T) {
	t.Helper()
	for _, path := range []string{fx.hardwareThumb, fx.hardwareProxy} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	for _, typ := range []string{"thumbnail", "proxy"} {
		artifact, err := fx.repo.GetArtifact(context.Background(), fx.assetID, typ)
		if err != nil {
			t.Fatal(err)
		}
		if artifact == nil {
			t.Fatalf("missing %s row", typ)
		}
	}
}
