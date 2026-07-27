package providers

import (
	"github.com/ev/timingdex/internal/providers/common"
	videoproviders "github.com/ev/timingdex/internal/providers/video"
)

type TranscribeRequest = common.TranscribeRequest
type AnalyzeRequest = common.AnalyzeRequest
type PrepareVideoRequest = common.PrepareVideoRequest
type PreparedVideo = common.PreparedVideo
type AlignRequest = common.AlignRequest
type ASR = common.ASR
type Vision = common.Vision
type VideoUnderstandingProvider = videoproviders.VideoUnderstandingProvider
type VideoPreparer = common.VideoPreparer
type Alignment = common.Alignment
type TagCurator = common.TagCurator
type Embedder = common.Embedder
type RepurposePlanner = common.RepurposePlanner
