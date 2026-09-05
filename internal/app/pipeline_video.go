package app

import (
	"context"

	videoanalysis "github.com/evjohn-icu/nexusgate/internal/domain/video_analysis"
	videoproviders "github.com/evjohn-icu/nexusgate/internal/providers/video"
)

// pipelineVideo is the video provider the Pipeline actually sees. Service
// wiring hands the pipeline the channel runtime's wrapper so /providers
// routes keep working (channel-first, legacy fallback), but that wrapper
// deliberately exposes no multiframe surface — which silently disabled the
// multiframe orchestration in production: multiframeRouteOf found no router,
// the pipeline fell back to the plain whole-video path, and a frame-only
// provider failed with "does not support video preparation" instead of
// running detector mode or the two-pass fallback. The bridge keeps the
// channel wrapper as the routing/analyze surface and lifts the legacy
// router's multiframe selectors on top of it. Multiframe remains
// legacy-config-only (channels refuse openai_multiframe at build time), so
// the two surfaces cannot disagree about who owns a capability.
type pipelineVideo struct {
	// channel is the channel-first provider with legacy fallback; it is the
	// plain Analyze surface and the two-pass fallback's pass-1 provider.
	// The concrete *channelVideo type is held so the optional preparation
	// surface (RequiresVideoPreparation/PrepareVideo) stays reachable.
	channel *channelVideo
	// router is the legacy-config router; its MultiframeAnalyzer is the
	// detector-mode analyzer and the two-pass refinement analyzer.
	router *videoproviders.Router
}

func (b *pipelineVideo) Name() string                              { return b.channel.Name() }
func (b *pipelineVideo) Model() string                             { return b.channel.Model() }
func (b *pipelineVideo) Capabilities() []videoproviders.Capability { return b.channel.Capabilities() }
func (b *pipelineVideo) RequiresVideoPreparation() bool            { return b.channel.RequiresVideoPreparation() }
func (b *pipelineVideo) MaxInlineVideoBytes() int64                { return b.channel.MaxInlineVideoBytes() }
func (b *pipelineVideo) PrepareVideo(ctx context.Context, req videoproviders.PrepareVideoRequest) (videoproviders.PreparedVideo, error) {
	return b.channel.PrepareVideo(ctx, req)
}
func (b *pipelineVideo) Analyze(ctx context.Context, input videoanalysis.Input) (videoanalysis.Result, string, error) {
	return b.channel.Analyze(ctx, input)
}

// MultiframeAnalyzer exposes the legacy router's frame-only member. nil when
// vision_primary is a whole-video provider, which keeps the plain path.
func (b *pipelineVideo) MultiframeAnalyzer() videoproviders.MultiframeShotAnalyzer {
	if b.router == nil {
		return nil
	}
	return b.router.MultiframeAnalyzer()
}

// VideoAnalyzer is the pass-1 provider for the two-pass fallback: the
// channel wrapper, so a channel-managed whole-video provider (or the legacy
// fallback inside it) supplies the boundaries when no detector is configured.
func (b *pipelineVideo) VideoAnalyzer() videoproviders.VideoUnderstandingProvider {
	return b.channel
}
