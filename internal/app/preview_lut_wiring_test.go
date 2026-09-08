package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

// newPreviewLUTService builds the smallest real Hub whose pipeline field can be
// inspected. It mirrors newSupervisedLibrary (library_supervisor_test.go) with
// one difference: the caller controls cfg.PreviewLUTPath, which is the value
// under test. The repository is the real one because NewService is the code
// path being exercised; nothing here asserts on SQL.
func newPreviewLUTService(t *testing.T, previewLUTPath string) *Service {
	t.Helper()
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "preview-lut.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, config.Config{
		DataDir:        dir,
		CacheDir:       filepath.Join(dir, "cache"),
		Hardware:       media.HardwareConfig{Mode: "software", AllowFallback: true},
		PreviewLUTPath: previewLUTPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// The Hub's derive stage renders every preview itself, so cfg.PreviewLUTPath is
// only useful if NewService hands it to the Pipeline. Nothing else checks that:
// deleting .WithPreviewLUT(cfg.PreviewLUTPath) from service.go compiles and
// leaves every other test green, while an Apple Log install silently gets no
// thumbnail, no proxy and no audio track -- derive fails terminally and
// speech_gate is never queued. This is the test that makes that deletion red.
func TestNewServiceCarriesPreviewLUTPathIntoPipeline(t *testing.T) {
	const lutPath = "/srv/luts/apple-log.cube"
	service := newPreviewLUTService(t, lutPath)
	if service.pipeline == nil {
		t.Fatal("Service.pipeline is nil: no pipeline exists to carry the preview LUT to the derive stage")
	}
	if got := service.pipeline.previewLUTPath; got != lutPath {
		t.Fatalf("pipeline.previewLUTPath = %q, want %q\nNewService must pass cfg.PreviewLUTPath to NewPipeline(...).WithPreviewLUT; without that wiring the configured LUT never reaches the derive stage and Apple Log assets fail every preview render", got, lutPath)
	}
}

// Reverse control: the assertion above must read the configured value, not a
// constant. An install with no LUT configured must produce an empty field.
func TestNewServiceLeavesPreviewLUTPathEmptyWhenUnconfigured(t *testing.T) {
	service := newPreviewLUTService(t, "")
	if service.pipeline == nil {
		t.Fatal("Service.pipeline is nil: no pipeline exists to inspect")
	}
	if got := service.pipeline.previewLUTPath; got != "" {
		t.Fatalf("pipeline.previewLUTPath = %q, want empty when cfg.PreviewLUTPath is empty; the field must mirror configuration instead of holding a hard-coded value", got)
	}
}

// Source guard for the derive-stage call site. Reverting it to
// media.NewPreviewRenderer("") compiles, keeps every behaviour test green, and
// permanently disables the LUT: the renderer would never be handed the path the
// Pipeline carries, so plan resolution for Apple Log footage would always fail.
// This is the same class of guard as internal/api's page-anchor checks.
func TestDeriveStageHandsPipelinePreviewLUTToRenderer(t *testing.T) {
	source, err := os.ReadFile("pipeline.go")
	if err != nil {
		t.Fatalf("read pipeline.go: %v", err)
	}
	body := string(source)
	if !strings.Contains(body, "media.NewPreviewRenderer(p.previewLUTPath)") {
		t.Fatal("pipeline.go no longer contains media.NewPreviewRenderer(p.previewLUTPath): the derive stage must hand the Pipeline's configured LUT path to the renderer, otherwise cfg.PreviewLUTPath is dead configuration and Apple Log assets fail to render previews")
	}
	if strings.Contains(body, `media.NewPreviewRenderer("")`) {
		t.Fatal(`pipeline.go passes the empty string to media.NewPreviewRenderer: the derive stage must hand the Pipeline's configured LUT path to the renderer, otherwise cfg.PreviewLUTPath is dead configuration and Apple Log assets fail to render previews`)
	}
}
