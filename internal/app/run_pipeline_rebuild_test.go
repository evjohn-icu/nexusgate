package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/media"
	"github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
)

// countingRebuildRepo is the real repository with only the session rebuild
// overridden to count calls. What these tests assert is *whether* a rebuild
// happened on a given pass — the decision in RunPipeline — so the rebuild
// itself, like every other repository behaviour they rely on, must be the real
// SQL.
type countingRebuildRepo struct {
	*sqlite.Repository
	rebuildCalls int
}

func (r *countingRebuildRepo) RebuildAutomaticShootSessions(context.Context, string) error {
	r.rebuildCalls++
	return nil
}

// newCountingRebuildService builds a Hub with one library root and no queued
// jobs, and wraps the repository so RunPipeline can be observed without doing
// any real re-derivation. rootDir is where an asset fixture goes when a test
// needs the pipeline to actually work the queue.
func newCountingRebuildService(t *testing.T) (*Service, *countingRebuildRepo, string) {
	t.Helper()
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "rebuild.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Join(dir, "footage")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateLibraryRoot(ctx, rootDir); err != nil {
		t.Fatal(err)
	}
	counted := &countingRebuildRepo{Repository: repo}
	service, err := NewService(counted, config.Config{DataDir: dir, CacheDir: filepath.Join(dir, "cache"), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service, counted, rootDir
}

// An idle pass changes no asset, and sessions are derived from assets, so a
// rebuild has nothing to pick up. Running it anyway is a full re-derivation per
// root four times an hour on an idle library. The repo has one root on purpose:
// without the skip, the loop would call the rebuild once here.
func TestRunPipelineSkipsSessionRebuildOnAnIdlePass(t *testing.T) {
	service, repo, _ := newCountingRebuildService(t)
	if err := service.RunPipeline(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.rebuildCalls != 0 {
		t.Fatalf("an idle pass must not rebuild shoot sessions: called %d times", repo.rebuildCalls)
	}
}

// The skip is for idle passes only: a pass that actually worked the queue must
// still rebuild, or newly indexed assets would never get their sessions.
func TestRunPipelineRebuildsSessionsAfterWorkWasDone(t *testing.T) {
	service, repo, rootDir := newCountingRebuildService(t)
	ctx := context.Background()
	source := filepath.Join(rootDir, "clip.mov")
	if err := os.WriteFile(source, []byte("footage"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := repo.ListLibraryRoots(ctx)
	if err != nil || len(roots) != 1 {
		t.Fatalf("roots=%+v err=%v", roots, err)
	}
	if _, err := repo.UpsertScannedFile(ctx, roots[0], "clip.mov", source, info, "fp-clip"); err != nil {
		t.Fatal(err)
	}
	assets, err := repo.ListAssets(ctx, 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	// Index is the cheapest stage that runs to completion without FFmpeg on
	// PATH, so the pass has real work to count without touching media.
	if err := repo.EnqueueJob(ctx, assets[0].ID, domain.JobIndex, "index-input", 10); err != nil {
		t.Fatal(err)
	}
	if err := service.RunPipeline(ctx); err != nil {
		t.Fatal(err)
	}
	if repo.rebuildCalls != 1 {
		t.Fatalf("a pass that ran a job must rebuild sessions once: called %d times", repo.rebuildCalls)
	}
}
