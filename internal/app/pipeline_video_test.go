package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// TestServiceWiringExposesMultiframeRouteThroughTheBridge pins the wiring
// regression: the pipeline used to receive the channel runtime's bare
// wrapper, which implements no MultiframeAnalyzer/VideoAnalyzer surface, so
// multiframeRouteOf resolved to nil in production and the multiframe
// orchestration never ran through the real Hub/CLI — a frame-only provider
// failed with "does not support video preparation" instead of entering
// detector mode. Only the app-level tests that constructed NewPipeline with
// a Router directly were green. The wiring must hand the pipeline the
// pipelineVideo bridge (channel wrapper + legacy router).
func TestServiceWiringExposesMultiframeRouteThroughTheBridge(t *testing.T) {
	dir := secureDataDir(t)
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(dir, "wiring.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		DataDir: dir, CacheDir: filepath.Join(dir, "cache"),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
		Providers: config.ProvidersConfig{
			VisionPrimary: "local_vlm",
			LocalVLM: config.ProviderConfig{
				Enabled: true, Protocol: "openai_multiframe",
				BaseURL: "http://127.0.0.1:19090/v1", Path: "chat/completions", Model: "Qwen3-VL-4B-Instruct",
			},
			ShotDetection: config.ShotDetectionConfig{Enabled: true, Mode: "ffmpeg_scene"},
		},
	}
	service, err := NewService(repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bridge, ok := service.pipeline.videoProvider.(*pipelineVideo)
	if !ok {
		t.Fatalf("pipeline video provider is %T, want *pipelineVideo (channel wrapper + legacy router)", service.pipeline.videoProvider)
	}
	if bridge.router == nil || bridge.channel == nil {
		t.Fatalf("bridge surfaces are incomplete: router=%v channel=%v", bridge.router != nil, bridge.channel != nil)
	}
	route := service.pipeline.multiframeRouteOf()
	if route == nil {
		t.Fatal("multiframeRouteOf resolved nil through the production wiring: detector mode can never run")
	}
	if route.analyzer == nil {
		t.Fatal("route carries no multiframe analyzer")
	}
	if route.video == nil {
		t.Fatal("route carries no video fallback for two-pass")
	}
}

// TestServiceWiringKeepsPlainPathWithoutMultiframeProvider is the control:
// a whole-video vision provider (gemini) must resolve no multiframe route, so
// the plain analyze path — the channel wrapper — is what the pipeline uses.
func TestServiceWiringKeepsPlainPathWithoutMultiframeProvider(t *testing.T) {
	dir := secureDataDir(t)
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(dir, "wiring.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		DataDir: dir, CacheDir: filepath.Join(dir, "cache"),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
		Providers: config.ProvidersConfig{
			VisionPrimary: "gemini",
			Gemini: config.ProviderConfig{
				Enabled: true, Protocol: "gemini_generate_content",
				BaseURL: "https://generativelanguage.googleapis.com/v1beta", Model: "gemini-2.5-flash",
			},
		},
	}
	service, err := NewService(repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if route := service.pipeline.multiframeRouteOf(); route != nil {
		t.Fatalf("gemini wiring resolved a multiframe route (%+v), want nil", route)
	}
	if _, ok := service.pipeline.videoProvider.(*pipelineVideo); !ok {
		t.Fatalf("pipeline video provider is %T, want *pipelineVideo", service.pipeline.videoProvider)
	}
}
