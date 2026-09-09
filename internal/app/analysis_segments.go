package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/cachecoord"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	videoanalysis "github.com/evjohn-icu/nexusgate/internal/domain/video_analysis"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/normalize"
	"github.com/evjohn-icu/nexusgate/internal/providers/common"
	videoproviders "github.com/evjohn-icu/nexusgate/internal/providers/video"
)

// analyzeAssetVideo runs the plain video analysis path against one provider:
// a model run recorded under the provider's own identity, window planning
// around its inline budget, validation, and the canonical commit. The plain
// route passes the whole router (fallbacks stay intact); the two-pass
// multiframe fallback reuses it for pass 1 with the route's video member.
func (p *Pipeline) analyzeAssetVideo(ctx context.Context, j *domain.Job, m *domain.MediaMetadata, provider videoproviders.VideoUnderstandingProvider, sourcePath, worker string) error {
	reqJSON := fmt.Sprintf(`{"asset_id":%q,"path":%q}`, j.AssetID, sourcePath)
	// v4 of the prompt and v2 of the schema change shot semantics: a shot
	// that did not observe an object/action/mood keeps empty lists instead
	// of inheriting the asset-global ones, and Gemini now reports per-shot
	// objects/actions/mood. The version bump is what breaks CreateModelRun's
	// cache so an old run can never satisfy a new analysis.
	providerName, modelName, promptVersion := provider.Name(), provider.Model(), p.promptVersion("footage-analysis-v4")
	runID, cached, err := p.repo.CreateModelRun(ctx, j.AssetID, "vision", providerName, modelName, j.InputHash, promptVersion, "asset-analysis/v2", reqJSON, j.ID, worker)
	if err != nil {
		return err
	}
	if cached {
		return nil
	}
	var a domain.StructuredAnalysis
	var raw string
	proxy, e := p.repo.GetArtifact(ctx, j.AssetID, "proxy")
	if e != nil {
		return e
	}
	if proxy == nil {
		return domain.Permanent(fmt.Errorf("proxy artifact missing"))
	}
	transcript, e := p.repo.GetTranscript(ctx, j.AssetID)
	if e != nil {
		return e
	}
	// The strongest timing evidence wins: a forced alignment's
	// word-level timestamps place speech on the asset timeline far
	// more precisely than the ASR transcript's segments (which some
	// providers emit as a single 0-0 placeholder, or not at all).
	// Without this, alignment results were persisted and never read.
	if transcript != nil {
		words, e := p.repo.GetAlignmentWords(ctx, j.AssetID)
		if e != nil {
			return e
		}
		if aligned := domain.TranscriptFromAlignmentWords(words); aligned != nil {
			aligned.Language = transcript.Language
			transcript = aligned
		}
	}
	analyzeReq := videoanalysis.Input{VideoPath: proxy.LocalPath, Transcript: transcript, Metadata: *m, Language: p.analysisLanguage}
	cacheLock, e := cachecoord.AcquireShared(filepath.Dir(p.cacheDir))
	if e != nil {
		return e
	}
	requiresPreparation := false
	if preparation, ok := provider.(interface{ RequiresVideoPreparation() bool }); ok {
		requiresPreparation = preparation.RequiresVideoPreparation()
	} else if _, ok := provider.(videoproviders.VideoPreparer); ok {
		requiresPreparation = true
	}
	if requiresPreparation {
		preparer, ok := provider.(videoproviders.VideoPreparer)
		if !ok {
			return domain.Permanent(fmt.Errorf("video provider %q requires preparation but cannot prepare video", provider.Name()))
		}
		cachedFile, e := p.repo.GetProviderFile(ctx, j.AssetID, "proxy", proxy.ProfileHash, provider.Name())
		if e != nil {
			return e
		}
		if cachedFile == nil || cachedFile.State != "ACTIVE" || (cachedFile.ExpiresAt != nil && cachedFile.ExpiresAt.Before(time.Now())) {
			prepared, e := preparer.PrepareVideo(ctx, videoproviders.PrepareVideoRequest{VideoPath: proxy.LocalPath, DisplayName: j.AssetID + "-proxy.mp4", MIMEType: "video/mp4"})
			if e != nil {
				return e
			}
			cachedFile = &domain.ProviderFile{ID: idgen.New(), AssetID: j.AssetID, ArtifactType: "proxy", ProfileHash: proxy.ProfileHash, Provider: provider.Name(), RemoteName: prepared.RemoteName, RemoteURI: prepared.RemoteURI, MIMEType: prepared.MIMEType, State: prepared.State, SizeBytes: prepared.SizeBytes}
			if e := p.repo.SaveProviderFile(ctx, *cachedFile); e != nil {
				return e
			}
		}
		analyzeReq.RemoteURI, analyzeReq.MIMEType = cachedFile.RemoteURI, cachedFile.MIMEType
	}
	if e := cacheLock.Release(); e != nil {
		return e
	}
	result, rawResult, providerErr := p.analyzeVideo(ctx, provider, j.AssetID, analyzeReq, m.DurationMS)
	raw = rawResult
	err = providerErr
	if err != nil {
		if failErr := p.failModelRun(ctx, runID, "provider_error", err.Error(), raw, j, worker); failErr != nil {
			return failErr
		}
		return err
	}
	// The model has been paid and has answered; nothing from here to the
	// commit touches the network. Both validators mark their own
	// rejections permanent (see normalize.ValidateAndNormalize and
	// validateAnalysisShots), so these two returns stay plain: the
	// verdict travels with the error rather than being restated by
	// whoever happens to be calling.
	a = result.ToStructuredAnalysis()
	a, err = normalize.ValidateAndNormalize(a)
	if err != nil {
		if failErr := p.failModelRun(ctx, runID, "validation_error", err.Error(), "", j, worker); failErr != nil {
			return failErr
		}
		return err
	}
	shots := result.ToAssetShots(j.AssetID, runID)
	if err := validateAnalysisShots(shots, m.DurationMS); err != nil {
		if failErr := p.failModelRun(ctx, runID, "validation_error", err.Error(), raw, j, worker); failErr != nil {
			return failErr
		}
		return err
	}
	parsed, _ := json.Marshal(a)
	if err := p.repo.StageModelRun(ctx, runID, raw, string(parsed), j.ID, worker); err != nil {
		return err
	}
	if err := p.repo.CommitAnalysisWithShots(ctx, j.AssetID, runID, "asset-analysis/v2", a, shots, j.ID, worker); err != nil {
		return err
	}
	// The run is canonical; record its estimate against the serving
	// channel's cost metadata. No channel with cost metadata → no-op.
	p.recordCostEstimate(ctx, "video_analysis", providerName, modelName, j.AssetID, m.DurationMS)
	// The shots are canonical now; the embedding layer (if configured) gets
	// its incremental rebuild. Synchronous on purpose — the pipeline's
	// "process exited means no call in flight" invariant. Errors are the
	// hook's problem, never this job's.
	p.afterShotsCommitted(ctx, j.AssetID)
	return nil
}

// analyzeVideo runs the video model over an asset's proxy and returns one
// result for the whole asset.
//
// An asset longer than the provider's request will not fit in a single call —
// the endpoint answers 413. So the proxy is cut into windows that fit, each is
// analysed on its own, and the answers are merged back onto the asset
// timeline.
//
// Splitting is avoided wherever possible: a provider that uploads out of band
// has no request-size ceiling, and a proxy already inside the budget is sent
// whole. The overwhelming majority of clips take neither branch.
//
// A declared budget is an estimate, not a guarantee: bitrate is not constant
// across a proxy, and a stream-copy extraction can only cut on a keyframe, so
// a window can arrive larger than planned even when the arithmetic that sized
// it was correct. A 413 is a statement about *this request's size* — unlike
// the rest of the 4xx family, the caller controls the one thing it complains
// about, and presenting a smaller version of the same window is not "identical
// inputs to a deterministic decision" (see domain.ErrPermanentFailure). So a
// window that gets a 413 is bisected and each half is retried, down to
// media.MinAnalysisWindowMS; only a window that cannot be split any further
// and still gets a 413 is a genuine dead end.
func (p *Pipeline) analyzeVideo(ctx context.Context, provider videoproviders.VideoUnderstandingProvider, assetID string, input videoanalysis.Input, durationMS int64) (videoanalysis.Result, string, error) {
	single := func() (videoanalysis.Result, string, error) {
		return provider.Analyze(ctx, input)
	}
	// Already uploaded out of band: the request carries a reference, not bytes.
	if input.RemoteURI != "" {
		return single()
	}
	budget, inline := videoproviders.InlineVideoBudget(provider, media.DefaultInlineBudgetBytes)
	if !inline {
		return single()
	}
	info, err := os.Stat(input.VideoPath)
	if err != nil {
		return videoanalysis.Result{}, "", err
	}
	plan := media.PlanAnalysisWindows(durationMS, info.Size(), budget, media.DefaultMaxWindowMS, media.DefaultWindowOverlapMS)
	if !plan.Split {
		result, raw, err := single()
		if err == nil || !isRequestEntityTooLarge(err) {
			return result, raw, err
		}
		if durationMS/2 < media.MinAnalysisWindowMS {
			// Nothing smaller exists to retry with — the endpoint's actual
			// ceiling is below the shortest window this pipeline will plan.
			return videoanalysis.Result{}, raw, fmt.Errorf("analyse whole asset (0ms-%dms), too short to split further: %w", durationMS, err)
		}
		// The declared budget said the whole proxy would fit; the endpoint
		// disagrees. Recover by bisecting straight away rather than resending
		// the same oversized request first — that upload has already been
		// paid for once.
		mid := durationMS / 2
		plan = media.WindowPlan{Split: true, Windows: []media.AnalysisWindow{
			{Index: 0, StartMS: 0, EndMS: mid},
			{Index: 1, StartMS: mid, EndMS: durationMS},
		}}
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
	total := len(plan.Windows)
	for i, window := range plan.Windows {
		windowResults, windowRaws, err := p.analyzeAnalysisWindow(ctx, provider, dir, input, window, i, total)
		raws = append(raws, windowRaws...)
		if err != nil {
			// One failed window makes the asset's analysis incomplete, and a
			// partial answer committed as if it described the whole asset
			// would be worse than none. Carry the raw responses collected so
			// far so the failed run still records what came back.
			return videoanalysis.Result{}, joinRaw(raws), err
		}
		results = append(results, windowResults...)
	}
	return videoanalysis.MergeWindowResults(results), joinRaw(raws), nil
}

// analyzeAnalysisWindow extracts one window, analyses it, and — on a 413 for
// a window still wide enough to halve — bisects it and retries each half
// before giving up. It returns every window result the recursion produced
// (more than one when a bisection succeeded) and every raw response along the
// way, success or failure, so the caller's evidence trail is complete either
// way.
func (p *Pipeline) analyzeAnalysisWindow(ctx context.Context, provider videoproviders.VideoUnderstandingProvider, dir string, input videoanalysis.Input, window media.AnalysisWindow, index, total int) ([]videoanalysis.WindowResult, []json.RawMessage, error) {
	path := filepath.Join(dir, fmt.Sprintf("window-%s.mp4", idgen.New()))
	if err := media.ExtractAnalysisWindow(ctx, input.VideoPath, path, window); err != nil {
		return nil, nil, fmt.Errorf("extract analysis window %d of %d: %w", index+1, total, err)
	}
	windowInput := input
	windowInput.VideoPath = path
	windowInput.Transcript = sliceTranscript(input.Transcript, window)

	result, raw, err := provider.Analyze(ctx, windowInput)
	// Free the window as soon as it has been sent. A long asset otherwise
	// holds every window on disk at once, on top of the proxy itself.
	_ = os.Remove(path)
	raws := []json.RawMessage{rawMessage(raw)}
	if err == nil {
		return []videoanalysis.WindowResult{{StartMS: window.StartMS, EndMS: window.EndMS, Result: result}}, raws, nil
	}
	if isRequestEntityTooLarge(err) && window.Duration()/2 >= media.MinAnalysisWindowMS {
		mid := window.StartMS + window.Duration()/2
		left := media.AnalysisWindow{Index: window.Index, StartMS: window.StartMS, EndMS: mid}
		right := media.AnalysisWindow{Index: window.Index, StartMS: mid, EndMS: window.EndMS}
		leftResults, leftRaws, lerr := p.analyzeAnalysisWindow(ctx, provider, dir, input, left, index, total)
		raws = append(raws, leftRaws...)
		if lerr != nil {
			return nil, raws, lerr
		}
		rightResults, rightRaws, rerr := p.analyzeAnalysisWindow(ctx, provider, dir, input, right, index, total)
		raws = append(raws, rightRaws...)
		if rerr != nil {
			return nil, raws, rerr
		}
		return append(leftResults, rightResults...), raws, nil
	}
	return nil, raws, fmt.Errorf("analyse window %d of %d (%dms-%dms): %w", index+1, total, window.StartMS, window.EndMS, err)
}

// isRequestEntityTooLarge reports whether err is a 413 from the provider —
// classified by status, the same way every other retry decision in this
// pipeline is (see isRetryableJobError in pipeline.go), never by matching on
// the body text a relay happened to echo back.
func isRequestEntityTooLarge(err error) bool {
	var status *common.StatusError
	return errors.As(err, &status) && status.StatusCode == http.StatusRequestEntityTooLarge
}

// sliceTranscript narrows the transcript to what is audible inside the window
// and re-bases it to window-relative time, matching the video the model is
// shown. Handing it the whole asset's transcript would describe speech that is
// not in the clip it is looking at.
//
// Only a transcript whose segments carry real timestamps is sliced. Some ASR
// providers (Qwen, Volcengine) return the full text with a single 0-0
// placeholder segment: that text has no placement on the timeline, and giving
// it to a window — any window — would let the model attribute speech from
// elsewhere in the asset to the footage it is looking at. In split mode such a
// transcript is withheld entirely; the single-call path (analyzeVideo's
// `single`) passes the whole text alongside the whole video, where it is
// honest context. Alignment word timestamps are the recovery path for long
// assets: when an aligner is configured the analyze stage feeds these slices
// from the aligned transcript instead.
func sliceTranscript(transcript *domain.Transcript, window media.AnalysisWindow) *domain.Transcript {
	if transcript == nil || !transcript.Timed() {
		return nil
	}
	sliced := &domain.Transcript{Language: transcript.Language}
	text := make([]byte, 0, len(transcript.Text))
	for _, segment := range transcript.Segments {
		// Degenerate segments (EndMS <= StartMS) are dropped outright: the
		// overlap check below is enough for a 0-0 placeholder, but an inverted
		// segment would otherwise survive re-basing and reach the model.
		if segment.EndMS <= segment.StartMS || segment.EndMS <= window.StartMS || segment.StartMS >= window.EndMS {
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

// promptVersion folds the configured analysis language into the prompt identity
// CreateModelRun dedupes on. The language is part of the prompt, so an answer
// written in another language is not the same answer — without this, switching
// the language returns the cached English text for every asset already
// analysed, and the setting looks broken while behaving exactly as designed.
//
// An unset language leaves the version byte-identical, so no existing cache
// entry is invalidated by this code existing. And because a cached run is only
// consulted when an analyze job actually runs, changing the language does not
// re-bill a library on upgrade — it changes what the next reanalysis produces.
func (p *Pipeline) promptVersion(base string) string {
	if strings.TrimSpace(p.analysisLanguage) == "" {
		return base
	}
	return base + "+lang=" + p.analysisLanguage
}
