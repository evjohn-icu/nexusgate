package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

// TestDeriveFiresCompletionHookForSilentAsset pins the call site that made
// silent visual imports depend on a later multimodal-settings save. The hook
// used to live only inside the HasAudio branch, so a video with no audio track
// finished derive without enqueueing detect_shots; the settings backfill was
// the only path that ever created visual vectors for it.
func TestDeriveFiresCompletionHookForSilentAsset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dir, "silent-derive.db"))
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
	source := filepath.Join(footage, "silent.mov")
	if err := os.WriteFile(source, []byte("silent footage placeholder"), 0o600); err != nil {
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
	scanned, err := repo.UpsertScannedFile(ctx, root, "silent.mov", source, info, "fp-silent")
	if err != nil {
		t.Fatal(err)
	}
	assetID := scanned.AssetID
	if err := repo.SaveMediaMetadata(ctx, assetID, domain.MediaMetadata{DurationMS: 4000, HasAudio: false}, "test", "", ""); err != nil {
		t.Fatal(err)
	}

	// The derive stage skips rendering when these files exist; the stand-ins
	// keep the test independent of ffmpeg while still reaching the hook.
	cacheDir := filepath.Join(dir, "cache")
	assetDir := filepath.Join(cacheDir, assetID)
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"thumbnail-software.jpg", "proxy-software.mp4"} {
		if err := os.WriteFile(filepath.Join(assetDir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	pipeline := NewPipeline(repo, cacheDir, nil, nil, &recordingVideoProvider{}, nil, nil, media.HardwarePlan{Mode: "software"}, nil, providerRouteDeferral, 0)
	var calls int
	var hookAssetID, hookInputHash string
	pipeline.SetAfterDeriveCompleted(func(_ context.Context, assetID, inputHash string) {
		calls++
		hookAssetID = assetID
		hookInputHash = inputHash
	})

	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "silent-input", 50); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("derive completion hook calls = %d, want 1 for a silent asset; the hook must run outside the HasAudio branch", calls)
	}
	if hookAssetID != assetID || hookInputHash != "silent-input" {
		t.Fatalf("hook got asset=%q input=%q, want %q / silent-input", hookAssetID, hookInputHash, assetID)
	}
}
