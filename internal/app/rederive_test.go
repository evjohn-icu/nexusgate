package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// TestRederiveSkipsPaidChainForCommittedAsset pins the recovery contract of
// `cache gc --rebuildable`: the CLI enqueues a JobDerive for each affected
// committed asset, and that re-derive must re-create the files WITHOUT
// re-enqueueing speech gate / transcribe / analyze — a cache clean-up must
// never re-bill a model run. The committed state is produced by a real first
// pass (fake video provider), then a re-derive job runs to completion and the
// job inventory must show exactly one analyze job total.
func TestRederiveSkipsPaidChainForCommittedAsset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dir, "pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	footage := filepath.Join(dir, "footage")
	if err := os.MkdirAll(footage, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(footage, "clip.mov")
	if err := os.WriteFile(source, []byte("footage"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, footage)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := repo.UpsertScannedFile(ctx, root, "clip.mov", source, info, "fp-rederive")
	if err != nil {
		t.Fatal(err)
	}
	assetID := scanned.AssetID
	if err := repo.SaveMediaMetadata(ctx, assetID, domain.MediaMetadata{DurationMS: 4000, HasAudio: false}, "test"); err != nil {
		t.Fatal(err)
	}

	// The derive stage's os.IsNotExist guard skips rendering when the files
	// are present, so the test needs no ffmpeg; the files are tiny stand-ins.
	cacheDir := filepath.Join(dir, "cache")
	assetDir := filepath.Join(cacheDir, assetID)
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	thumb := filepath.Join(assetDir, "thumbnail-software.jpg")
	proxy := filepath.Join(assetDir, "proxy-software.mp4")
	if err := os.WriteFile(thumb, []byte("thumb"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxy, []byte("proxy"), 0o600); err != nil {
		t.Fatal(err)
	}

	hardware := media.HardwarePlan{Mode: "software"}
	pipeline := NewPipeline(repo, cacheDir, nil, nil, &recordingVideoProvider{}, nil, nil, hardware, nil, providerRouteDeferral, 0)

	// First pass: derive (files present, no ffmpeg) then analyze commits.
	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "seed-derive", 50); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}
	committed, err := repo.HasCommittedAnalysis(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("first pass must commit the analysis")
	}

	// Simulate `cache gc --rebuildable`'s recovery half.
	enqueued, err := repo.EnqueueRederive(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if !enqueued {
		t.Fatal("a committed asset must be re-derived")
	}
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}

	jobs, err := repo.ListJobs(ctx, 20)
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	var (
		derives, analyzes, transcribes, speechGates int
		rederiveFailed                              bool
	)
	for _, j := range jobs {
		switch j.Type {
		case domain.JobDerive:
			derives++
			if j.State != domain.JobSucceeded {
				rederiveFailed = true
			}
		case domain.JobAnalyze:
			analyzes++
		case domain.JobTranscribe:
			transcribes++
		case domain.JobSpeechGate:
			speechGates++
		}
	}
	if rederiveFailed {
		t.Fatalf("re-derive must succeed: %+v", jobs)
	}
	if derives != 2 {
		t.Fatalf("derive jobs = %d, want 2 (original + re-derive): %+v", derives, jobs)
	}
	if analyzes != 1 {
		t.Fatalf("analyze jobs = %d, want exactly 1: a re-derive must not re-bill analysis: %+v", analyzes, jobs)
	}
	if transcribes != 0 || speechGates != 0 {
		t.Fatalf("re-derive must not enqueue the paid chain: transcribes=%d speech_gates=%d", transcribes, speechGates)
	}

	// The artifacts are still canonical after the re-derive: rows point at the
	// re-saved files.
	proxyArtifact, err := repo.GetArtifact(ctx, assetID, "proxy")
	if err != nil {
		t.Fatal(err)
	}
	if proxyArtifact == nil || proxyArtifact.LocalPath != proxy {
		t.Fatalf("proxy artifact not re-saved at its path: %+v", proxyArtifact)
	}
	if _, err := os.Stat(proxy); err != nil {
		t.Fatalf("proxy file missing after re-derive: %v", err)
	}
}
