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

type PrepareVideoRequest = common.PrepareVideoRequest
type PreparedVideo = common.PreparedVideo
