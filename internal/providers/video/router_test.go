package video

import (
	"context"
	"errors"
	"testing"

	videoanalysis "github.com/evjohn-icu/nexusgate/internal/domain/video_analysis"
)

type stubProvider struct {
	name string
	err  error
	seen videoanalysis.Input
}

func (p stubProvider) Name() string               { return p.name }
func (p stubProvider) Model() string              { return p.name + "-model" }
func (p stubProvider) Capabilities() []Capability { return []Capability{CapabilityVideoAnalysis} }
func (p *stubProvider) Analyze(_ context.Context, input videoanalysis.Input) (videoanalysis.Result, string, error) {
	p.seen = input
	if p.err != nil {
		return videoanalysis.Result{}, "raw-" + p.name, p.err
	}
	return videoanalysis.Result{Summary: p.name}, "raw-" + p.name, nil
}

func TestRouterFallsBackAfterProviderFailure(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&stubProvider{name: "primary", err: errors.New("primary unavailable")})
	registry.Register(&stubProvider{name: "fallback"})
	router, err := NewRouter(registry, "primary", []string{"fallback"})
	if err != nil {
		t.Fatal(err)
	}

	got, raw, err := router.Analyze(context.Background(), videoanalysis.Input{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "fallback" || raw != "raw-fallback" {
		t.Fatalf("router did not return fallback result: got=%+v raw=%q", got, raw)
	}
}

func TestRouterClearsProviderSpecificRemoteURIForNonPreparingFallback(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&stubProvider{name: "gemini", err: errors.New("Gemini unavailable")})
	fallback := &stubProvider{name: "qwen_video"}
	registry.Register(fallback)
	router, err := NewRouter(registry, "gemini", []string{"qwen_video"})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = router.Analyze(context.Background(), videoanalysis.Input{VideoPath: "/tmp/proxy.mp4", RemoteURI: "files/gemini-upload", MIMEType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if fallback.seen.RemoteURI != "" {
		t.Fatalf("fallback received provider-specific remote URI: %+v", fallback.seen)
	}
	if fallback.seen.VideoPath != "/tmp/proxy.mp4" {
		t.Fatalf("fallback lost portable video path: %+v", fallback.seen)
	}
}

func TestRouterOnlyRequiresPreparationWhenPrimaryProviderSupportsIt(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&stubProvider{name: "qwen_video"})
	router, err := NewRouter(registry, "qwen_video", nil)
	if err != nil {
		t.Fatal(err)
	}
	if router.RequiresVideoPreparation() {
		t.Fatal("OpenAI-compatible provider should use its local input directly")
	}
}

func TestNewRouterRejectsUnknownProvider(t *testing.T) {
	_, err := NewRouter(NewRegistry(), "missing", nil)
	if err == nil {
		t.Fatal("expected unknown provider error")
	}
}
