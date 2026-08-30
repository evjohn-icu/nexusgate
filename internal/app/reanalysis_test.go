package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
	"github.com/evjohn-icu/timingdex/internal/media"
	videoproviders "github.com/evjohn-icu/timingdex/internal/providers/video"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

type failIndexEnqueueOnceRepo struct {
	*sqlite.Repository
	failed bool
}

func (r *failIndexEnqueueOnceRepo) EnqueueJob(ctx context.Context, assetID string, typ domain.JobType, inputHash string, priority int) error {
	if typ == domain.JobIndex && !r.failed {
		r.failed = true
		return errors.New("injected index enqueue failure")
	}
	return r.Repository.EnqueueJob(ctx, assetID, typ, inputHash, priority)
}

// reanalysisVideoProvider answers analyze and tags its response so the test
// can tell which run's shots landed in the canonical tables.
type reanalysisVideoProvider struct {
	label string
}

func (f *reanalysisVideoProvider) Name() string  { return "fixture-video" }
func (f *reanalysisVideoProvider) Model() string { return "fixture-v" }
func (f *reanalysisVideoProvider) Capabilities() []videoproviders.Capability {
	return []videoproviders.Capability{videoproviders.CapabilityVideoAnalysis}
}
func (f *reanalysisVideoProvider) Analyze(_ context.Context, _ videoanalysis.Input) (videoanalysis.Result, string, error) {
	return videoanalysis.Result{
		Summary: "reanalysis fixture " + f.label,
		Shots:   []videoanalysis.Shot{{StartMS: 0, EndMS: 5_000, Description: "shot from " + f.label}},
	}, `{"label":"` + f.label + `"}`, nil
}

// seedAnalyzeReadyAsset builds an asset that can run the analyze stage:
// metadata, proxy artifact (through a real derive lease) and a transcript.
func seedAnalyzeReadyAsset(t *testing.T, repo *sqlite.Repository, root domain.LibraryRoot, id string) string {
	t.Helper()
	ctx := context.Background()
	proxyPath := filepath.Join(root.Path, id+".mov")
	if err := os.WriteFile(proxyPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(proxyPath)
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := repo.UpsertScannedFile(ctx, root, id+".mov", proxyPath, info, "fp-"+id)
	if err != nil {
		t.Fatal(err)
	}
	assetID := scanned.AssetID
	if err := repo.SaveMediaMetadata(ctx, assetID, domain.MediaMetadata{DurationMS: 120_000}, "test", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "seed-"+id, 50); err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, "seed", nil, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease: job=%+v err=%v", job, err)
	}
	if err := repo.SaveArtifact(ctx, domain.DerivedArtifact{ID: "art-" + id, AssetID: assetID, Type: "proxy", ProfileHash: "p1", LocalPath: proxyPath, SizeBytes: info.Size()}, job.ID, "seed"); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteJob(ctx, job.ID, "seed", domain.JobSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTranscript(ctx, assetID, "qwen", "qwen3-asr-flash", "thash-"+id, domain.Transcript{Language: "zh", Text: "text " + id}, "", ""); err != nil {
		t.Fatal(err)
	}
	return assetID
}

// The reanalysis contract: a forced analyze re-run produces a NEW model run
// and switches the canonical shots to it, while the old run stays in
// model_runs for audit. Without the nonce the succeeded job's input-hash
// dedup would swallow the re-run entirely.
func TestReanalysisProducesNewCanonicalRunKeepsOldAuditable(t *testing.T) {
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "reanalysis.db"))
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
	root, err := repo.CreateLibraryRoot(ctx, footage)
	if err != nil {
		t.Fatal(err)
	}
	assetID := seedAnalyzeReadyAsset(t, repo, root, "clip-a")

	first := &reanalysisVideoProvider{label: "run-1"}
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, first, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)
	if err := repo.EnqueueJob(ctx, assetID, domain.JobAnalyze, hashStrings("first", "analyze"), 30); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}
	assertSingleIndexJob(t, repo, assetID, hashStrings("first", "analyze"))
	shots, err := repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 1 || shots[0].SourceRunID == "" {
		t.Fatalf("first run committed no canonical shots: %+v", shots)
	}
	firstRun := shots[0].SourceRunID

	second := &reanalysisVideoProvider{label: "run-2"}
	pipeline = NewPipeline(repo, t.TempDir(), nil, nil, second, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)
	service, err := NewService(repo, config.Config{DataDir: dir, CacheDir: filepath.Join(dir, "cache"), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := service.ReanalyzeAssets(ctx, []string{assetID}, "prompt v4 migration")
	if err != nil {
		t.Fatal(err)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1", enqueued)
	}
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, err := repo.ListJobs(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var indexHashes []string
	for _, job := range jobs {
		if job.AssetID == assetID && job.Type == domain.JobIndex {
			indexHashes = append(indexHashes, job.InputHash)
		}
	}
	if len(indexHashes) != 2 || indexHashes[0] == indexHashes[1] {
		t.Fatalf("reanalysis index hashes = %v, want two distinct hashes", indexHashes)
	}
	shots, err = repo.ListAssetShots(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 1 {
		t.Fatalf("reanalysis must leave exactly one canonical shot per run, got %+v", shots)
	}
	if shots[0].SourceRunID == firstRun {
		t.Fatalf("canonical analysis did not switch to the new run: %s", shots[0].SourceRunID)
	}
	// Search must be rebuilt against the new run's shots.
	hits, err := repo.SearchShots(ctx, "run-2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("search not rebuilt for reanalysis: %+v", hits)
	}
}

func TestCachedAnalysisStillEnqueuesIndex(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dir, "cached-analysis.db"))
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
	root, err := repo.CreateLibraryRoot(ctx, footage)
	if err != nil {
		t.Fatal(err)
	}
	assetID := seedAnalyzeReadyAsset(t, repo, root, "cached")
	analyzeHash := hashStrings("cached", "analyze")
	runID, cached, err := repo.CreateModelRun(ctx, assetID, "vision", "fixture-video", "fixture-v", analyzeHash, "footage-analysis-v4", "asset-analysis/v2", `{}`, "", "")
	if err != nil || cached {
		t.Fatalf("seed run id=%q cached=%v err=%v", runID, cached, err)
	}
	if err := repo.StageModelRun(ctx, runID, `{}`, `{"summary":"cached"}`, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitAnalysisWithShots(ctx, assetID, runID, "asset-analysis/v2", domain.StructuredAnalysis{Summary: "cached"}, []domain.AssetShot{{StartMS: 0, EndMS: 1000, Description: "cached shot"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, assetID, domain.JobAnalyze, analyzeHash, 30); err != nil {
		t.Fatal(err)
	}
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, &reanalysisVideoProvider{label: "unused"}, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}
	assertSingleIndexJob(t, repo, assetID, analyzeHash)
}

func TestIndexEnqueueRetryDeduplicatesSuccessor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dir, "index-retry.db"))
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
	root, err := repo.CreateLibraryRoot(ctx, footage)
	if err != nil {
		t.Fatal(err)
	}
	assetID := seedAnalyzeReadyAsset(t, repo, root, "index-retry")
	analyzeHash := hashStrings("retry", "analyze")
	wrapped := &failIndexEnqueueOnceRepo{Repository: repo}
	if err := repo.EnqueueJob(ctx, assetID, domain.JobAnalyze, analyzeHash, 30); err != nil {
		t.Fatal(err)
	}
	pipeline := NewPipeline(wrapped, t.TempDir(), nil, nil, &reanalysisVideoProvider{label: "retry"}, nil, nil, media.HardwarePlan{}, nil, providerRouteDeferral, 0)
	for i := 0; i < 20; i++ {
		if _, err := pipeline.RunUntilIdle(ctx); err != nil {
			t.Fatal(err)
		}
		jobs, err := repo.ListJobs(ctx, 20)
		if err != nil {
			t.Fatal(err)
		}
		ready := true
		for _, job := range jobs {
			if job.AssetID == assetID && (job.State != domain.JobSucceeded && (job.Type == domain.JobAnalyze || job.Type == domain.JobIndex)) {
				ready = false
			}
		}
		if ready {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	assertSingleIndexJob(t, repo, assetID, analyzeHash)
}

// Selector resolution: --asset validates existence, --root resolves every
// asset under the root, --all covers everything, and conflicting selectors
// are rejected.
func TestResolveReanalysisAssets(t *testing.T) {
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "resolver.db"))
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
	root, err := repo.CreateLibraryRoot(ctx, footage)
	if err != nil {
		t.Fatal(err)
	}
	assetA := seedAnalyzeReadyAsset(t, repo, root, "clip-a")
	seedAnalyzeReadyAsset(t, repo, root, "clip-b")
	service, err := NewService(repo, config.Config{DataDir: dir, CacheDir: filepath.Join(dir, "cache"), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}

	got, err := service.ResolveReanalysisAssets(ctx, ReanalysisSelector{AssetID: assetA, Reason: "r"})
	if err != nil || len(got) != 1 || got[0] != assetA {
		t.Fatalf("asset selector: got=%v err=%v", got, err)
	}
	got, err = service.ResolveReanalysisAssets(ctx, ReanalysisSelector{RootID: root.ID, Reason: "r"})
	if err != nil || len(got) != 2 {
		t.Fatalf("root selector: got=%v err=%v", got, err)
	}
	got, err = service.ResolveReanalysisAssets(ctx, ReanalysisSelector{All: true, Reason: "r"})
	if err != nil || len(got) != 2 {
		t.Fatalf("all selector: got=%v err=%v", got, err)
	}
	if _, err := service.ResolveReanalysisAssets(ctx, ReanalysisSelector{AssetID: assetA, All: true}); err == nil {
		t.Fatal("conflicting selectors must be rejected")
	}
	if _, err := service.ResolveReanalysisAssets(ctx, ReanalysisSelector{AssetID: "missing"}); err == nil {
		t.Fatal("unknown asset must be rejected")
	}
	if _, err := service.ResolveReanalysisAssets(ctx, ReanalysisSelector{RootID: "missing"}); err == nil {
		t.Fatal("unknown root must be rejected")
	}
	if _, err := service.ResolveReanalysisAssets(ctx, ReanalysisSelector{Reason: "r"}); err == nil {
		t.Fatal("no selector must be rejected")
	}
}
