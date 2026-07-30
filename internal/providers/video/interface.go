package video

import (
	"context"

	videoanalysis "github.com/ev/timingdex/internal/domain/video_analysis"
	"github.com/ev/timingdex/internal/providers/common"
)

type Capability string

const (
	CapabilityVideoAnalysis Capability = "video_analysis"
	CapabilitySceneAnalysis Capability = "scene_analysis"
	CapabilityShotAnalysis  Capability = "shot_analysis"
)

type VideoUnderstandingProvider interface {
	Name() string
	Model() string
	Capabilities() []Capability
	Analyze(context.Context, videoanalysis.Input) (videoanalysis.Result, string, error)
}

type VideoPreparer interface {
	PrepareVideo(context.Context, common.PrepareVideoRequest) (common.PreparedVideo, error)
}

// InlineVideoLimiter is implemented by providers that carry the video inside
// the request body. It reports the largest file the endpoint will accept, so a
// longer asset can be cut into windows that fit rather than rejected whole.
//
// The limit is deliberately the provider's to state and not a constant here:
// endpoints differ by more than an order of magnitude — one relay refuses
// around 40 MB while another documented API takes 100 MB inline — and a
// provider that uploads out of band via VideoPreparer has no such ceiling at
// all. A single hardcoded number would either waste most of the allowance or
// exceed it.
//
// A zero or negative return means "unknown"; callers fall back to a
// conservative default rather than assuming the request will fit.
type InlineVideoLimiter interface {
	MaxInlineVideoBytes() int64
}

// InlineVideoBudget reports the byte budget for one request to this provider,
// and whether the provider needs one at all. A provider that prepares uploads
// out of band is not limited by request size, so it is never split.
//
// Preparation is detected the way the pipeline detects it — via
// RequiresVideoPreparation where present — because Router implements
// PrepareVideo unconditionally in order to delegate, and a bare type assertion
// would therefore report every routed provider as an uploader.
func InlineVideoBudget(provider any, fallback int64) (budget int64, inline bool) {
	uploads := false
	if declares, ok := provider.(interface{ RequiresVideoPreparation() bool }); ok {
		uploads = declares.RequiresVideoPreparation()
	} else if _, ok := provider.(VideoPreparer); ok {
		uploads = true
	}
	if uploads {
		return 0, false
	}
	if limiter, ok := provider.(InlineVideoLimiter); ok {
		if declared := limiter.MaxInlineVideoBytes(); declared > 0 {
			return declared, true
		}
	}
	return fallback, true
}

type PrepareVideoRequest = common.PrepareVideoRequest
type PreparedVideo = common.PreparedVideo
