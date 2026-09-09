package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/cachecoord"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
	"github.com/evjohn-icu/nexusgate/internal/ingest"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/providerchannels"
	"github.com/evjohn-icu/nexusgate/internal/providerpool"
	"github.com/evjohn-icu/nexusgate/internal/providers"
	"github.com/evjohn-icu/nexusgate/internal/providers/common"
	"github.com/evjohn-icu/nexusgate/internal/providers/shotdetect"
	videoproviders "github.com/evjohn-icu/nexusgate/internal/providers/video"
	"github.com/evjohn-icu/nexusgate/internal/staging"
)

type PipelineRepository interface {
	GetPrimaryLocation(context.Context, string) (domain.AssetLocation, error)
	SaveMediaMetadata(context.Context, string, domain.MediaMetadata, string, string, string) error
	GetMediaMetadata(context.Context, string) (*domain.MediaMetadata, error)
	SaveArtifact(context.Context, domain.DerivedArtifact, string, string) error
	GetArtifact(context.Context, string, string) (*domain.DerivedArtifact, error)
	SaveSpeechClassification(context.Context, string, domain.SpeechClassification, string, string) error
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
	// RetryJob and FailJobTerminally both take the failing attempt's
	// JobFailureCategory alongside the message: the category is written into
	// jobs.last_error_code, the single column the issues view aggregates on,
	// so a job's last failure stays machine-readable even after it stopped
	// being retried. DeferJob's reason already lands in that column and IS a
	// category value (see the JobDefer* constants).
	RetryJob(context.Context, string, string, domain.JobFailureCategory, string, time.Duration) error
	DeferJob(context.Context, string, string, time.Time, string, string) error
	FailJobTerminally(context.Context, string, string, domain.JobFailureCategory, string) error
	RequeueFailedJobs(context.Context) (int, error)
	RequeueFailedJobsByCategory(context.Context, string) (int, error)
	ResumeDeferredJobs(context.Context, string) (int, error)
	GetPipelineThrottle(context.Context) (domain.PipelineThrottle, error)
	// PipelineThrottleConfigured reports whether the settings row exists. It
	// is the difference between "0 = the default" and "0 = the operator
	// explicitly disabled the check": without it, a settings-page 0 could not
	// override a nonzero config floor.
	PipelineThrottleConfigured(context.Context) (bool, error)
	ListJobs(context.Context, int) ([]domain.Job, error)
	JobSummary(context.Context) (domain.JobSummary, error)
	RebuildSearch(context.Context, string) error
	Search(context.Context, string, int) ([]string, error)
	CreateModelRun(context.Context, string, string, string, string, string, string, string, string, string, string) (string, bool, error)
	FailModelRun(context.Context, string, string, string, string, string, string) error
	StageModelRun(context.Context, string, string, string, string, string) error
	CommitAnalysis(context.Context, string, string, string, domain.StructuredAnalysis, string, string) error
	CommitAnalysisWithShots(context.Context, string, string, string, domain.StructuredAnalysis, []domain.AssetShot, string, string) error
	SyncAnalysisTags(context.Context, string, string, domain.StructuredAnalysis) error
	ReplaceAssetShots(context.Context, string, string, []domain.AssetShot, string, string) error
	CommitShotRefinement(context.Context, string, string, []domain.AssetShot, string, string) error
	GetSpeechClassification(context.Context, string) (*domain.SpeechClassification, error)
	SaveTranscript(context.Context, string, string, string, string, domain.Transcript, string, string) error
	GetTranscript(context.Context, string) (*domain.Transcript, error)
	GetAlignmentWords(context.Context, string) ([]domain.AlignmentWord, error)
	GetProviderFile(context.Context, string, string, string, string) (*domain.ProviderFile, error)
	SaveProviderFile(context.Context, domain.ProviderFile) error
	SaveAlignment(context.Context, string, string, string, string, string, domain.AlignmentResult, string, string) error
	EnqueueReanalysis(context.Context, string, string) error
	// HasCommittedAnalysis reports whether the asset's canonical analysis is
	// already committed. JobDerive uses it to stop a re-derive job (enqueued
	// by `cache gc --rebuildable`) from re-enqueueing the paid chain for an
	// asset whose analysis is done: the files are what the clean-up asked to
	// rebuild, never a new model run.
	HasCommittedAnalysis(context.Context, string) (bool, error)
	ListAssetShots(context.Context, string) ([]domain.AssetShot, error)
	MarkModelRunCommitted(context.Context, string, string, string) error
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
	// minFreeBytes is the free-space floor the disk preflight enforces before
	// a heavy stage runs; zero disables the preflight. It is resolved once at
	// construction from configuration, like routeDeferral, so RunUntilIdle
	// never reads config itself.
	minFreeBytes int64
	// previewLUTPath is the per-install preview LUT handed to
	// media.PreviewRenderer by the derive stage. It is per-install
	// configuration, unlike the read rate (per-job policy chosen from the
	// throttle), because a LUT is a property of the colour pipeline the
	// operator set up, not of the asset being rendered. The Pipeline must
	// carry it because the Hub's derive stage renders previews inside
	// RunUntilIdle; without it an Apple Log asset can never resolve a render
	// plan and every derive fails terminally.
	previewLUTPath string
	// onShotsCommitted is the optional post-commit hook (set by NewService
	// via SetAfterShotsCommitted): it runs after shot rows become canonical,
	// synchronously inside the job, so "the process exited" still means "no
	// job and no Provider call is still running" — the pipeline's own
	// invariant. The hook never fails the job: its implementation swallows
	// errors (the embedding layer is derived and best-effort).
	onShotsCommitted func(ctx context.Context, assetID string)
	// costEstimator is the optional post-commit cost hook (set by NewService
	// via SetCostEstimator): it records an estimate for a committed model
	// run against the serving channel's cost metadata. Same contract as
	// onShotsCommitted — synchronous inside the job, never failing it — and
	// nil when no cost tracking is wired (tests, minimal setups), in which
	// case recordCostEstimate is a no-op.
	costEstimator func(ctx context.Context, capability, provider, model, assetID string, durationMS int64)
	// leaseOwner is the stable identity this process mints lease owners from
	// when it runs the queue. It doubles as the executor_id in the
	// pipeline_executors liveness registry, so HealStaleRunningJobs can map a
	// 'running' job back to the process that holds it. Empty in tests and
	// minimal setups, where RunUntilIdle falls back to a fresh per-run owner
	// (the historical behaviour).
	leaseOwner string
}

func (p *Pipeline) failModelRun(ctx context.Context, runID, code, message, raw string, j *domain.Job, worker string) error {
	err := p.repo.FailModelRun(ctx, runID, code, message, raw, j.ID, worker)
	if errors.Is(err, domain.ErrJobLeaseLost) {
		return err
	}
	return nil
}

func NewPipeline(repo PipelineRepository, cacheDir string, asr providers.ASR, asrFallback providers.ASR, videoProvider videoproviders.VideoUnderstandingProvider, alignment providers.Alignment, shotDetector shotdetect.Detector, hardware media.HardwarePlan, sourceStager *staging.SourceStager, deferral time.Duration, minFreeBytes int64) *Pipeline {
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
	return &Pipeline{repo: repo, cacheDir: cacheDir, asr: asr, asrFallback: asrFallback, videoProvider: videoProvider, alignment: alignment, shotDetector: shotDetector, hardware: hardware, sourceStager: sourceStager, routeDeferral: deferral, minFreeBytes: minFreeBytes}
}

// SetLeaseOwner pins the identity this pipeline uses for every lease it mints
// and for its pipeline_executors liveness row (see the struct field). It must
// be set before the first RunUntilIdle and before HealOnStartup, or the
// startup sweep cannot tell this process's own jobs from a dead predecessor's.
func (p *Pipeline) SetLeaseOwner(owner string) {
	p.leaseOwner = owner
}

// SetAfterShotsCommitted attaches the post-commit hook (see onShotsCommitted).
// Tests construct pipelines without one; the hook stays nil there.
func (p *Pipeline) SetAfterShotsCommitted(hook func(ctx context.Context, assetID string)) {
	p.onShotsCommitted = hook
}

// SetCostEstimator attaches the post-commit cost hook (see costEstimator).
// Tests construct pipelines without one; the hook stays nil there, which
// keeps recordCostEstimate a no-op.
func (p *Pipeline) SetCostEstimator(hook func(ctx context.Context, capability, provider, model, assetID string, durationMS int64)) {
	p.costEstimator = hook
}

// WithPreviewLUT carries the per-install preview LUT path into every preview
// render the derive stage performs. It is a chained setter rather than a
// NewPipeline parameter because NewPipeline already takes eleven arguments
// and has thirty-one call sites, nearly all of which would have to name a
// value that is empty in the common, LUT-less install. This is the inverse of
// media.PreviewRenderer's split — there WithReadRate is the chained, per-job
// option and the LUT is the constructor argument — because the constructor
// here is shared by every stage, not just the preview path.
func (p *Pipeline) WithPreviewLUT(path string) *Pipeline {
	p.previewLUTPath = path
	return p
}

// afterShotsCommitted fires the post-commit hook when one is attached.
func (p *Pipeline) afterShotsCommitted(ctx context.Context, assetID string) {
	if p.onShotsCommitted != nil {
		p.onShotsCommitted(ctx, assetID)
	}
}

// recordCostEstimate fires the cost hook when one is attached. The hook
// itself swallows errors (see recordAnalysisCostEstimate), so a ledger
// hiccup can never fail the already-committed analysis job.
func (p *Pipeline) recordCostEstimate(ctx context.Context, capability, provider, model, assetID string, durationMS int64) {
	if p.costEstimator != nil {
		p.costEstimator(ctx, capability, provider, model, assetID, durationMS)
	}
}

func (p *Pipeline) EnqueueAsset(ctx context.Context, assetID string) error {
	loc, err := p.repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		return err
	}
	// The probe hash is derived from what the asset IS (fingerprint, size,
	// mtime), never from where it sits: a rename, remount or case change keeps
	// content and mtime intact, resolves to the same asset, and must not
	// re-enqueue the chain — or every move would pay for a fresh analysis of
	// identical footage. See ingest.StableAssetKey for the trade-off of
	// collapsing byte-identical files onto one key.
	h := ingest.StableAssetKey(loc.QuickFingerprint, loc.FileSize, loc.ProbeModifiedNS)
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
// `nexusgate serve` alongside `nexusgate pipeline run`) reclaimed the job
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
	// A stable per-process owner (see SetLeaseOwner) lets the executor
	// registry attribute in-flight jobs to this process; empty falls back to
	// a fresh per-run owner, which tests and minimal setups rely on.
	worker := p.leaseOwner
	if worker == "" {
		worker = "local-" + idgen.New()
	}
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
			// `nexusgate serve` and `nexusgate pipeline run` are both
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
				if deferErr := p.repo.DeferJob(ctx, job.ID, worker, resumeAt, domain.JobDeferProviderRouteExhausted, persistedErrorMessage(err)); deferErr != nil {
					if isLeaseLostErr(deferErr) {
						slog.Warn("job lease reclaimed before it could be deferred; discarding", "job", job.ID, "job_type", job.Type)
						continue
					}
					return executed, deferErr
				}
				slog.Warn("provider route exhausted; job deferred", "job", job.ID, "job_type", job.Type, "resume_at", resumeAt.Format(time.RFC3339))
				continue
			}
			// The cache volume fell below the configured free-space floor
			// before a heavy stage ran. The disk state is not the job's fault
			// — no attempt is spent — and space frees slowly, so the park is a
			// fixed long wait rather than retry backoff, which would burn the
			// job's whole attempt budget in seconds while the operator frees
			// space. The job resumes on its own; failing it would take the
			// whole queue down with a full disk.
			if errors.Is(err, errDiskSpaceLow) {
				resumeAt := time.Now().Add(diskSpaceRetryDelay)
				if deferErr := p.repo.DeferJob(ctx, job.ID, worker, resumeAt, domain.JobDeferDiskSpaceLow, persistedErrorMessage(err)); deferErr != nil {
					if isLeaseLostErr(deferErr) {
						slog.Warn("job lease reclaimed before it could be deferred; discarding", "job", job.ID, "job_type", job.Type)
						continue
					}
					return executed, deferErr
				}
				slog.Warn("cache volume below the configured free-space floor; job deferred", "job", job.ID, "job_type", job.Type, "resume_at", resumeAt.Format(time.RFC3339), "min_free_bytes", p.minFreeBytes)
				continue
			}
			// An actual disk-full failure — not the preflight's verdict but the
			// filesystem answering ENOSPC mid-write — is the same environmental
			// fact, so it gets the same park. The preflight exists to stop the
			// job before it starts; this catches the disk that filled since,
			// or the volume the preflight does not see (NAS mode, a Worker's
			// local cache). Retrying it hot would burn the job's whole attempt
			// budget against a volume that is still full; DeferJob hands the
			// attempt back, and the job resumes on its own once the operator
			// frees space or diskSpaceRetryDelay elapses.
			if isNoSpaceErr(err) {
				if deferErr := p.deferJobForDisk(ctx, *job, worker, err); deferErr != nil {
					if isLeaseLostErr(deferErr) {
						slog.Warn("job lease reclaimed before it could be deferred; discarding", "job", job.ID, "job_type", job.Type)
						continue
					}
					return executed, deferErr
				}
				slog.Warn("disk full; job deferred", "job", job.ID, "job_type", job.Type, "resume_at", time.Now().Add(diskSpaceRetryDelay).Format(time.RFC3339))
				continue
			}
			if isRetryableJobError(err) && job.AttemptCount < job.MaxAttempts {
				if retryErr := p.repo.RetryJob(ctx, job.ID, worker, classifyJobFailure(err), persistedErrorMessage(err), retryDelay(job.AttemptCount)); retryErr != nil {
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
			// as before, since it already sat on a best-effort path. The
			// category rides into jobs.last_error_code with the message, so the
			// issues view can aggregate this terminal failure without parsing
			// prose.
			_ = p.repo.FailJobTerminally(ctx, job.ID, worker, classifyJobFailure(err), persistedErrorMessage(err))
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

func persistedErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return common.BoundedString(err.Error())
}

func persistedRaw(raw string) string { return common.BoundedString(raw) }

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

// diskSpaceRetryDelay is how long a job parked on a full disk waits before it
// is offered again — both the preflight's verdict (cache volume below
// minimum_free_space_bytes) and an actual ENOSPC failure. The cause is
// environmental: the operator has to free space, which a retry cannot hurry —
// and hot-retrying would burn the job's whole attempt budget in seconds
// against a volume that is still full. Ten minutes is long enough for the
// queue to idle rather than spin, and short enough that a freed disk resumes
// within the hour. Like the route-exhausted park, DeferJob spends no attempt.
const diskSpaceRetryDelay = 10 * time.Minute

// isNoSpaceErr reports whether the error chain is a full-disk failure. It
// matches syscall.ENOSPC by value through errors.Is, never by message text, so
// a wrapper that rewords the text ("no space left on device") cannot change
// the verdict. os.ErrNoSpace does not exist in Go (checked against the
// standard library), so the syscall sentinel is the portable choice: it is
// defined on every GOOS the binary targets. Coverage note: the sentinel is
// carried by Go-side writes (os.CreateTemp, staging copies); modernc's SQLite
// reports SQLITE_FULL as a raw driver error without an exported sentinel, and
// an ffmpeg mid-encode exit carries no ENOSPC at all. The preflight is the
// real defense against a full disk; these two uncovered paths retry and
// eventually fail terminal rather than being misclassified as the job's fault
// — exactly the pre-wave behavior, minus the misreading.
func isNoSpaceErr(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

// deferJobForDisk parks a job on a full disk on wall-clock time with the
// disk_space_low reason instead of retrying it hot or failing it. cause is
// the failure that surfaced it (the pre-lease guard can pass nil); its text
// lands in last_error_message behind the admin token, while the reason
// constant is what the queue's visible state reads. DeferJob hands the attempt
// back, so a disk that fills during a scan costs the queue nothing.
func (p *Pipeline) deferJobForDisk(ctx context.Context, job domain.Job, worker string, cause error) error {
	msg := "disk space low"
	if cause != nil {
		msg = persistedErrorMessage(cause)
	}
	return p.repo.DeferJob(ctx, job.ID, worker, time.Now().Add(diskSpaceRetryDelay), domain.JobDeferDiskSpaceLow, msg)
}

// diskFreeCheck is the free-space probe the preflight calls, behind a
// package-level var so tests can override it: statfs needs a real path on a
// real filesystem, and what the preflight tests are about is the deferral
// wiring, not the kernel's reporting. Production is always freeBytes.
var diskFreeCheck = freeBytes

// errDiskSpaceLow is the sentinel a heavy stage returns when the preflight
// found the cache volume below the configured floor. RunUntilIdle parks the
// job on wall-clock time the same way it parks one on an exhausted provider
// route — no attempt is spent, because the disk state is not the job's fault.
// execute wraps the sentinel with the floor for the operator-facing message;
// classification is by errors.Is, never by that text.
var errDiskSpaceLow = errors.New("disk space low")

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
	//
	// A full disk is the one unmarked exception: the execute error path parks
	// it (deferJobForDisk) before this function is even consulted, and
	// classifying it as unretryable here is the guard at the decision point —
	// if a future branch reordering ever routes an ENOSPC here, it must fall
	// to the terminal path, not burn attempts. It is not marked
	// domain.Permanent because deferring is the remedy, not failing.
	if isNoSpaceErr(err) {
		return false
	}
	return !errors.Is(err, domain.ErrPermanentFailure)
}

// classifyJobFailure maps a job's failure to its stable machine-readable
// category (domain.JobFailureCategory), decided by sentinel alone — never by
// reading the message text, which is prose for operators and drifts. It runs
// at the moment the pipeline records the failure, and its answer lands in
// jobs.last_error_code, so the issues view aggregates categories from that one
// column without ever re-classifying prose.
//
// The order matters for what errors.Is/errors.As find first, and both walk the
// whole chain, so domain.Permanent's marker (domain/errors.go) cannot hide the
// underlying class: a Permanent-wrapped 429 still classifies as
// JobFailureCategoryProviderQuota, exactly as the pipeline intends — marking
// a failure permanent decides retries, never the category.
func classifyJobFailure(err error) domain.JobFailureCategory {
	if errors.Is(err, errDiskSpaceLow) || isNoSpaceErr(err) {
		return domain.JobFailureCategoryDiskSpaceLow
	}
	if errors.Is(err, providerchannels.ErrRouteExhausted) {
		return domain.JobFailureCategoryProviderRouteExhausted
	}
	// The three provider-channel sentinels that say "the deployment is
	// incomplete", as distinct from "this key is dead" (401/402/403, below,
	// which is ProviderAuth) and "the route is momentarily out of members"
	// (ErrNoRoute/ErrNoAvailable, which are deliberately unmarked because a
	// cooldown clears them). A cold-start Hub with nothing configured landed
	// every terminal failure in Unknown, so /api/v1/issues showed an unlabeled
	// bucket with no repair link — while the progress page has carried the
	// configuration→/providers mapping and its five translations all along,
	// waiting for a producer that was never written.
	if errors.Is(err, ErrProviderChannelNotConfigured) ||
		errors.Is(err, errProviderChannelSecretMissing) ||
		errors.Is(err, errProviderChannelMultiframeUnsupported) {
		return domain.JobFailureCategoryConfiguration
	}
	// Mirrors isRetryableJobError's status probe: the same *common.StatusError
	// the retry decision reads, classified by status code alone — 401/402/403
	// describe the key, 408/429 the account's momentary cap, 5xx the provider
	// itself. Any other status (a 4xx the pool classified NonRetryable, say) is
	// left to the default below: the registry does not yet give it a category,
	// and guessing one from the body would reintroduce text matching.
	var status *common.StatusError
	if errors.As(err, &status) {
		switch status.StatusCode {
		case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
			return domain.JobFailureCategoryProviderAuth
		case http.StatusRequestTimeout, http.StatusTooManyRequests:
			return domain.JobFailureCategoryProviderQuota
		}
		if status.StatusCode >= 500 && status.StatusCode <= 599 {
			return domain.JobFailureCategoryProviderUnavailable
		}
	}
	// The non-HTTP side of the same "the provider is not well" family: a
	// deadline the request's context enforced, or a transport-level network
	// error (the same net.Error probe providerpool.ClassifyFailure uses). Both
	// heal on their own, so both share the unavailable category.
	if errors.Is(err, context.DeadlineExceeded) {
		return domain.JobFailureCategoryProviderUnavailable
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return domain.JobFailureCategoryProviderUnavailable
	}
	// The decode half of this family now has a producer: media.ErrProbeRejected
	// is wrapped only around an *exec.ExitError, so it means ffprobe ran to
	// completion and judged the file undecodable — a verdict, not a missing
	// binary or a timeout (those stay retryable and fall through to Unknown).
	// Deliberately not domain.Permanent: exit 1 cannot rule out a file still
	// being copied in, and the failure happens before any read, so a retry is
	// nearly free.
	if errors.Is(err, media.ErrProbeRejected) {
		return domain.JobFailureCategoryMediaDecode
	}
	// The other file-level verdict, and the first producer of
	// JobFailureCategoryUnsupportedMedia: ResolvePlan rejects a RAW source
	// (and any plan whose renderer mode is unavailable) with a
	// *media.PreviewRenderError before anything reads the file, so this is a
	// statement about the asset, not about the environment. errors.As reads the
	// exported Code rather than the message, and each of the two codes carries
	// its own category: PreviewErrorRendererUnavailable says this build lacks
	// the renderer the asset needs (UnsupportedMedia), while
	// PreviewErrorLUTRequired says the operator has not supplied the LUT that
	// an Apple Log preview demands (PreviewLUTMissing). The new category
	// deliberately carries no repair link: the remedy is the config.json key
	// preview_lut_path, and no page can set it yet — the progress page wires
	// its configuration link to /providers, which has nothing to do with it,
	// and pointing at the wrong door is worse than pointing at none. Add a
	// link only when a page gains the ability to set preview_lut_path.
	//
	// Both branches stay below the disk-space guard above on purpose: a full
	// cache volume is an environmental fault that heals on its own, and a
	// file-level verdict must not take that park away from it. Deliberately
	// not domain.Permanent either — a RAW renderer may be added later, the LUT
	// can be configured at any moment, and the failure happens before any
	// read, so a retry is nearly free.
	var previewRenderErr *media.PreviewRenderError
	if errors.As(err, &previewRenderErr) {
		switch previewRenderErr.Code {
		case media.PreviewErrorRendererUnavailable:
			return domain.JobFailureCategoryUnsupportedMedia
		case media.PreviewErrorLUTRequired:
			return domain.JobFailureCategoryPreviewLUTMissing
		}
	}
	return domain.JobFailureCategoryUnknown
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

// diskSpaceErr reports whether a heavy stage may proceed: nil means run, an
// error wrapping errDiskSpaceLow means the cache volume is below the
// configured floor and the job must be parked. It is deliberately permissive
// on a failed probe: an unreadable statfs (or a platform without one —
// Windows) reads as "unknown", never as "blocked", because a false blocker
// would park every heavy job in the queue while the operator hunts a disk
// problem that does not exist. The guard is a safety net for NAS/local Linux
// hubs. The check is one statfs per heavy job — nothing worth caching.
// diskFloor picks the live free-space floor for one job: the throttle value
// (editable from the settings page without a restart) when the settings row
// exists, otherwise the static config floor. The configured flag is what makes
// an explicitly saved zero meaningful: 0 in the settings page means "check
// disabled", while a missing row falls back to the config floor.
func (p *Pipeline) diskFloor(throttle domain.PipelineThrottle, configured bool) int64 {
	if configured {
		return throttle.MinimumFreeSpaceBytes
	}
	return p.minFreeBytes
}

func (p *Pipeline) diskSpaceErr(floor int64) error {
	if floor <= 0 {
		return nil
	}
	free, err := diskFreeCheck(p.cacheDir)
	if err != nil {
		slog.Warn("disk free-space probe failed; proceeding without the preflight", "cache_dir", p.cacheDir, "error", err)
		return nil
	}
	if free < floor {
		return fmt.Errorf("cache volume free space %d bytes below the configured minimum of %d bytes: %w", free, floor, errDiskSpaceLow)
	}
	return nil
}

func (p *Pipeline) execute(ctx context.Context, j domain.Job, worker string, throttle domain.PipelineThrottle) error {
	loc, err := p.repo.GetPrimaryLocation(ctx, j.AssetID)
	if err != nil {
		return err
	}
	sourcePath, err := p.sourcePath(ctx, loc)
	if err != nil {
		return err
	}
	// The effective disk floor for this job: the settings page's throttle
	// value wins when the row exists (including an explicit 0 = disabled),
	// otherwise the static config floor. One existence query per job, not per
	// stage.
	configured, err := p.repo.PipelineThrottleConfigured(ctx)
	if err != nil {
		slog.Warn("pipeline throttle row presence unreadable; using the config floor", "error", err)
		configured = false
	}
	diskFloor := p.diskFloor(throttle, configured)
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
		if err := p.repo.SaveMediaMetadata(ctx, j.AssetID, m, "ffprobe-exif-v1", j.ID, worker); err != nil {
			return err
		}
		return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobDerive, hashStrings(j.InputHash, "derive-v1"), 90)
	case domain.JobDerive:
		if err := p.diskSpaceErr(diskFloor); err != nil {
			return err
		}
		base := filepath.Join(p.cacheDir, j.AssetID)
		cacheLock, lockErr := cachecoord.AcquireShared(filepath.Dir(p.cacheDir))
		if lockErr != nil {
			return fmt.Errorf("acquire cache reader lock: %w", lockErr)
		}
		defer cacheLock.Release()
		// Keep profiles in separate cache paths: a proxy created by software x264
		// must never be relabelled as an NVENC/QSV/VideoToolbox result.
		requestedThumb := filepath.Join(base, "thumbnail-"+p.hardware.Mode+".jpg")
		requestedProxy := filepath.Join(base, "proxy-"+p.hardware.Mode+".mp4")
		software := p.hardware.SoftwareFallback()
		softwareThumb := filepath.Join(base, "thumbnail-"+software.Mode+".jpg")
		softwareProxy := filepath.Join(base, "proxy-"+software.Mode+".mp4")
		thumbPlan, proxyPlan := p.hardware, p.hardware
		thumb, proxy := requestedThumb, requestedProxy
		if !media.UsableDerivedFile(requestedThumb) && media.UsableDerivedFile(softwareThumb) {
			thumb, thumbPlan = softwareThumb, software
		}
		if !media.UsableDerivedFile(requestedProxy) && media.UsableDerivedFile(softwareProxy) {
			proxy, proxyPlan = softwareProxy, software
		}
		needThumb, needProxy := !media.UsableDerivedFile(thumb), !media.UsableDerivedFile(proxy)

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
			renderer := media.NewPreviewRenderer(p.previewLUTPath).WithReadRate(readRate)
			if needThumb {
				staged := filepath.Join(base, ".derive-thumbnail.tmp.jpg")
				actual, err := renderer.RenderThumbnail(ctx, sourcePath, staged, p.hardware, previewPlan)
				if err != nil {
					return err
				}
				thumbPlan, thumb = actual, filepath.Join(base, "thumbnail-"+actual.Mode+".jpg")
				if err := media.PublishDerivedOutput(staged, thumb); err != nil {
					return err
				}
			}
			if needProxy {
				staged := filepath.Join(base, ".derive-proxy.tmp.mp4")
				actual, err := renderer.RenderProxy(ctx, sourcePath, staged, p.hardware, previewPlan)
				if err != nil {
					return err
				}
				proxyPlan, proxy = actual, filepath.Join(base, "proxy-"+actual.Mode+".mp4")
				if err := media.PublishDerivedOutput(staged, proxy); err != nil {
					return err
				}
			}
		}
		if err := saveArtifact(p.repo, ctx, j.AssetID, "thumbnail", "thumb-"+thumbPlan.Profile(), thumb, j.ID, worker); err != nil {
			return err
		}
		if err := saveArtifact(p.repo, ctx, j.AssetID, "proxy", "proxy-720-"+proxyPlan.Profile(), proxy, j.ID, worker); err != nil {
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
			// A re-derive job (enqueued by `cache gc --rebuildable`) re-creates
			// the files for an asset whose analysis already committed. The paid
			// chain must not re-run: the canonical shots are already there, and
			// re-billing them for a cache clean-up is exactly the double spend
			// the model-run dedup exists to prevent. The check is the committed
			// analysis, the same canonical evidence reanalysis switches; the
			// artifacts above were re-saved either way, so the rows never dangle.
			committed, err := p.repo.HasCommittedAnalysis(ctx, j.AssetID)
			if err != nil {
				return err
			}
			if committed {
				return nil
			}
			return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobSpeechGate, hashStrings(j.InputHash, "speech-v1"), 80)
		}
		committed, err := p.repo.HasCommittedAnalysis(ctx, j.AssetID)
		if err != nil {
			return err
		}
		if committed {
			return nil
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
		cacheLock, lockErr := cachecoord.AcquireShared(filepath.Dir(p.cacheDir))
		if lockErr != nil {
			return fmt.Errorf("acquire cache reader lock: %w", lockErr)
		}
		c, err := media.SpeechGate(ctx, a.LocalPath, metadata.DurationMS)
		_ = cacheLock.Release()
		if err != nil {
			return err
		}
		if err := p.repo.SaveSpeechClassification(ctx, j.AssetID, c, j.ID, worker); err != nil {
			return err
		}
		if c.SpeechProbability >= 0.5 && p.asr != nil {
			return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobTranscribe, hashStrings(j.InputHash, p.asr.Name(), p.asr.Model()), 60)
		}
		return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobAnalyze, hashStrings(j.InputHash, "analyze-v1"), 30)
	case domain.JobTranscribe:
		if err := p.diskSpaceErr(diskFloor); err != nil {
			return err
		}
		a, err := p.repo.GetArtifact(ctx, j.AssetID, "audio")
		if err != nil {
			return err
		}
		if a == nil {
			return domain.Permanent(fmt.Errorf("audio artifact missing"))
		}
		cacheLock, lockErr := cachecoord.AcquireShared(filepath.Dir(p.cacheDir))
		if lockErr != nil {
			return fmt.Errorf("acquire cache reader lock: %w", lockErr)
		}
		t, err := p.asr.Transcribe(ctx, providers.TranscribeRequest{AudioPath: a.LocalPath, Language: "zh"})
		providerUsed := p.asr
		if err != nil && p.asrFallback != nil {
			t, err = p.asrFallback.Transcribe(ctx, providers.TranscribeRequest{AudioPath: a.LocalPath, Language: "zh"})
			providerUsed = p.asrFallback
		}
		if err != nil {
			_ = cacheLock.Release()
			return err
		}
		if err := cacheLock.Release(); err != nil {
			return err
		}
		if err := p.repo.SaveTranscript(ctx, j.AssetID, providerUsed.Name(), providerUsed.Model(), j.InputHash, t, j.ID, worker); err != nil {
			return err
		}
		// The transcript is canonical; record the ASR estimate against the
		// serving channel's cost metadata. The metadata fetch is skipped when
		// no cost hook is wired, so unwired pipelines pay nothing for a
		// guide they do not keep. Duration stands in for audio minutes (see
		// RecordAnalysisCostEstimate). A metadata read failure must NOT fail
		// the job: the transcript is already committed, and a retry would
		// re-run the paid transcription to feed an estimate — the ledger is
		// a guide, never a reason to spend twice.
		if p.costEstimator != nil {
			if metadata, metadataErr := p.repo.GetMediaMetadata(ctx, j.AssetID); metadataErr != nil {
				slog.Warn("cost estimate metadata unreadable; skipping estimate", "asset_id", j.AssetID, "error", metadataErr)
			} else if metadata != nil {
				p.recordCostEstimate(ctx, "asr", providerUsed.Name(), providerUsed.Model(), j.AssetID, metadata.DurationMS)
			}
		}
		if p.alignment != nil {
			return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobAlign, hashStrings(j.InputHash, p.alignment.Name(), p.alignment.Model()), 50)
		}
		return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobAnalyze, hashStrings(j.InputHash, "analyze-v1"), 30)
	case domain.JobAlign:
		if err := p.diskSpaceErr(diskFloor); err != nil {
			return err
		}
		// Alignment is local work and has no provider cost estimate.
		a, err := p.repo.GetArtifact(ctx, j.AssetID, "audio")
		if err != nil {
			return err
		}
		if a == nil {
			return domain.Permanent(fmt.Errorf("audio artifact missing"))
		}
		cacheLock, lockErr := cachecoord.AcquireShared(filepath.Dir(p.cacheDir))
		if lockErr != nil {
			return fmt.Errorf("acquire cache reader lock: %w", lockErr)
		}
		t, err := p.repo.GetTranscript(ctx, j.AssetID)
		if err != nil {
			_ = cacheLock.Release()
			return err
		}
		if t == nil || t.Text == "" {
			_ = cacheLock.Release()
			return domain.Permanent(fmt.Errorf("transcript missing"))
		}
		result, err := p.alignment.Align(ctx, providers.AlignRequest{AudioPath: a.LocalPath, Transcript: *t, Language: t.Language})
		_ = cacheLock.Release()
		if err != nil {
			return err
		}
		req, _ := json.Marshal(map[string]any{"audio_path": a.LocalPath, "text": t.Text, "language": t.Language})
		if err := p.repo.SaveAlignment(ctx, j.AssetID, p.alignment.Name(), p.alignment.Model(), j.InputHash, string(req), result, j.ID, worker); err != nil {
			return err
		}
		return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobAnalyze, hashStrings(j.InputHash, "analyze-v1"), 30)
	case domain.JobAnalyze:
		if err := p.diskSpaceErr(diskFloor); err != nil {
			return err
		}
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
		var analyzeErr error
		if route != nil {
			if p.shotDetector != nil {
				analyzeErr = p.analyzeWithDetector(ctx, &j, m, route, worker)
			} else if route.video != nil {
				analyzeErr = p.analyzeTwoPass(ctx, &j, m, route, worker)
			} else {
				// A frame-only endpoint cannot produce its own boundaries and no
				// fallback can either: the deployment is missing one of the two
				// things the multiframe path needs. Deterministic config, same
				// answer on retry — permanent, with the remedy spelled out.
				analyzeErr = domain.Permanent(fmt.Errorf("multiframe video provider %q requires a shot detector (providers.shot_detection) or a video-capable fallback provider (providers.vision_fallback)", route.analyzer.Name()))
			}
		} else {
			analyzeErr = p.analyzeAssetVideo(ctx, &j, m, p.videoProvider, sourcePath, worker)
		}
		if analyzeErr != nil {
			return analyzeErr
		}
		return p.enqueueAnalysisIndex(ctx, j)
	case domain.JobIndex:
		return p.repo.RebuildSearch(ctx, j.AssetID)
	default:
		return domain.Permanent(fmt.Errorf("unsupported job type %s", j.Type))
	}
}

func (p *Pipeline) enqueueAnalysisIndex(ctx context.Context, j domain.Job) error {
	return p.repo.EnqueueJob(ctx, j.AssetID, domain.JobIndex, hashStrings(j.InputHash, "index-v1"), 20)
}

func (p *Pipeline) sourcePath(ctx context.Context, location domain.AssetLocation) (string, error) {
	if p.sourceStager == nil {
		return location.AbsolutePath, nil
	}
	version := hashStrings(location.AbsolutePath, fmt.Sprint(location.ModifiedNS))
	return p.sourceStager.Stage(ctx, location.AbsolutePath, location.AssetID, version)
}

// sourcePath stages the source once with a versioned cache key (or returns
// the NAS path verbatim when staging is off), and every stage — including the
// probe — reads that same staged file, so copy mode copies once per scan
// version instead of re-reading the network share for each media stage.

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
