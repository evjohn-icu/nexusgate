package video

import (
	"context"

	"github.com/evjohn-icu/nexusslate/internal/domain"
	videoanalysis "github.com/evjohn-icu/nexusslate/internal/domain/video_analysis"
	"github.com/evjohn-icu/nexusslate/internal/providers/common"
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

// ShotAnalysisRequest is one shot handed to a multiframe VLM. NexusSlate has
// already decided the shot's boundaries, sampled its frames and sliced the
// transcript to the shot's window — the model is only asked to describe what
// the frames show. Frame timestamps are asset-relative.
type ShotAnalysisRequest struct {
	Frames      []videoanalysis.Frame
	ShotStartMS int64
	ShotEndMS   int64
	Transcript  *domain.Transcript
	Metadata    domain.MediaMetadata
}

// ShotMetadata is what a multiframe VLM returns: pure per-shot understanding,
// with no timeline fields. The boundaries come from NexusSlate's detector; the
// model must not be able to move a shot, only describe it. Enum-valued fields
// (shot_size, camera_motion, quality) use the same controlled vocabularies as
// the asset-level analysis.
type ShotMetadata struct {
	Description  string   `json:"description"`
	Objects      []string `json:"objects,omitempty"`
	Actions      []string `json:"actions,omitempty"`
	Mood         []string `json:"mood,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	ShotSize     string   `json:"shot_size,omitempty"`
	CameraMotion string   `json:"camera_motion,omitempty"`
	Quality      string   `json:"quality,omitempty"`
	UsableAs     []string `json:"usable_as,omitempty"`
	Confidence   float64  `json:"confidence,omitempty"`
}

// MultiframeShotAnalyzer is implemented by providers that understand still
// frames only (openai_multiframe). The embedded VideoUnderstandingProvider
// covers the asset-level summary call (Analyze with a handful of
// representative frames); AnalyzeShot is the per-shot refinement call. The
// raw response is returned alongside the metadata so the model run can record
// it; a provider is expected to mark undecodable output permanent at the call
// site.
type MultiframeShotAnalyzer interface {
	VideoUnderstandingProvider
	AnalyzeShot(context.Context, ShotAnalysisRequest) (ShotMetadata, string, error)
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
