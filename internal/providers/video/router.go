package video

import (
	"context"
	"errors"
	"fmt"
	"strings"

	videoanalysis "github.com/ev/timingdex/internal/domain/video_analysis"
)

type Router struct {
	registry *Registry
	chain    []string
}

func NewRouter(registry *Registry, primary string, fallbacks []string) (*Router, error) {
	if registry == nil {
		return nil, fmt.Errorf("video provider registry is nil")
	}
	if strings.TrimSpace(primary) == "" || primary == "none" {
		return nil, fmt.Errorf("video provider primary is required")
	}
	chain := make([]string, 0, len(fallbacks)+1)
	seen := make(map[string]struct{})
	for _, name := range append([]string{primary}, fallbacks...) {
		name = strings.TrimSpace(name)
		if name == "" || name == "none" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		if _, ok := registry.Get(name); !ok {
			return nil, fmt.Errorf("video provider %q is not registered", name)
		}
		seen[name] = struct{}{}
		chain = append(chain, name)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("video provider route is empty")
	}
	return &Router{registry: registry, chain: chain}, nil
}

func (r *Router) Name() string { return r.chain[0] }

func (r *Router) Model() string {
	provider, _ := r.registry.Get(r.chain[0])
	return provider.Model()
}

func (r *Router) Capabilities() []Capability {
	provider, _ := r.registry.Get(r.chain[0])
	return provider.Capabilities()
}

func (r *Router) Analyze(ctx context.Context, input videoanalysis.Input) (videoanalysis.Result, string, error) {
	var errs []error
	var lastRaw string
	for _, name := range r.chain {
		provider, _ := r.registry.Get(name)
		providerInput := input
		// A remote URI produced by a preparer (for example Gemini Files) is
		// provider-specific. Every provider still receives the portable local
		// path, so a non-preparing fallback must start from that input instead.
		if _, prepared := provider.(VideoPreparer); !prepared {
			providerInput.RemoteURI = ""
		}
		result, raw, err := provider.Analyze(ctx, providerInput)
		if err == nil {
			return result, raw, nil
		}
		if raw != "" {
			lastRaw = raw
		}
		errs = append(errs, fmt.Errorf("%s: %w", name, err))
	}
	if len(errs) == 0 {
		return videoanalysis.Result{}, lastRaw, fmt.Errorf("video provider route is empty")
	}
	return videoanalysis.Result{}, lastRaw, errors.Join(errs...)
}

// RequiresVideoPreparation reports whether the selected primary provider has
// an explicit remote-file preparation step. It keeps the router from making
// every provider look like a preparer merely because the router can delegate.
func (r *Router) RequiresVideoPreparation() bool {
	provider, ok := r.registry.Get(r.chain[0])
	if !ok {
		return false
	}
	_, ok = provider.(VideoPreparer)
	return ok
}

// MaxInlineVideoBytes forwards the primary provider's inline ceiling. The
// router itself has no opinion on request size; answering for the provider that
// will actually be called is what lets a caller size its windows correctly
// after a fallback changes the chain.
func (r *Router) MaxInlineVideoBytes() int64 {
	provider, ok := r.registry.Get(r.chain[0])
	if !ok {
		return 0
	}
	limiter, ok := provider.(InlineVideoLimiter)
	if !ok {
		return 0
	}
	return limiter.MaxInlineVideoBytes()
}

func (r *Router) PrepareVideo(ctx context.Context, req PrepareVideoRequest) (PreparedVideo, error) {
	provider, _ := r.registry.Get(r.chain[0])
	preparer, ok := provider.(VideoPreparer)
	if !ok {
		return PreparedVideo{}, fmt.Errorf("video provider %q does not support video preparation", provider.Name())
	}
	return preparer.PrepareVideo(ctx, req)
}
