package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// newSetupStatusService builds a Service over a real, migrated SQLite
// repository — the NextStep progression is a claim about what the database
// actually reports, so the fake-repo pattern would prove nothing here — with a
// temp cache dir the disk probes can touch.
func newSetupStatusService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "setup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, config.Config{DataDir: dir, CacheDir: cacheDir, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestSetupStatusFreshHubGuidesAddFootage(t *testing.T) {
	service := newSetupStatusService(t)
	status := service.SetupStatus(context.Background())
	if status.NextStep != SetupNextStepAddFootage {
		t.Fatalf("fresh hub: NextStep = %q, want %q", status.NextStep, SetupNextStepAddFootage)
	}
	if status.Ready {
		t.Fatal("fresh hub must not report ready")
	}
	if status.RootCount != 0 || status.ProviderCount != 0 || status.AssetCount != 0 {
		t.Fatalf("fresh hub: counts = roots %d providers %d assets %d, want all zero", status.RootCount, status.ProviderCount, status.AssetCount)
	}
	if !status.DataDirWritable || !status.CacheWritable {
		t.Fatalf("temp dirs must be writable: data_dir_writable=%v cache_writable=%v", status.DataDirWritable, status.CacheWritable)
	}
	if status.FreeDiskBytes <= 0 {
		t.Fatalf("free disk on the cache dir must be measurable and positive, got %d", status.FreeDiskBytes)
	}
	if !status.DBHealthy {
		t.Fatal("a freshly migrated database must pass integrity check")
	}
}

func TestSetupStatusRootThenProvidersAdvanceNextStep(t *testing.T) {
	service := newSetupStatusService(t)
	ctx := context.Background()
	rootDir := filepath.Join(t.TempDir(), "footage")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := service.repo.CreateLibraryRoot(ctx, rootDir); err != nil {
		t.Fatal(err)
	}
	status := service.SetupStatus(ctx)
	if status.RootCount != 1 {
		t.Fatalf("RootCount = %d, want 1", status.RootCount)
	}
	if status.NextStep != SetupNextStepConfigureProviders {
		t.Fatalf("root without providers: NextStep = %q, want %q", status.NextStep, SetupNextStepConfigureProviders)
	}
	if status.Ready {
		t.Fatal("a root with no provider must not report ready")
	}
}

// The binary booleans must track what exec.LookPath actually finds on this
// machine — a CI box without ffmpeg must see FFmpeg=false, one with it true.
func TestSetupStatusBinariesMatchLookPath(t *testing.T) {
	service := newSetupStatusService(t)
	status := service.SetupStatus(context.Background())
	for _, tc := range []struct {
		name  string
		got   bool
		field string
	}{
		{"ffmpeg", status.FFmpeg, "FFmpeg"},
		{"ffprobe", status.FFprobe, "FFprobe"},
		{"exiftool", status.ExifTool, "ExifTool"},
	} {
		_, err := exec.LookPath(tc.name)
		if tc.got != (err == nil) {
			t.Fatalf("%s = %v, but exec.LookPath(%q) error is %v", tc.field, tc.got, tc.name, err)
		}
	}
}
