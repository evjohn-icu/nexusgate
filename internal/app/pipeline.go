package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ev/timingdex/internal/domain"
	videoanalysis "github.com/ev/timingdex/internal/domain/video_analysis"
	"github.com/ev/timingdex/internal/idgen"
	"github.com/ev/timingdex/internal/media"
	"github.com/ev/timingdex/internal/normalize"
	"github.com/ev/timingdex/internal/providers"
	"github.com/ev/timingdex/internal/providers/common"
	videoproviders "github.com/ev/timingdex/internal/providers/video"
	"github.com/ev/timingdex/internal/staging"
)

type PipelineRepository interface {
	GetPrimaryLocation(context.Context, string) (domain.AssetLocation, error)
	SaveMediaMetadata(context.Context, string, domain.MediaMetadata, string) error
	GetMediaMetadata(context.Context, string) (*domain.MediaMetadata, error)
	SaveArtifact(context.Context, domain.DerivedArtifact) error
	GetArtifact(context.Context, string, string) (*domain.DerivedArtifact, error)
	SaveSpeechClassification(context.Context, string, domain.SpeechClassification) error
	EnqueueJob(context.Context, string, domain.JobType, string, int) error
	LeaseNextJob(context.Context, string, time.Duration, domain.LeaseFilter) (*domain.Job, error)
	CompleteJob(context.Context, string, domain.JobState, string) error
	RetryJob(context.Context, string, string, time.Duration) error
	FailJobTerminally(context.Context, string, string) error
	RequeueFailedJobs(context.Context) (int, error)
	GetPipelineThrottle(context.Context) (domain.PipelineThrottle, error)
	ListJobs(context.Context, int) ([]domain.Job, error)
	RebuildSearch(context.Context, string) error
	Search(context.Context, string, int) ([]string, error)
	CreateModelRun(context.Context, string, string, string, string, string, string, string, string) (string, bool, error)
	FailModelRun(context.Context, string, string, string, string) error
	StageModelRun(context.Context, string, string, string) error
	CommitAnalysis(context.Context, string, string, string, domain.StructuredAnalysis) error
	CommitAnalysisWithShots(context.Context, string, string, string, domain.StructuredAnalysis, []domain.AssetShot) error
	SyncAnalysisTags(context.Context, string, string, domain.StructuredAnalysis) error
	ReplaceAssetShots(context.Context, string, string, []domain.AssetShot) error
	GetSpeechClassification(context.Context, string) (*domain.SpeechClassification, error)
	SaveTranscript(context.Context, string, string, string, string, domain.Transcript) error
	GetTranscript(context.Context, string) (*domain.Transcript, error)
	GetProviderFile(context.Context, string, string, string, string) (*domain.ProviderFile, error)
	SaveProviderFile(context.Context, domain.ProviderFile) error
	SaveAlignment(context.Context, string, string, string, string, string, domain.AlignmentResult) error
}

type Pipeline struct {
	repo          PipelineRepository
	cacheDir      string
	asr           providers.ASR
	asrFallback   providers.ASR
	videoProvider videoproviders.VideoUnderstandingProvider
	alignment     providers.Alignment
	hardware      media.HardwarePlan
	sourceStager  *staging.SourceStager
}

func NewPipeline(repo PipelineRepository, cacheDir string, asr providers.ASR, asrFallback providers.ASR, videoProvider videoproviders.VideoUnderstandingProvider, alignment providers.Alignment, hardware media.HardwarePlan, sourceStager *staging.SourceStager) *Pipeline {
	return &Pipeline{repo: repo, cacheDir: cacheDir, asr: asr, asrFallback: asrFallback, videoProvider: videoProvider, alignment: alignment, hardware: hardware, sourceStager: sourceStager}
}

func (p *Pipeline) EnqueueAsset(ctx context.Context, assetID string) error {
	loc, err := p.repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		return err
	}
	h := hashStrings(loc.AbsolutePath, fmt.Sprint(loc.ModifiedNS))
	return p.repo.EnqueueJob(ctx, assetID, domain.JobProbe, h, 100)
}

func (p *Pipeline) RunUntilIdle(ctx context.Context) error {
	worker := "local-" + idgen.New()
	for {
		// Re-read the throttle each iteration rather than once per run. A run
		// can last hours, which is longer than the off-peak window it is
		// supposed to respect, and the operator must be able to tighten the
		// limit while a scan is already grinding.
		throttle, err := p.repo.GetPipelineThrottle(ctx)
		if err != nil {
			slog.Warn("pipeline throttle unreadable; running unthrottled", "error", err)
			throttle = domain.DefaultPipelineThrottle()
		}
		now := time.Now()
		job, err := p.repo.LeaseNextJob(ctx, worker, 2*time.Minute, domain.LeaseFilter{MaxAssetBytes: throttle.MaxAssetBytesAt(now)})
		if err != nil {
			return err
		}
		if job == nil {
			return nil
		}
		if err := p.execute(ctx, *job, throttle); err != nil {
			if isRetryableJobError(err) && job.AttemptCount < job.MaxAttempts {
				if retryErr := p.repo.RetryJob(ctx, job.ID, err.Error(), retryDelay(job.AttemptCount)); retryErr != nil {
					return retryErr
				}
				continue
			}
			// Either the failure is permanent or the attempts ran out. Both are
			// terminal, so stop the lease predicate from handing it back.
			_ = p.repo.FailJobTerminally(ctx, job.ID, err.Error())
			if err := sleepContext(ctx, throttle.CooldownAt(time.Now())); err != nil {
				return err
			}
			continue
		}
		if err := p.repo.CompleteJob(ctx, job.ID, domain.JobSucceeded, ""); err != nil {
			return err
		}
		// The pause is what turns a multi-hour scan from continuous disk load
		// into duty-cycled load; a rate cap alone still reads flat-out forever.
		if err := sleepContext(ctx, throttle.CooldownAt(time.Now())); err != nil {
			return err
		}
	}
}

// sleepContext waits without outliving a cancelled run. A plain time.Sleep here
// would make Ctrl-C on a throttled pipeline take up to a full cooldown to be
// noticed.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return time.Second
	}
	delay := time.Second << min(attempt-1, 5)
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func isRetryableJobError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A 4xx is deterministic: an invalid key, a missing model, or a request the
	// provider will reject identically every time. Retrying it burns paid quota
	// without changing the outcome. 408 and 429 are the two that do clear on
	// their own, so they keep the backoff path.
	var status *common.StatusError
	if errors.As(err, &status) && status.StatusCode >= 400 && status.StatusCode < 500 {
		if status.StatusCode != http.StatusRequestTimeout && status.StatusCode != http.StatusTooManyRequests {
			return false
		}
	}
	// Everything below is config-shaped: a name that maps to no implementation, a
	// member an operator switched off, a capability the adapter lacks. None of it
	// reaches the network, so retrying only delays surfacing a fixable mistake.
	// "no available route"/"no available member" are deliberately absent — those
	// clear once a cooldown expires.
	message := strings.ToLower(err.Error())
	for _, permanent := range []string{
		"not configured", "validation_error", "validation error", "unsupported ",
		"is disabled", "does not support remote file preparation",
		"metadata missing", "proxy artifact missing", "audio artifact missing", "transcript missing",
		"media duration missing", "invalid shot", "summary is required",
	} {
		if strings.Contains(message, permanent) {
			return false
		}
	}
	return true
}

// previewPlanForDerive picks the preview render plan JobDerive needs for its
// thumbnail/proxy renders. JobProbe already classified the source and saved
// the result as MediaMetadata.SourceColor, and SelectPreviewRenderPlan is a
// pure function of that one value, so the common case reconstructs the plan
// for free. Only metadata predating this field (or a missing row) falls back
// to a single fresh probe here, shared by both renders instead of one probe
// each.
func previewPlanForDerive(ctx context.Context, sourcePath string, m *domain.MediaMetadata) (media.PreviewRenderPlan, error) {
	if m != nil && m.SourceColor != "" {
		return media.SelectPreviewRenderPlan(media.SourceColorClass(m.SourceColor)), nil
	}
	probe, err := media.Probe(ctx, sourcePath)
	return media.PreviewPlanForProbeResult(probe, err, sourcePath), nil
}

// sourceSizeBytes reports the source size for the throttle's small-file
// exemption. An unreadable file returns 0, which the exemption treats as
// "unknown" and therefore throttles — failing towards the gentler behaviour.
func sourceSizeBytes(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (p *Pipeline) execute(ctx context.Context, j domain.Job, throttle domain.PipelineThrottle) error {
	loc, err := p.repo.GetPrimaryLocation(ctx, j.AssetID)
	if err != nil {
		return err
	}
	sourcePath, err := p.sourcePathForJob(ctx, j.Type, loc)
	if err != nil {
		return err
	}
	switch j.Type {
	case domain.JobProbe:
		probe, err := media.Probe(ctx, sourcePath)
		if err != nil {
			return err
		}
		exif, err := media.ReadExif(ctx, sourcePath)
		if err != nil {
			exif = map[string]any{"warning": err.Error()}
		}
		m := media.NormalizeMetadata(probe, exif)
		if err := p.repo.SaveMediaMetadata(ctx, j.AssetID, m, "ffprobe-exif-v1"); err != nil {
			return err
		}
		return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobDerive, hashStrings(j.InputHash, "derive-v1"), 90)
	case domain.JobDerive:
		base := filepath.Join(p.cacheDir, j.AssetID)
		// Keep profiles in separate cache paths: a proxy created by software x264
		// must never be relabelled as an NVENC/QSV/VideoToolbox result.
		thumb := filepath.Join(base, "thumbnail-"+p.hardware.Mode+".jpg")
		proxy := filepath.Join(base, "proxy-"+p.hardware.Mode+".mp4")
		_, thumbStatErr := os.Stat(thumb)
		_, proxyStatErr := os.Stat(proxy)
		needThumb := os.IsNotExist(thumbStatErr)
		needProxy := os.IsNotExist(proxyStatErr)

		m, err := p.repo.GetMediaMetadata(ctx, j.AssetID)
		if err != nil {
			return err
		}

		// JobProbe already ran ffprobe once for this asset and persisted the
		// resulting SourceColor; reuse it instead of probing again here so a
		// thumbnail+proxy derive costs at most one extra ffprobe call, and only
		// when the stored metadata predates this field or is missing.
		// The rate cap is chosen per asset so the small-file exemption can
		// apply. Source size comes from a stat rather than the database because
		// the throttle protects the disk this read is about to hit, and that is
		// the file on disk right now.
		readRate := throttle.ReadRateFor(sourceSizeBytes(sourcePath))
		if needThumb || needProxy {
			previewPlan, err := previewPlanForDerive(ctx, sourcePath, m)
			if err != nil {
				return err
			}
			renderer := media.NewPreviewRenderer("").WithReadRate(readRate)
			if needThumb {
				if err := renderer.RenderThumbnail(ctx, sourcePath, thumb, p.hardware, previewPlan); err != nil {
					return err
				}
			}
			if needProxy {
				if err := renderer.RenderProxy(ctx, sourcePath, proxy, p.hardware, previewPlan); err != nil {
					return err
				}
			}
		}
		if err := saveArtifact(p.repo, ctx, j.AssetID, "thumbnail", "thumb-"+p.hardware.Profile(), thumb); err != nil {
			return err
		}
		if err := saveArtifact(p.repo, ctx, j.AssetID, "proxy", "proxy-720-"+p.hardware.Profile(), proxy); err != nil {
			return err
		}
		if m != nil && m.HasAudio {
			audio := filepath.Join(base, "audio.m4a")
			if _, err := os.Stat(audio); os.IsNotExist(err) {
				if err := media.ExtractAudio(ctx, sourcePath, audio, readRate); err != nil {
					return err
				}
			}
			if err := saveArtifact(p.repo, ctx, j.AssetID, "audio", "audio-16k-v1", audio); err != nil {
				return err
			}
			return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobSpeechGate, hashStrings(j.InputHash, "speech-v1"), 80)
		}
		return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobAnalyze, hashStrings(j.InputHash, "analyze-v1"), 30)
	case domain.JobSpeechGate:
		a, err := p.repo.GetArtifact(ctx, j.AssetID, "audio")
		if err != nil {
			return err
		}
		if a == nil {
			return fmt.Errorf("audio artifact missing")
		}
		metadata, err := p.repo.GetMediaMetadata(ctx, j.AssetID)
		if err != nil {
			return err
		}
		if metadata == nil || metadata.DurationMS <= 0 {
			return fmt.Errorf("media duration missing for speech gate")
		}
		c, err := media.SpeechGate(ctx, a.LocalPath, metadata.DurationMS)
		if err != nil {
			return err
		}
		if err := p.repo.SaveSpeechClassification(ctx, j.AssetID, c); err != nil {
			return err
		}
		if c.SpeechProbability >= 0.5 && p.asr != nil {
			return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobTranscribe, hashStrings(j.InputHash, p.asr.Name(), p.asr.Model()), 60)
		}
		return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobAnalyze, hashStrings(j.InputHash, "analyze-v1"), 30)
	case domain.JobTranscribe:
		a, err := p.repo.GetArtifact(ctx, j.AssetID, "audio")
		if err != nil {
			return err
		}
		if a == nil {
			return fmt.Errorf("audio artifact missing")
		}
		t, err := p.asr.Transcribe(ctx, providers.TranscribeRequest{AudioPath: a.LocalPath, Language: "zh"})
		providerUsed := p.asr
		if err != nil && p.asrFallback != nil {
			t, err = p.asrFallback.Transcribe(ctx, providers.TranscribeRequest{AudioPath: a.LocalPath, Language: "zh"})
			providerUsed = p.asrFallback
		}
		if err != nil {
			return err
		}
		if err := p.repo.SaveTranscript(ctx, j.AssetID, providerUsed.Name(), providerUsed.Model(), j.InputHash, t); err != nil {
			return err
		}
		if p.alignment != nil {
			return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobAlign, hashStrings(j.InputHash, p.alignment.Name(), p.alignment.Model()), 50)
		}
		return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobAnalyze, hashStrings(j.InputHash, "analyze-v1"), 30)
	case domain.JobAlign:
		a, err := p.repo.GetArtifact(ctx, j.AssetID, "audio")
		if err != nil {
			return err
		}
		if a == nil {
			return fmt.Errorf("audio artifact missing")
		}
		t, err := p.repo.GetTranscript(ctx, j.AssetID)
		if err != nil {
			return err
		}
		if t == nil || t.Text == "" {
			return fmt.Errorf("transcript missing")
		}
		result, err := p.alignment.Align(ctx, providers.AlignRequest{AudioPath: a.LocalPath, Transcript: *t, Language: t.Language})
		if err != nil {
			return err
		}
		req, _ := json.Marshal(map[string]any{"audio_path": a.LocalPath, "text": t.Text, "language": t.Language})
		if err := p.repo.SaveAlignment(ctx, j.AssetID, p.alignment.Name(), p.alignment.Model(), j.InputHash, string(req), result); err != nil {
			return err
		}
		return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobAnalyze, hashStrings(j.InputHash, "analyze-v1"), 30)
	case domain.JobAnalyze:
		if p.videoProvider == nil {
			return fmt.Errorf("video analysis provider is not configured; set providers.vision_primary to an enabled provider")
		}
		m, err := p.repo.GetMediaMetadata(ctx, j.AssetID)
		if err != nil {
			return err
		}
		if m == nil {
			return fmt.Errorf("metadata missing")
		}
		reqJSON := fmt.Sprintf(`{"asset_id":%q,"path":%q}`, j.AssetID, sourcePath)
		providerName, modelName, promptVersion := p.videoProvider.Name(), p.videoProvider.Model(), "footage-analysis-v3"
		runID, cached, err := p.repo.CreateModelRun(ctx, j.AssetID, "vision", providerName, modelName, j.InputHash, promptVersion, "asset-analysis/v1", reqJSON)
		if err != nil {
			return err
		}
		if !cached {
			var a domain.StructuredAnalysis
			var raw string
			proxy, e := p.repo.GetArtifact(ctx, j.AssetID, "proxy")
			if e != nil {
				return e
			}
			if proxy == nil {
				return fmt.Errorf("proxy artifact missing")
			}
			transcript, e := p.repo.GetTranscript(ctx, j.AssetID)
			if e != nil {
				return e
			}
			analyzeReq := videoanalysis.Input{VideoPath: proxy.LocalPath, Transcript: transcript, Metadata: *m}
			requiresPreparation := false
			if preparation, ok := p.videoProvider.(interface{ RequiresVideoPreparation() bool }); ok {
				requiresPreparation = preparation.RequiresVideoPreparation()
			} else if _, ok := p.videoProvider.(videoproviders.VideoPreparer); ok {
				requiresPreparation = true
			}
			if requiresPreparation {
				preparer, ok := p.videoProvider.(videoproviders.VideoPreparer)
				if !ok {
					return fmt.Errorf("video provider %q requires preparation but cannot prepare video", p.videoProvider.Name())
				}
				cachedFile, e := p.repo.GetProviderFile(ctx, j.AssetID, "proxy", proxy.ProfileHash, p.videoProvider.Name())
				if e != nil {
					return e
				}
				if cachedFile == nil || cachedFile.State != "ACTIVE" || (cachedFile.ExpiresAt != nil && cachedFile.ExpiresAt.Before(time.Now())) {
					prepared, e := preparer.PrepareVideo(ctx, videoproviders.PrepareVideoRequest{VideoPath: proxy.LocalPath, DisplayName: j.AssetID + "-proxy.mp4", MIMEType: "video/mp4"})
					if e != nil {
						return e
					}
					cachedFile = &domain.ProviderFile{ID: idgen.New(), AssetID: j.AssetID, ArtifactType: "proxy", ProfileHash: proxy.ProfileHash, Provider: p.videoProvider.Name(), RemoteName: prepared.RemoteName, RemoteURI: prepared.RemoteURI, MIMEType: prepared.MIMEType, State: prepared.State, SizeBytes: prepared.SizeBytes}
					if e := p.repo.SaveProviderFile(ctx, *cachedFile); e != nil {
						return e
					}
				}
				analyzeReq.RemoteURI, analyzeReq.MIMEType = cachedFile.RemoteURI, cachedFile.MIMEType
			}
			result, rawResult, providerErr := p.analyzeVideo(ctx, j.AssetID, analyzeReq, m.DurationMS)
			raw = rawResult
			err = providerErr
			if err != nil {
				_ = p.repo.FailModelRun(ctx, runID, "provider_error", err.Error(), raw)
				return err
			}
			a = result.ToStructuredAnalysis()
			a, err = normalize.ValidateAndNormalize(a)
			if err != nil {
				_ = p.repo.FailModelRun(ctx, runID, "validation_error", err.Error(), "")
				return err
			}
			shots := result.ToAssetShots(j.AssetID, runID)
			if err := validateAnalysisShots(shots, m.DurationMS); err != nil {
				_ = p.repo.FailModelRun(ctx, runID, "validation_error", err.Error(), raw)
				return err
			}
			parsed, _ := json.Marshal(a)
			if err := p.repo.StageModelRun(ctx, runID, raw, string(parsed)); err != nil {
				return err
			}
			if err := p.repo.CommitAnalysisWithShots(ctx, j.AssetID, runID, "asset-analysis/v1", a, shots); err != nil {
				return err
			}
		}
		return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobIndex, hashStrings(j.InputHash, "index-v1"), 10)
	case domain.JobIndex:
		return p.repo.RebuildSearch(ctx, j.AssetID)
	default:
		return fmt.Errorf("unsupported job type %s", j.Type)
	}
}

func (p *Pipeline) sourcePath(ctx context.Context, location domain.AssetLocation) (string, error) {
	if p.sourceStager == nil {
		return location.AbsolutePath, nil
	}
	version := hashStrings(location.AbsolutePath, fmt.Sprint(location.ModifiedNS))
	return p.sourceStager.Stage(ctx, location.AbsolutePath, location.AssetID, version)
}

// sourcePathForJob keeps Hub probe work on the NAS path. Only jobs that need
// to decode or transform the whole source use the disposable Worker/local cache.
func (p *Pipeline) sourcePathForJob(ctx context.Context, typ domain.JobType, location domain.AssetLocation) (string, error) {
	if typ == domain.JobProbe {
		return location.AbsolutePath, nil
	}
	return p.sourcePath(ctx, location)
}

// maxAnalysisShots bounds how many shots one analysis may commit. The per-shot
// checks below validate shape but not cardinality, so a model stuck in a
// repetition loop could otherwise write an unbounded number of rows into
// asset_shots and the FTS index. No genuine single asset comes close.
const maxAnalysisShots = 2000

func validateAnalysisShots(shots []domain.AssetShot, durationMS int64) error {
	if len(shots) > maxAnalysisShots {
		return fmt.Errorf("invalid shot count: %d exceeds limit %d", len(shots), maxAnalysisShots)
	}
	for i, shot := range shots {
		if shot.StartMS < 0 || shot.EndMS <= shot.StartMS {
			return fmt.Errorf("invalid shot time range at ordinal %d: %d-%d", i, shot.StartMS, shot.EndMS)
		}
		if strings.TrimSpace(shot.Description) == "" {
			return fmt.Errorf("shot description is required at ordinal %d", i)
		}
		if durationMS > 0 && shot.EndMS > durationMS {
			return fmt.Errorf("shot at ordinal %d ends after asset duration: %d > %d", i, shot.EndMS, durationMS)
		}
	}
	return nil
}

func saveArtifact(repo PipelineRepository, ctx context.Context, assetID, typ, profile, path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	return repo.SaveArtifact(ctx, domain.DerivedArtifact{ID: idgen.New(), AssetID: assetID, Type: typ, ProfileHash: profile, LocalPath: path, SizeBytes: st.Size()})
}
func hashStrings(v ...string) string {
	h := sha256.New()
	for _, s := range v {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
