package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/staging"
)

func TestPipelineSourcePathStagesNetworkSourceBeforeMediaWork(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "mounted-nas-footage.mov")
	if err := os.WriteFile(sourcePath, []byte("remote footage"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	stager, err := staging.New("copy", cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := &Pipeline{sourceStager: stager}
	got, err := pipeline.sourcePath(context.Background(), domain.AssetLocation{AssetID: "asset-1", AbsolutePath: sourcePath, ModifiedNS: 42})
	if err != nil {
		t.Fatal(err)
	}
	if got == sourcePath {
		t.Fatalf("pipeline should use the local staged path, got source %q", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("staged pipeline source is unavailable: %v", err)
	}
}

func TestPipelineProbeUsesStagedNASSource(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "mounted-nas-footage.mov")
	if err := os.WriteFile(sourcePath, []byte("remote footage"), 0o600); err != nil {
		t.Fatal(err)
	}
	stager, err := staging.New("copy", filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	pipeline := &Pipeline{sourceStager: stager}
	got, err := pipeline.sourcePath(context.Background(), domain.AssetLocation{AssetID: "asset-1", AbsolutePath: sourcePath, ModifiedNS: 42})
	if err != nil {
		t.Fatal(err)
	}
	if got == sourcePath {
		t.Fatalf("probe must read the staged copy in copy mode, not the NAS path; got source %q", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("staged probe source is unavailable: %v", err)
	}
}
