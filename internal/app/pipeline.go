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

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/idgen"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/providerchannels"
	"github.com/evjohn-icu/timingdex/internal/providerpool"
	"github.com/evjohn-icu/timingdex/internal/providers"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
	"github.com/evjohn-icu/timingdex/internal/providers/shotdetect"
	videoproviders "github.com/evjohn-icu/timingdex/internal/providers/video"
	"github.com/evjohn-icu/timingdex/internal/staging"
)

type PipelineRepository interface {
	GetPrimaryLocation(context.Context, string) (domain.AssetLocation, error)
	SaveMediaMetadata(context.Context, string, domain.MediaMetadata, string) error
	GetMediaMetadata(context.Context, string) (*domain.MediaMetadata, error)
	SaveArtifact(context.Context, domain.DerivedArtifact, string, string) error
	GetArtifact(context.Context, string, string) (*domain.DerivedArtifact, error)
	SaveSpeechClassification(context.Context, string, domain.SpeechClassification) error
	EnqueueJob(context.Context, string, domain.JobType, string, int) error
	LeaseNextJob(context.Context, string, func(domain.JobType) time.Duration, domain.LeaseFilter) (*domain.Job, error)
	// The string after id on each of these four is owner: the identity
	// RunUntilIdle minted for this pipeline run and passed to LeaseNextJob.
	// Every one of them now carries a compare-and-swap predicate mirroring
	// CompleteWorkerJob (remote_jobs.go) -- see repository.go's comments on
	// CompleteJob and jobLeaseActive for why an unconditional write here was
	// reachable, not theoretical: the Hub-local pipeline leases for a fixed,
	// never-renewed 2 minutes and runs synchronous work that routinely
	// exceeds it.
	CompleteJob(context.Context, string, string, domain.JobState, string) error
	RetryJob(context.Context, string, string, string, time.Duration) error
	DeferJob(context.Context, string, string, time.Time, string, string) error
	FailJobTerminally(context.Context, string, string, string) error
	RequeueFailedJobs(context.Context) (int, error)
	ResumeDeferredJobs(context.Context, string) (int, error)
	GetPipelineThrottle(context.Context) (domain.PipelineThrottle, error)
	ListJobs(context.Context, int) ([]domain.Job, error)
	JobSummary(context.Context) (domain.JobSummary, error)
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
	GetAlignmentWords(context.Context, string) ([]domain.AlignmentWord, error)
	GetProviderFile(context.Context, string, string, string, string) (*domain.ProviderFile, error)
	SaveProviderFile(context.Context, domain.ProviderFile) error
	SaveAlignment(context.Context, string, string, string, string, string, domain.AlignmentResult) error
	EnqueueReanalysis(context.Context, string, string) error
	ListAssetShots(context.Context, string) ([]domain.AssetShot, error)
	MarkModelRunCommitted(context.Context, string) error
}

type Pipeline struct {
	repo          PipelineRepository
	cacheDir      string
	asr           providers.ASR
	asrFallback   providers.ASR
	videoProvider videoproviders.VideoUnderstandingProvider
	alignment     providers.Alignment
	shotDetector  shotdetect.Detector
	hardware      media.HardwarePlan
	sourceStager  *staging.SourceStager
	// routeDeferral is how long a job is parked when every key on its route is
	// failing at once. It is resolved once at construction from configuration
	// so RunUntilIdle never reads config itself.
	routeDeferral time.Duration
}

func NewPipeline(repo PipelineRepository, cacheDir string, asr providers.ASR, asrFallback providers.ASR, videoProvider videoproviders.VideoUnderstandingProvider, alignment providers.Alignment, shotDetector shotdetect.Detector, hardware media.HardwarePlan, sourceStager *staging.SourceStager, deferral time.Duration) *Pipeline {
	// A configured deferral of zero or less is a typo, not a request to park
	// for no time at all — see minProviderRouteDeferral for why that matters.
	// Floored here, at the point of use, exactly as the supervisor floors its
	// interval, so no caller needs to remember to validate the value.
	if deferral <= 0 {
		deferral = providerRouteDeferral
	}
	if deferral < minProviderRouteDeferral {
		deferral = minProviderRouteDeferral
	}
	return &Pipeline{repo: repo, cacheDir: cacheDir, asr: asr, asrFallback: asrFallback, videoProvider: videoProvider, alignment: alignment, shotDetector: shotDetector, hardware: hardware, sourceStager: sourceStager, routeDeferral: deferral}
}

func (p *Pipeline) EnqueueAsset(ctx context.Context, assetID string) error {
	loc, err := p.repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		return err
	}
	h := hashStrings(loc.AbsolutePath, fmt.Sprint(loc.ModifiedNS))
	return p.repo.EnqueueJob(ctx, assetID, domain.JobProbe, h, 100)
}

// maxConsecutiveLeaseErrors bounds how many times RunUntilIdle retries a
// failing lease query before returning the error.  The original behaviour
// (return immediately) was the right default for a database that should be
// local and healthy; this cap restores that semantic while tolerating
// transient blips.
const maxConsecutiveLeaseErrors = 3

// leaseTTLByType gives each stage a lease ceiling it can plausibly outlive.
// The old flat 2-minute lease was routinely exceeded by derive encodes,
// windowed analysis and ASR calls, so a second process (a paired Worker, or
// `timingdex serve` alongside `timingdex pipeline run`) reclaimed the job
// mid-flight and both executors burned the same paid work. Correctness never
// broke -- every completion write is CAS-protected -- but cost doubled. The
// ceiling is a heuristic: a lease that still expires is simply reclaimed,
// exactly as before.
var leaseTTLByType = map[domain.JobType]time.Duration{
	domain.JobProbe:      2 * time.Minute,
	domain.JobDerive:     30 * time.Minute,
	domain.JobTranscribe: 15 * time.Minute,
	domain.JobAnalyze:    20 * time.Minute,
}

func leaseTTL(typ domain.JobType) time.Duration {
	if ttl, ok := leaseTTLByType[typ]; ok {
		return ttl
	}
	return 2 * time.Minute
}

func (p *Pipeline) RunUntilIdle(ctx context.Context) (int, error) {
	worker := "local-" + idgen.New()
	executed := 0
	consecutiveLeaseErrors := 0
	for {
		if err := ctx.Err(); err != nil {
			return executed, err
		}
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
		job, err := p.repo.LeaseNextJob(ctx, worker, leaseTTL, domain.LeaseFilter{MaxAssetBytes: throttle.MaxAssetBytesAt(now)})
		if err != nil {
			consecutiveLeaseErrors++
			if consecutiveLeaseErrors%5 == 1 || consecutiveLeaseErrors == 1 {
				slog.Warn("pipeline lease query failing; will retry", "consecutive_errors", consecutiveLeaseErrors, "error", err)
			}
			if consecutiveLeaseErrors >= maxConsecutiveLeaseErrors {
				return executed, fmt.Errorf("pipeline lease query failed %d consecutive times, last error: %w", consecutiveLeaseErrors, err)
			}
			if sleepErr := sleepContext(ctx, 500*time.Millisecond); sleepErr != nil {
				return executed, sleepErr
			}
			continue
		}
		consecutiveLeaseErrors = 0
		if job == nil {
			return executed, nil
		}
		// Counted the moment the job leaves the queue, whether it runs to
		// completion, retries, or is deferred on a dead route: the queue
		// changed either way, which is all the caller needs to know.
		executed++
		if err := p.execute(ctx, *job, worker, throttle); err != nil {
			// A stale execution: something else's compare-and-swap already won
			// this job (a paired Worker on `derive`, or a second Hub process --
			// `timingdex serve` and `timingdex pipeline run` are both
			// documented, and the in-process single-pass mutex at service.go
			// cannot span processes). Every completion write below is now
			// CAS-protected, so attempting one under our own (no-longer-valid)
			// owner id would just be a guaranteed second miss. There is nothing
			// left to do with this job; move on instead of treating expected
			// contention as fatal to the whole run.
			if isLeaseLostErr(err) {
				slog.Warn("job lease reclaimed by another holder; discarding stale execution", "job", job.ID, "job_type", job.Type)
				continue
			}
			// Every key on the capability's route is failing. On the
			// recommended plan that is a spent monthly quota — nothing about
			// this job is wrong and nothing about it will be different in one
			// second, so it is parked on wall-clock time and the attempt this
			// lease consumed is handed back. Retrying instead would spend the
			// job's whole budget inside ten seconds and fail it permanently
			// for an outage, and each of those attempts is a paid call.
			if errors.Is(err, providerchannels.ErrRouteExhausted) {
				resumeAt := time.Now().Add(p.routeDeferral)
				if deferErr := p.repo.DeferJob(ctx, job.ID, worker, resumeAt, domain.JobDeferProviderRouteExhausted, err.Error()); deferErr != nil {
					if isLeaseLostErr(deferErr) {
						slog.Warn("job lease reclaimed before it could be deferred; discarding", "job", job.ID, "job_type", job.Type)
						continue
					}
					return executed, deferErr
				}
				slog.Warn("provider route exhausted; job deferred", "job", job.ID, "job_type", job.Type, "resume_at", resumeAt.Format(time.RFC3339))
				continue
			}
			if isRetryableJobError(err) && job.AttemptCount < job.MaxAttempts {
				if retryErr := p.repo.RetryJob(ctx, job.ID, worker, err.Error(), retryDelay(job.AttemptCount)); retryErr != nil {
					if isLeaseLostErr(retryErr) {
						slog.Warn("job lease reclaimed before it could be retried; discarding", "job", job.ID, "job_type", job.Type)
						continue
					}
					return executed, retryErr
				}
				continue
			}
			// Either the failure is permanent or the attempts ran out. Both are
			// terminal, so stop the lease predicate from handing it back. The
			// lease-lost case is handled above before execute's error even
			// reaches here; any other error from this call is swallowed exactly
			// as before, since it already sat on a best-effort path.
			_ = p.repo.FailJobTerminally(ctx, job.ID, worker, err.Error())
			if err := sleepContext(ctx, throttle.CooldownAt(time.Now())); err != nil {
				return executed, err
			}
			continue
		}
		if err := p.repo.CompleteJob(ctx, job.ID, worker, domain.JobSucceeded, ""); err != nil {
			if !isLeaseLostErr(err) {
				return executed, err
			}
			// The work finished, but the lease was already reclaimed while it
			// ran -- the same race, just lost after success instead of during
			// it. The new holder's attempt is the one of record now; this
			// result is discarded rather than overwriting it.
			slog.Warn("job completed locally but lease was already reclaimed; discarding stale result", "job", job.ID, "job_type", job.Type)
			continue
		}
		// The pause is what turns a multi-hour scan from continuous disk load
		// into duty-cycled load; a rate cap alone still reads flat-out forever.
		if err := sleepContext(ctx, throttle.CooldownAt(time.Now())); err != nil {
			return executed, err
		}
	}
}

// isLeaseLostErr reports whether err is the compare-and-swap miss
// repository.go's leaseLostErr produces (CompleteJob, FailJobTerminally,
// RetryJob, DeferJob, SaveArtifact). Matched against domain.ErrJobLeaseLost
// with errors.Is, not by message: this package and internal/repository/sqlite
// both already import internal/domain (PipelineRepository, declared here at
// the consumer, stays free of a concrete dependency on the sqlite package
// either way), so the sentinel costs nothing to share and, unlike a
// substring, survives leaseLostErr's message being reworded. See
// domain.ErrJobLeaseLost's doc comment for why that distinction matters here
// specifically: a drifted string would make RunUntilIdle either abort a run
// over ordinary contention or swallow a genuine failure as if it were.
func isLeaseLostErr(err error) bool {
	return errors.Is(err, domain.ErrJobLeaseLost)
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

// providerRouteDeferral is the default wait after every provider key on a
// route has failed. It is sized for the failure it exists for: the recommended
// plan's quota is monthly and hard, so there is no point asking again in
// seconds, and a handful of probe calls a day costs nothing while still
// recovering on its own once the account is topped up or the month rolls over.
// It is deliberately not a retry backoff — DeferJob spends no attempt — so the
// queue never permanently fails work over an empty account. Operators on a
// different plan shorten it through config.pipeline.provider_route_deferral_minutes
// without rebuilding; this constant is what NewPipeline falls back to when
// configuration yields no usable value.
const providerRouteDeferral = 5 * time.Hour

// minProviderRouteDeferral floors a mistyped deferral, the same way
// LibrarySupervisor floors its scan interval. The retry backoff for an ordinary
// failure already tops out at 30 seconds, so a deferral below a minute would
// ask an exhausted route more often than we ask a live one that is merely
// rate-limited — the park would read as a retry. Zero or negative is worse
// still: the job re-arms instantly, so RunUntilIdle spins on the dead route,
// making a paid provider call per pass, which is the exact failure the park
// exists to prevent.
const minProviderRouteDeferral = time.Minute

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
	// A 4xx is deterministic *for the member that answered it*: a missing
	// model, or a request the provider will reject identically every time.
	// Retrying those burns paid quota without changing the outcome. 408 and
	// 429 are the two that clear on their own, so they keep the backoff path.
	//
	// 401, 402 and 403 keep it too, but for a different reason: providerpool
	// classifies them MemberSpent -- they describe the key, not the request --
	// and Pool.complete has already retired that member by the time this
	// error is visible here (see providerpool.ClassifyFailure). The executor
	// normally turns a route with nothing left into
	// providerchannels.ErrRouteExhausted before it ever reaches this
	// function; that case is handled above, by parking rather than retrying.
	// It cannot do that when the last untried member is merely saturated by a
	// concurrent caller: Pool.Select returns ErrNoAvailable without an
	// attempt, providerchannels.Executor.routeFailingEverywhere correctly
	// declines to call an unattempted member exhausted, and the raw
	// MemberSpent error surfaces here instead of the sentinel. Retrying is
	// right either way it got here: on the executor's own conclusion there
	// truly is nothing left, backoff burns only a few attempts before this
	// job's normal terminal path takes over; on the saturation race, the very
	// next attempt selects the member the pool retired around, once
	// selection succeeds at all. Classification is by status alone, not by
	// what the provider's body says, so an upstream message that happens to
	// read as permanent ("unauthorized", "not configured") cannot flip this
	// back to a fail-permanent outcome for a key the pool has already moved
	// on from.
	var status *common.StatusError
	if errors.As(err, &status) && status.StatusCode >= 400 && status.StatusCode < 500 {
		if status.StatusCode != http.StatusRequestTimeout && status.StatusCode != http.StatusTooManyRequests {
			if providerpool.ClassifyFailure(err) == providerpool.MemberSpent {
				return true
			}
			return false
		}
	}
	// Everything else answers for itself. A failure that a retry cannot change
	// is marked at the line that creates it — see domain.ErrPermanentFailure
	// for why that replaced a list of message substrings kept here, and why an
	// unmarked error is still retried. providerchannels.ErrNoRoute and
	// providerpool.ErrNoAvailable are deliberately unmarked: those clear on
	// their own once a cooldown expires.
	return !errors.Is(err, domain.ErrPermanentFailure)
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

func (p *Pipeline) execute(ctx context.Context, j domain.Job, worker string, throttle domain.PipelineThrottle) error {
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
		if err := saveArtifact(p.repo, ctx, j.AssetID, "thumbnail", "thumb-"+p.hardware.Profile(), thumb, j.ID, worker); err != nil {
			return err
		}
		if err := saveArtifact(p.repo, ctx, j.AssetID, "proxy", "proxy-720-"+p.hardware.Profile(), proxy, j.ID, worker); err != nil {
			return err
		}
		if m != nil && m.HasAudio {
			audio := filepath.Join(base, "audio.m4a")
			if _, err := os.Stat(audio); os.IsNotExist(err) {
				if err := media.ExtractAudio(ctx, sourcePath, audio, readRate); err != nil {
					return err
				}
			}
			if err := saveArtifact(p.repo, ctx, j.AssetID, "audio", "audio-16k-v1", audio, j.ID, worker); err != nil {
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
		// This guard and its siblings below (metadata, transcript, proxy) all
		// report that a predecessor left something out. They are permanent
		// because the chain only ever enqueues successors — nothing a retry
		// does re-runs the stage that was supposed to produce this — so every
		// attempt reads the same absent row and the backoff buys nothing but
		// delay before an operator sees it.
		if a == nil {
			return domain.Permanent(fmt.Errorf("audio artifact missing"))
		}
		metadata, err := p.repo.GetMediaMetadata(ctx, j.AssetID)
		if err != nil {
			return err
		}
		if metadata == nil || metadata.DurationMS <= 0 {
			return domain.Permanent(fmt.Errorf("media duration missing for speech gate"))
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
			return domain.Permanent(fmt.Errorf("audio artifact missing"))
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
			return domain.Permanent(fmt.Errorf("audio artifact missing"))
		}
		t, err := p.repo.GetTranscript(ctx, j.AssetID)
		if err != nil {
			return err
		}
		if t == nil || t.Text == "" {
			return domain.Permanent(fmt.Errorf("transcript missing"))
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
			return domain.Permanent(fmt.Errorf("video analysis provider is not configured; set providers.vision_primary to an enabled provider"))
		}
		m, err := p.repo.GetMediaMetadata(ctx, j.AssetID)
		if err != nil {
			return err
		}
		if m == nil {
			return domain.Permanent(fmt.Errorf("metadata missing"))
		}
		route := p.multiframeRouteOf()
		if route != nil {
			if p.shotDetector != nil {
				return p.analyzeWithDetector(ctx, &j, m, route)
			}
			if route.video != nil {
				return p.analyzeTwoPass(ctx, &j, m, route)
			}
			// A frame-only endpoint cannot produce its own boundaries and no
			// fallback can either: the deployment is missing one of the two
			// things the multiframe path needs. Deterministic config, same
			// answer on retry — permanent, with the remedy spelled out.
			return domain.Permanent(fmt.Errorf("multiframe video provider %q requires a shot detector (providers.shot_detection) or a video-capable fallback provider (providers.vision_fallback)", route.analyzer.Name()))
		}
		return p.analyzeAssetVideo(ctx, &j, m, p.videoProvider, sourcePath)
	case domain.JobIndex:
		return p.repo.RebuildSearch(ctx, j.AssetID)
	default:
		return domain.Permanent(fmt.Errorf("unsupported job type %s", j.Type))
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

// validateAnalysisShots rejects a model answer that cannot be committed. Every
// rejection below is a verdict on bytes the Hub already holds and already paid
// for, so the next attempt would put the same input to the same decision and
// be told the same thing — at the price of another provider call. That is
// marked here, in a wrapper over the checks rather than on each check, so a
// rule added to analysisShotsProblem is permanent the moment it is written;
// the alternative was a phrase in another package that two of the four checks
// below never got. See domain.ErrPermanentFailure.
func validateAnalysisShots(shots []domain.AssetShot, durationMS int64) error {
	if err := analysisShotsProblem(shots, durationMS); err != nil {
		return domain.Permanent(err)
	}
	return nil
}

func analysisShotsProblem(shots []domain.AssetShot, durationMS int64) error {
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

func saveArtifact(repo PipelineRepository, ctx context.Context, assetID, typ, profile, path, jobID, owner string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	return repo.SaveArtifact(ctx, domain.DerivedArtifact{ID: idgen.New(), AssetID: assetID, Type: typ, ProfileHash: profile, LocalPath: path, SizeBytes: st.Size()}, jobID, owner)
}
func hashStrings(v ...string) string {
	h := sha256.New()
	for _, s := range v {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
