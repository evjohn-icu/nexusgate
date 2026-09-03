package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/evjohn-icu/nexusslate/internal/cachecoord"
	"github.com/evjohn-icu/nexusslate/internal/domain"
	videoanalysis "github.com/evjohn-icu/nexusslate/internal/domain/video_analysis"
	"github.com/evjohn-icu/nexusslate/internal/media"
	"github.com/evjohn-icu/nexusslate/internal/normalize"
	"github.com/evjohn-icu/nexusslate/internal/providers/shotdetect"
	videoproviders "github.com/evjohn-icu/nexusslate/internal/providers/video"
)

// multiframeRoute is the part of the video route the multiframe analysis
// needs: the member that understands still frames only (analyzer) and the
// first member that consumes video whole (video). A route without an analyzer
// is a plain video route; a route with an analyzer but no video member cannot
// run the two-pass fallback.
type multiframeRoute struct {
	analyzer videoproviders.MultiframeShotAnalyzer
	video    videoproviders.VideoUnderstandingProvider
}

// multiframeRouteOf resolves the route from the pipeline's provider. The
// Router exposes the two selectors; a bare provider yields nil, which keeps
// non-multiframe deployments on the plain video path. Production wiring hands
// the pipeline a pipelineVideo bridge (channel wrapper + legacy router), so
// this resolves whenever the legacy router carries a multiframe member.
func (p *Pipeline) multiframeRouteOf() *multiframeRoute {
	router, ok := p.videoProvider.(interface {
		MultiframeAnalyzer() videoproviders.MultiframeShotAnalyzer
		VideoAnalyzer() videoproviders.VideoUnderstandingProvider
	})
	if !ok {
		return nil
	}
	analyzer := router.MultiframeAnalyzer()
	if analyzer == nil {
		return nil
	}
	return &multiframeRoute{analyzer: analyzer, video: router.VideoAnalyzer()}
}

// analyzeWithDetector runs the pure multiframe path: deterministic shot
// boundaries from the detector, one summary call for the asset-level
// analysis, and one refinement call per shot. The run's input hash derives
// from the detector identity and the sampler, so a detector swap or threshold
// change re-keys the analysis instead of reusing a cached run — the detector
// IS part of what produced the shots, exactly as the model is.
func (p *Pipeline) analyzeWithDetector(ctx context.Context, j *domain.Job, m *domain.MediaMetadata, route *multiframeRoute, worker string) error {
	proxy, err := p.repo.GetArtifact(ctx, j.AssetID, "proxy")
	if err != nil {
		return err
	}
	if proxy == nil {
		return domain.Permanent(fmt.Errorf("proxy artifact missing"))
	}
	transcript, err := p.analysisTranscript(ctx, j.AssetID)
	if err != nil {
		return err
	}
	detectorName := p.shotDetector.Name()
	runHash := hashStrings(j.InputHash, "multiframe-analysis-v1", detectorName, media.FrameSamplingBoundaryAware)
	reqJSON := multiframeRequestJSON(j.AssetID, proxy.LocalPath, detectorName, media.FrameSamplingBoundaryAware)
	runID, cached, err := p.repo.CreateModelRun(ctx, j.AssetID, "vision", route.analyzer.Name(), route.analyzer.Model(), runHash, "multiframe-analysis-v1", "asset-analysis/v2", reqJSON, j.ID, worker)
	if err != nil {
		return err
	}
	if cached {
		return nil
	}
	raws := make([]json.RawMessage, 0, 32)

	cacheLock, err := cachecoord.AcquireShared(filepath.Dir(p.cacheDir))
	if err != nil {
		return err
	}
	bounds, err := p.shotDetector.Detect(ctx, proxy.LocalPath, m.DurationMS)
	_ = cacheLock.Release()
	if err != nil {
		if failErr := p.failModelRun(ctx, runID, "provider_error", err.Error(), "", j, worker); failErr != nil {
			return failErr
		}
		return err
	}
	// A boundary set that violates the nexusslate-owned rules is a verdict on
	// bytes NexusSlate already holds: the same video and the same detector
	// would fail the same way again. Normalize's rejection is therefore
	// permanent, exactly like a model answer that fails validation.
	normalized, err := shotdetect.Normalize(bounds, m.DurationMS)
	if err != nil {
		if failErr := p.failModelRun(ctx, runID, "validation_error", err.Error(), "", j, worker); failErr != nil {
			return failErr
		}
		return domain.Permanent(err)
	}

	cacheLock, err = cachecoord.AcquireShared(filepath.Dir(p.cacheDir))
	if err != nil {
		return err
	}
	summary, summaryRaw, err := p.summaryCall(ctx, route.analyzer, proxy, transcript, m, normalized)
	_ = cacheLock.Release()
	raws = append(raws, rawMessage(summaryRaw))
	if err != nil {
		if failErr := p.failModelRun(ctx, runID, "provider_error", err.Error(), joinRaw(raws), j, worker); failErr != nil {
			return failErr
		}
		return err
	}
	a := summary.ToStructuredAnalysis()
	a, err = normalize.ValidateAndNormalize(a)
	if err != nil {
		if failErr := p.failModelRun(ctx, runID, "validation_error", err.Error(), joinRaw(raws), j, worker); failErr != nil {
			return failErr
		}
		return err
	}

	cacheLock, err = cachecoord.AcquireShared(filepath.Dir(p.cacheDir))
	if err != nil {
		return err
	}
	shots, metas, raws2, err := p.refineShots(ctx, j.AssetID, runID, proxy, transcript, m, route.analyzer, normalized)
	_ = cacheLock.Release()
	raws = append(raws, raws2...)
	if err != nil {
		if failErr := p.failModelRun(ctx, runID, "provider_error", err.Error(), joinRaw(raws), j, worker); failErr != nil {
			return failErr
		}
		return err
	}
	if err := validateAnalysisShots(shots, m.DurationMS); err != nil {
		if failErr := p.failModelRun(ctx, runID, "validation_error", err.Error(), joinRaw(raws), j, worker); failErr != nil {
			return failErr
		}
		return err
	}
	foldShotMetadata(&a, metas)
	parsed, _ := json.Marshal(a)
	if err := p.repo.StageModelRun(ctx, runID, joinRaw(raws), string(parsed), j.ID, worker); err != nil {
		return err
	}
	return p.repo.CommitAnalysisWithShots(ctx, j.AssetID, runID, "asset-analysis/v2", a, shots, j.ID, worker)
}

// analyzeTwoPass runs the no-detector fallback: pass 1 is the existing VLM
// window analysis through the route's video member (committed as its own
// run, asset-level analysis attributed to the provider that produced it),
// pass 2 refines every shot through the multiframe analyzer and replaces the
// shot rows. A failed pass 2 leaves pass 1's results intact — degradation,
// not loss.
func (p *Pipeline) analyzeTwoPass(ctx context.Context, j *domain.Job, m *domain.MediaMetadata, route *multiframeRoute, worker string) error {
	proxy, err := p.repo.GetArtifact(ctx, j.AssetID, "proxy")
	if err != nil {
		return err
	}
	if proxy == nil {
		return domain.Permanent(fmt.Errorf("proxy artifact missing"))
	}
	transcript, err := p.analysisTranscript(ctx, j.AssetID)
	if err != nil {
		return err
	}

	// Pass 1: boundaries plus the asset-level analysis, exactly as a plain
	// video route would produce them. It runs first so a pass-1 failure needs
	// no refinement run of its own to fail — the asset simply keeps whatever
	// committed analysis it had.
	if err := p.analyzeAssetVideo(ctx, j, m, route.video, proxy.LocalPath, worker); err != nil {
		return err
	}

	// Pass 2: per-shot refinement from the pass-1 boundaries. The run's input
	// hash carries its own identity so a cached refinement can never satisfy
	// a different pass-1 output.
	runHash := hashStrings(j.InputHash, "multiframe-refinement-v1")
	reqJSON := multiframeRequestJSON(j.AssetID, proxy.LocalPath, "vlm-window-analysis", media.FrameSamplingBoundaryAware)
	runID, cached, err := p.repo.CreateModelRun(ctx, j.AssetID, "vision", route.analyzer.Name(), route.analyzer.Model(), runHash, "multiframe-refinement-v1", "asset-analysis/v2", reqJSON, j.ID, worker)
	if err != nil {
		return err
	}
	if cached {
		return nil
	}
	passOneShots, err := p.repo.ListAssetShots(ctx, j.AssetID)
	if err != nil {
		return err
	}
	raws := make([]json.RawMessage, 0, len(passOneShots))
	cacheLock, err := cachecoord.AcquireShared(filepath.Dir(p.cacheDir))
	if err != nil {
		return err
	}
	refined, _, raws2, err := p.refineShots(ctx, j.AssetID, runID, proxy, transcript, m, route.analyzer, shotsToBounds(passOneShots))
	_ = cacheLock.Release()
	raws = append(raws, raws2...)
	if err != nil {
		if failErr := p.failModelRun(ctx, runID, "provider_error", err.Error(), joinRaw(raws), j, worker); failErr != nil {
			return failErr
		}
		return err
	}
	if err := validateAnalysisShots(refined, m.DurationMS); err != nil {
		if failErr := p.failModelRun(ctx, runID, "validation_error", err.Error(), joinRaw(raws), j, worker); failErr != nil {
			return failErr
		}
		return err
	}
	if err := p.repo.StageModelRun(ctx, runID, joinRaw(raws), "", j.ID, worker); err != nil {
		return err
	}
	if err := p.repo.CommitShotRefinement(ctx, j.AssetID, runID, refined, j.ID, worker); err != nil {
		return err
	}
	p.afterShotsCommitted(ctx, j.AssetID)
	return nil
}

// analysisTranscript is the shared transcript+alignment resolution the
// analyze paths all use: the strongest timing evidence wins.
func (p *Pipeline) analysisTranscript(ctx context.Context, assetID string) (*domain.Transcript, error) {
	transcript, err := p.repo.GetTranscript(ctx, assetID)
	if err != nil {
		return nil, err
	}
	if transcript != nil {
		words, err := p.repo.GetAlignmentWords(ctx, assetID)
		if err != nil {
			return nil, err
		}
		if aligned := domain.TranscriptFromAlignmentWords(words); aligned != nil {
			aligned.Language = transcript.Language
			transcript = aligned
		}
	}
	return transcript, nil
}

// summaryCall asks the multiframe analyzer for the asset-level analysis from
// a handful of frames spread across the timeline. Three frames — first shot,
// middle shot, last shot — cost one call and give the model the shape of the
// asset without ever asking it for boundaries.
func (p *Pipeline) summaryCall(ctx context.Context, analyzer videoproviders.MultiframeShotAnalyzer, proxy *domain.DerivedArtifact, transcript *domain.Transcript, m *domain.MediaMetadata, bounds []shotdetect.ShotBound) (videoanalysis.Result, string, error) {
	if len(bounds) == 0 {
		return videoanalysis.Result{}, "", domain.Permanent(fmt.Errorf("shot detector produced no shots"))
	}
	dir := filepath.Join(filepath.Dir(proxy.LocalPath), "analysis-frames")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return videoanalysis.Result{}, "", err
	}
	defer os.RemoveAll(dir)
	indices := []int{0}
	if middle := len(bounds) / 2; middle != 0 {
		indices = append(indices, middle)
	}
	if last := len(bounds) - 1; last != 0 && last != indices[len(indices)-1] {
		indices = append(indices, last)
	}
	var frames []videoanalysis.Frame
	for _, index := range indices {
		b := bounds[index]
		points := media.PlanShotFrames(b.StartMS, b.EndMS, media.FrameSamplingUniform)
		if len(points) == 0 {
			continue
		}
		point := points[len(points)/2]
		paths, err := media.ExtractFrames(ctx, proxy.LocalPath, dir, []media.SamplePoint{point}, 0)
		if err != nil {
			return videoanalysis.Result{}, "", err
		}
		frames = append(frames, videoanalysis.Frame{Path: paths[0], TimestampMS: point.TimestampMS})
	}
	if len(frames) == 0 {
		return videoanalysis.Result{}, "", domain.Permanent(fmt.Errorf("shot detector produced no sampleable shots"))
	}
	input := videoanalysis.Input{VideoPath: proxy.LocalPath, Frames: frames, Transcript: boundSummaryTranscript(transcript), Metadata: *m}
	return analyzer.Analyze(ctx, input)
}

// maxSummaryTranscriptRunes bounds the transcript handed to the summary call.
// The per-shot calls slice their own window, but the summary call sees the
// whole asset; a long-form asset's transcript could otherwise overflow a
// small local model's context on top of the frames. The excerpt keeps the
// head and the tail — enough to infer what the asset is about and how it
// ends — with an explicit gap marker, deterministic and honest.
const maxSummaryTranscriptRunes = 4000

func boundSummaryTranscript(transcript *domain.Transcript) *domain.Transcript {
	if transcript == nil || len([]rune(transcript.Text)) <= maxSummaryTranscriptRunes {
		return transcript
	}
	runes := []rune(transcript.Text)
	head := string(runes[:maxSummaryTranscriptRunes/2])
	tail := string(runes[len(runes)-maxSummaryTranscriptRunes/5:])
	bounded := *transcript
	bounded.Text = head + "\n…[transcript truncated for summary]…\n" + tail
	bounded.Segments = nil
	return &bounded
}

// refineShots runs the per-shot multiframe refinement for every boundary:
// deterministic frame sampling, transcript slice, one model call per shot.
// A failure in any shot fails the whole run — a partial answer committed as
// if it described the whole asset would be worse than none, the same rule as
// the windowed path. The scratch frame directory is removed when the call
// returns, so even a long asset never accumulates analysis stills.
func (p *Pipeline) refineShots(ctx context.Context, assetID, runID string, proxy *domain.DerivedArtifact, transcript *domain.Transcript, m *domain.MediaMetadata, analyzer videoproviders.MultiframeShotAnalyzer, bounds []shotdetect.ShotBound) ([]domain.AssetShot, []videoproviders.ShotMetadata, []json.RawMessage, error) {
	dir := filepath.Join(filepath.Dir(proxy.LocalPath), "analysis-frames")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, nil, err
	}
	defer os.RemoveAll(dir)
	shots := make([]domain.AssetShot, 0, len(bounds))
	metas := make([]videoproviders.ShotMetadata, 0, len(bounds))
	raws := make([]json.RawMessage, 0, len(bounds))
	for i, b := range bounds {
		points := media.PlanShotFrames(b.StartMS, b.EndMS, media.FrameSamplingBoundaryAware)
		paths, err := media.ExtractFrames(ctx, proxy.LocalPath, dir, points, 0)
		if err != nil {
			return nil, metas, raws, fmt.Errorf("extract frames for shot %d of %d (%dms-%dms): %w", i+1, len(bounds), b.StartMS, b.EndMS, err)
		}
		frames := make([]videoanalysis.Frame, 0, len(paths))
		for index, path := range paths {
			frames = append(frames, videoanalysis.Frame{Path: path, TimestampMS: points[index].TimestampMS})
		}
		window := media.AnalysisWindow{Index: i, StartMS: b.StartMS, EndMS: b.EndMS}
		meta, raw, err := analyzer.AnalyzeShot(ctx, videoproviders.ShotAnalysisRequest{
			Frames:      frames,
			ShotStartMS: b.StartMS,
			ShotEndMS:   b.EndMS,
			Transcript:  sliceTranscript(transcript, window),
			Metadata:    *m,
		})
		raws = append(raws, rawMessage(raw))
		if err != nil {
			return nil, metas, raws, fmt.Errorf("analyse shot %d of %d (%dms-%dms): %w", i+1, len(bounds), b.StartMS, b.EndMS, err)
		}
		metas = append(metas, meta)
		shots = append(shots, domain.AssetShot{
			AssetID: assetID, SourceRunID: runID, Ordinal: i,
			StartMS: b.StartMS, EndMS: b.EndMS,
			Description: meta.Description, Tags: meta.Tags, Objects: meta.Objects,
			Actions: meta.Actions, Mood: meta.Mood, Confidence: meta.Confidence,
		})
	}
	return shots, metas, raws, nil
}

// foldShotMetadata folds the per-shot enum fields into the asset-level
// analysis. The per-shot evidence is complete — every shot contributed its
// own call — so it overrides the summary's single glance: the dominant
// shot_size and camera_motion, the worst quality across shots, and the union
// of usable_as. has_speech and audio_type are hearing questions and stay
// whatever the summary call reported (audio_type is inferred from the
// transcript, if at all).
func foldShotMetadata(a *domain.StructuredAnalysis, metas []videoproviders.ShotMetadata) {
	sizeCounts := map[string]int{}
	sizeOrder := []string{}
	motionCounts := map[string]int{}
	motionOrder := []string{}
	worst := -1
	var worstValue string
	seenUse := map[string]struct{}{}
	useOrder := []string{}

	for _, meta := range metas {
		// unknown is an abstention everywhere, not a verdict: the adapter maps
		// a sent-but-bogus answer to it, and an abstention must not tip the
		// dominant or beat not_recommended in the worst-quality fold.
		if v := strings.TrimSpace(meta.ShotSize); v != "" && v != "unknown" {
			sizeCounts[v]++
			sizeOrder = append(sizeOrder, v)
		}
		if v := strings.TrimSpace(meta.CameraMotion); v != "" && v != "unknown" {
			motionCounts[v]++
			motionOrder = append(motionOrder, v)
		}
		if v := strings.TrimSpace(meta.Quality); v != "" && v != "unknown" {
			if rank := qualityRank(v); rank > worst {
				worst = rank
				worstValue = v
			}
		}
		for _, u := range meta.UsableAs {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			if _, ok := seenUse[u]; ok {
				continue
			}
			seenUse[u] = struct{}{}
			useOrder = append(useOrder, u)
		}
	}
	if v := dominant(sizeCounts, sizeOrder); v != "" {
		a.ShotSize = v
	}
	if v := dominant(motionCounts, motionOrder); v != "" {
		a.CameraMotion = v
	}
	if worstValue != "" {
		a.Quality = worstValue
	}
	if len(useOrder) > 0 {
		a.UsableAs = useOrder
	}
}

// qualityRank orders the quality vocabulary from best to worst; unknown
// carries no verdict and is skipped by the caller.
func qualityRank(value string) int {
	for i, allowed := range normalize.QualityValues {
		if allowed == value {
			return i
		}
	}
	return -1
}

func dominant(counts map[string]int, order []string) string {
	best := ""
	bestCount := 0
	for _, v := range order {
		if counts[v] > bestCount {
			best, bestCount = v, counts[v]
		}
	}
	return best
}

func multiframeRequestJSON(assetID, path, detector, sampler string) string {
	payload, _ := json.Marshal(map[string]any{
		"asset_id":      assetID,
		"path":          path,
		"shot_detector": detector,
		"frame_sampler": sampler,
		"protocol":      "openai_multiframe",
	})
	return string(payload)
}

// shotsToBounds extracts boundaries from canonical shot rows, which is how
// the two-pass path feeds pass-1 results back into refinement.
func shotsToBounds(shots []domain.AssetShot) []shotdetect.ShotBound {
	bounds := make([]shotdetect.ShotBound, 0, len(shots))
	for _, shot := range shots {
		bounds = append(bounds, shotdetect.ShotBound{StartMS: shot.StartMS, EndMS: shot.EndMS})
	}
	return bounds
}
