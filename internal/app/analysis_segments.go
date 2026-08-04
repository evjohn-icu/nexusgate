package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/evjohn-icu/timingdex/internal/domain"
	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
	"github.com/evjohn-icu/timingdex/internal/media"
	videoproviders "github.com/evjohn-icu/timingdex/internal/providers/video"
)

// analyzeVideo runs the video model over an asset's proxy and returns one
// result for the whole asset.
//
// An asset longer than the provider's request will not fit in a single call —
// the endpoint answers 413 and the analysis is lost permanently, since a body
// that is too large stays too large on retry. So the proxy is cut into windows
// that fit, each is analysed on its own, and the answers are merged back onto
// the asset timeline.
//
// Splitting is avoided wherever possible: a provider that uploads out of band
// has no request-size ceiling, and a proxy already inside the budget is sent
// whole. The overwhelming majority of clips take neither branch.
func (p *Pipeline) analyzeVideo(ctx context.Context, assetID string, input videoanalysis.Input, durationMS int64) (videoanalysis.Result, string, error) {
	single := func() (videoanalysis.Result, string, error) {
		return p.videoProvider.Analyze(ctx, input)
	}
	// Already uploaded out of band: the request carries a reference, not bytes.
	if input.RemoteURI != "" {
		return single()
	}
	budget, inline := videoproviders.InlineVideoBudget(p.videoProvider, media.DefaultInlineBudgetBytes)
	if !inline {
		return single()
	}
	info, err := os.Stat(input.VideoPath)
	if err != nil {
		return videoanalysis.Result{}, "", err
	}
	plan := media.PlanAnalysisWindows(durationMS, info.Size(), budget, media.DefaultMaxWindowMS, media.DefaultWindowOverlapMS)
	if !plan.Split {
		return single()
	}

	// Windows are scratch: they are cheap to recreate from the proxy and must
	// not accumulate in the cache directory next to real artifacts.
	dir := filepath.Join(filepath.Dir(input.VideoPath), "analysis-windows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return videoanalysis.Result{}, "", err
	}
	defer os.RemoveAll(dir)

	results := make([]videoanalysis.WindowResult, 0, len(plan.Windows))
	raws := make([]json.RawMessage, 0, len(plan.Windows))
	for _, window := range plan.Windows {
		path := filepath.Join(dir, fmt.Sprintf("window-%03d.mp4", window.Index))
		if err := media.ExtractAnalysisWindow(ctx, input.VideoPath, path, window); err != nil {
			return videoanalysis.Result{}, "", fmt.Errorf("extract analysis window %d of %d: %w", window.Index+1, len(plan.Windows), err)
		}
		windowInput := input
		windowInput.VideoPath = path
		windowInput.Transcript = sliceTranscript(input.Transcript, window)

		result, raw, err := p.videoProvider.Analyze(ctx, windowInput)
		raws = append(raws, rawMessage(raw))
		if err != nil {
			// One failed window makes the asset's analysis incomplete, and a
			// partial answer committed as if it described the whole asset
			// would be worse than none. Carry the raw responses collected so
			// far so the failed run still records what came back.
			return videoanalysis.Result{}, joinRaw(raws), fmt.Errorf("analyse window %d of %d (%dms-%dms): %w",
				window.Index+1, len(plan.Windows), window.StartMS, window.EndMS, err)
		}
		results = append(results, videoanalysis.WindowResult{StartMS: window.StartMS, EndMS: window.EndMS, Result: result})
		// Free each window as soon as it has been sent. A long asset otherwise
		// holds every window on disk at once, on top of the proxy itself.
		_ = os.Remove(path)
	}
	return videoanalysis.MergeWindowResults(results), joinRaw(raws), nil
}

// sliceTranscript narrows the transcript to what is audible inside the window
// and re-bases it to window-relative time, matching the video the model is
// shown. Handing it the whole asset's transcript would describe speech that is
// not in the clip it is looking at.
func sliceTranscript(transcript *domain.Transcript, window media.AnalysisWindow) *domain.Transcript {
	if transcript == nil {
		return nil
	}
	if len(transcript.Segments) == 0 {
		// Nothing to place on the timeline; the plain text is all there is, and
		// it describes the whole asset rather than this window.
		return transcript
	}
	sliced := &domain.Transcript{Language: transcript.Language}
	text := make([]byte, 0, len(transcript.Text))
	for _, segment := range transcript.Segments {
		if segment.EndMS <= window.StartMS || segment.StartMS >= window.EndMS {
			continue
		}
		shifted := segment
		shifted.StartMS = maxInt64(segment.StartMS-window.StartMS, 0)
		shifted.EndMS = minInt64(segment.EndMS, window.EndMS) - window.StartMS
		sliced.Segments = append(sliced.Segments, shifted)
		if len(text) > 0 {
			text = append(text, ' ')
		}
		text = append(text, segment.Text...)
	}
	sliced.Text = string(text)
	return sliced
}

// rawMessage keeps a provider response embeddable in the run's raw record
// whether or not it was JSON. A non-JSON body — an HTML error page from a relay,
// say — would otherwise make the whole array unparseable.
func rawMessage(raw string) json.RawMessage {
	if json.Valid([]byte(raw)) {
		return json.RawMessage(raw)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return encoded
}

// joinRaw records every window's response, so the immutable model_run holds
// the full evidence for a merged analysis rather than the last window's reply.
func joinRaw(raws []json.RawMessage) string {
	if len(raws) == 1 {
		return string(raws[0])
	}
	encoded, err := json.Marshal(raws)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
