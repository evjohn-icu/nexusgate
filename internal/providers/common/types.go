package common

import (
	"context"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

type TranscribeRequest struct {
	AudioPath string
	Language  string
}

type AnalyzeRequest struct {
	VideoPath  string
	RemoteURI  string
	MIMEType   string
	Transcript *domain.Transcript
	Metadata   domain.MediaMetadata
	Prompt     string
}

type PrepareVideoRequest struct {
	VideoPath   string
	DisplayName string
	MIMEType    string
}

type PreparedVideo struct {
	RemoteName string
	RemoteURI  string
	MIMEType   string
	State      string
	SizeBytes  int64
}

type AlignRequest struct {
	AudioPath  string
	Transcript domain.Transcript
	Language   string
}

type ASR interface {
	Name() string
	Model() string
	Transcribe(context.Context, TranscribeRequest) (domain.Transcript, error)
}

type Vision interface {
	Name() string
	Model() string
	Analyze(context.Context, AnalyzeRequest) (domain.StructuredAnalysis, string, error)
}

type VideoPreparer interface {
	PrepareVideo(context.Context, PrepareVideoRequest) (PreparedVideo, error)
}

type Alignment interface {
	Name() string
	Model() string
	Align(context.Context, AlignRequest) (domain.AlignmentResult, error)
}

type TagCurator interface {
	Name() string
	Model() string
	Curate(context.Context, []domain.UnresolvedTag, []domain.CanonicalTag) ([]domain.TagProposal, error)
	SummarizeLibrary(context.Context, domain.LibrarySummaryInput) (domain.LibrarySummaryDraft, error)
}

// Embedder intentionally has no access to assets. It embeds only normalized
// tag strings so the vector layer cannot turn into an opaque asset search path.
type Embedder interface {
	Name() string
	Model() string
	Embed(context.Context, []string) ([][]float64, error)
}

type RepurposePlanner interface {
	Name() string
	Model() string
	Plan(context.Context, domain.RepurposeBrief) (domain.RepurposePlanDraft, error)
}
