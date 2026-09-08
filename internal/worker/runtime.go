package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/buildinfo"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/remote"
	"github.com/evjohn-icu/nexusgate/internal/staging"
)

type RuntimeClient interface {
	Heartbeat(context.Context, string, string, remote.WorkerCapabilities) error
	Lease(context.Context, string) (*remote.WorkerJob, error)
	UploadArtifact(context.Context, string, string, ArtifactUpload) error
	Complete(context.Context, string, string, domain.JobState, string) error
}

type Deriver interface {
	Derive(context.Context, remote.WorkerJob, string, string) ([]ArtifactUpload, error)
}

type Runtime struct {
	client  RuntimeClient
	config  Config
	deriver Deriver
	// capabilities is what heartbeats actually send. It starts as whatever
	// was declared at `worker enroll` time but is meant to be replaced with
	// MergeDetectedCapabilities' output at `worker run` startup, so a
	// container that gained /dev/dri after a template edit stops being
	// mislabelled the moment it restarts — which a template edit forces
	// anyway. It is deliberately not config.Registration.Capabilities: that
	// field is what got written to worker.json at enrollment and must stay
	// untouched as the on-disk record of what enroll saw.
	capabilities remote.WorkerCapabilities
}

type progressReporter interface {
	Progress(context.Context, string, string, string, float64, string, string) error
}

// NewRuntime wires a Runtime for the lease loop. capabilities is what
// heartbeats report; pass the output of MergeDetectedCapabilities so a fresh
// hardware detection, not the one frozen into worker.json at enroll time,
// reaches the Hub.
func NewRuntime(client RuntimeClient, config Config, deriver Deriver, capabilities remote.WorkerCapabilities) *Runtime {
	return &Runtime{client: client, config: config, deriver: deriver, capabilities: capabilities}
}

// MergeDetectedCapabilities combines a fresh hardware detection with the
// capabilities recorded at enroll time. Everything DetectHardware can
// actually observe — the FFmpeg-derived Proxy/Thumbnail/AudioExtract
// booleans and the selected accelerator — is replaced with what this
// process just measured; everything an operator declared by hand, that no
// probe can infer, is carried over unchanged: which library roots are
// mounted, which Provider operations this node is trusted for, its speed
// class, and its parallelism limits. Without that split, a container that
// gains /dev/dri after a template edit would either keep reporting stale
// hardware forever, or — if the whole struct were simply replaced — forget
// its declared roots and operations the next time it restarted.
func MergeDetectedCapabilities(declared remote.WorkerCapabilities, report media.HardwareReport) remote.WorkerCapabilities {
	merged := declared
	merged.Proxy = report.FFmpegFound
	merged.Thumbnail = report.FFmpegFound
	merged.AudioExtract = report.FFmpegFound
	merged.Hardware = report.SelectedBackends()
	return merged
}

// RunOnce sends a liveness signal, leases at most one job, and processes that
// job. It is intentionally small so the long-running loop and integration
// tests share exactly the same lease/derive/upload/complete behavior.
func (r *Runtime) RunOnce(ctx context.Context) (bool, error) {
	if r == nil || r.client == nil || r.deriver == nil {
		return false, fmt.Errorf("worker runtime is not configured")
	}
	if err := r.client.Heartbeat(ctx, r.config.Token, buildinfo.VersionString(), r.capabilities); err != nil {
		return false, err
	}
	job, err := r.client.Lease(ctx, r.config.Token)
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}
	if err := r.process(ctx, *job); err != nil {
		completeErr := r.client.Complete(ctx, r.config.Token, job.JobID, domain.JobFailed, err.Error())
		if completeErr != nil {
			return true, fmt.Errorf("process job: %w; report failure: %v", err, completeErr)
		}
		return true, err
	}
	if err := r.client.Complete(ctx, r.config.Token, job.JobID, domain.JobSucceeded, ""); err != nil {
		return true, err
	}
	return true, nil
}

type RunOptions struct {
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
}

func (o RunOptions) withDefaults() RunOptions {
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 30 * time.Second
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 2 * time.Second
	}
	return o
}

// Run keeps the Worker alive while the Hub is reachable. It heartbeats on a
// separate cadence and keeps leasing after the first heartbeat. A transient
// Hub error is returned to the service manager, which can restart the Worker.
func (r *Runtime) Run(ctx context.Context, options RunOptions) error {
	if r == nil || r.client == nil || r.deriver == nil {
		return fmt.Errorf("worker runtime is not configured")
	}
	options = options.withDefaults()
	lastHeartbeat := time.Time{}
	for {
		if lastHeartbeat.IsZero() || time.Since(lastHeartbeat) >= options.HeartbeatInterval {
			// Capabilities are re-detected once, at Run's caller (worker run
			// startup in cmd/nexusgate/main.go), not on this cadence. A device
			// becoming available requires a container recreate, which restarts
			// the process anyway, so nothing here would ever observe a change;
			// re-probing on every 30s heartbeat would only tax the GPU with a
			// throwaway encode for no new information.
			if err := r.client.Heartbeat(ctx, r.config.Token, buildinfo.VersionString(), r.capabilities); err != nil {
				return err
			}
			lastHeartbeat = time.Now()
		}
		job, err := r.client.Lease(ctx, r.config.Token)
		if err != nil {
			return err
		}
		if job != nil {
			if err := r.processAndComplete(ctx, *job); err != nil {
				return err
			}
			continue
		}
		timer := time.NewTimer(options.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (r *Runtime) processAndComplete(ctx context.Context, job remote.WorkerJob) error {
	if err := r.process(ctx, job); err != nil {
		if completeErr := r.client.Complete(ctx, r.config.Token, job.JobID, domain.JobFailed, err.Error()); completeErr != nil {
			return fmt.Errorf("process job: %w; report failure: %v", err, completeErr)
		}
		return nil
	}
	return r.client.Complete(ctx, r.config.Token, job.JobID, domain.JobSucceeded, "")
}

func (r *Runtime) process(ctx context.Context, job remote.WorkerJob) error {
	if job.JobType != domain.JobDerive {
		return fmt.Errorf("worker cannot execute job type %q", job.JobType)
	}
	r.reportProgress(ctx, job.JobID, "staging", 5, "started", "准备原始素材")
	source, err := r.config.ResolveSourcePath(job.RootID, job.RelativePath)
	if err != nil {
		return err
	}
	cacheDir := r.config.CachePath()
	stager, err := staging.NewCapped(string(staging.ModeCopy), cacheDir, r.config.SourceCacheCap())
	if err != nil {
		return err
	}
	version := fmt.Sprintf("%d", job.ModifiedNS)
	staged, err := stager.Stage(ctx, source, job.AssetID, version)
	if err != nil {
		return err
	}
	r.reportProgress(ctx, job.JobID, "derive", 25, "progress", "正在生成预览素材")
	outputDir := filepath.Join(cacheDir, "artifacts", safeID(job.AssetID))
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return err
	}
	artifacts, err := r.deriver.Derive(ctx, job, staged, outputDir)
	if err != nil {
		return err
	}
	for index, artifact := range artifacts {
		if err := validateArtifact(artifact); err != nil {
			return err
		}
		if err := r.client.UploadArtifact(ctx, r.config.Token, job.JobID, artifact); err != nil {
			return err
		}
		progress := 30 + float64(index+1)*60/float64(len(artifacts))
		r.reportProgress(ctx, job.JobID, "upload", progress, "progress", "派生产物已上传")
	}
	return nil
}

func (r *Runtime) reportProgress(ctx context.Context, jobID, stage string, progress float64, event, message string) {
	reporter, ok := r.client.(progressReporter)
	if !ok {
		return
	}
	// Progress is informational. The final upload/complete mutations retain
	// the strict failure semantics; a transient status update cannot discard a
	// correctly produced derived artifact.
	_ = reporter.Progress(ctx, r.config.Token, jobID, stage, progress, event, message)
}

func safeID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown-asset"
	}
	return value
}

func validateArtifact(artifact ArtifactUpload) error {
	if strings.TrimSpace(artifact.Type) == "" || strings.TrimSpace(artifact.ProfileHash) == "" || strings.TrimSpace(artifact.Path) == "" {
		return fmt.Errorf("worker derived artifact is incomplete")
	}
	info, err := os.Lstat(artifact.Path)
	if err != nil {
		return fmt.Errorf("lstat derived artifact: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("derived artifact is a symlink: %s", artifact.Path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("derived artifact is not a regular file: %s", artifact.Path)
	}
	if info.Size() <= 0 {
		return fmt.Errorf("derived artifact is empty or zero-byte: %s", artifact.Path)
	}
	return nil
}

type FFmpegDeriver struct {
	Plan media.HardwarePlan
}

func NewFFmpegDeriver(plan media.HardwarePlan) *FFmpegDeriver {
	return &FFmpegDeriver{Plan: plan}
}

func (d *FFmpegDeriver) Derive(ctx context.Context, job remote.WorkerJob, sourcePath, outputDir string) ([]ArtifactUpload, error) {
	if d == nil {
		return nil, fmt.Errorf("FFmpeg deriver is not configured")
	}
	thumbnailStage := filepath.Join(outputDir, ".derive-thumbnail.tmp.jpg")
	proxyStage := filepath.Join(outputDir, ".derive-proxy.tmp.mp4")

	// One probe covers the preview plan for both renders below and the
	// audio-stream check that used to run its own separate probe; a Worker
	// has no persisted MediaMetadata to reuse the way the Hub pipeline does,
	// so this is the one ffprobe call a derive here needs instead of three.
	probe, probeErr := media.Probe(ctx, sourcePath)
	previewPlan := media.PreviewPlanForProbeResult(probe, probeErr, sourcePath)

	renderer := media.NewPreviewRenderer("").WithReadRate(job.ReadRate)
	thumbnailPlan, err := renderer.RenderThumbnail(ctx, sourcePath, thumbnailStage, d.Plan, previewPlan)
	if err != nil {
		return nil, err
	}
	thumbnail := filepath.Join(outputDir, "thumbnail-"+thumbnailPlan.Mode+".jpg")
	if err := media.PublishDerivedOutput(thumbnailStage, thumbnail); err != nil {
		return nil, err
	}
	proxyPlan, err := renderer.RenderProxy(ctx, sourcePath, proxyStage, d.Plan, previewPlan)
	if err != nil {
		return nil, err
	}
	proxy := filepath.Join(outputDir, "proxy-"+proxyPlan.Mode+".mp4")
	if err := media.PublishDerivedOutput(proxyStage, proxy); err != nil {
		return nil, err
	}
	artifacts := []ArtifactUpload{
		{Type: "thumbnail", ProfileHash: "thumb-" + thumbnailPlan.Profile(), Path: thumbnail},
		{Type: "proxy", ProfileHash: "proxy-720-" + proxyPlan.Profile(), Path: proxy},
	}
	if probeErr != nil {
		return nil, probeErr
	}
	for _, stream := range probe.Streams {
		if stream.CodecType != "audio" {
			continue
		}
		audio := filepath.Join(outputDir, "audio.m4a")
		if err := media.ExtractAudio(ctx, sourcePath, audio, job.ReadRate); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, ArtifactUpload{Type: "audio", ProfileHash: "audio-16k-v1", Path: audio})
		break
	}
	return artifacts, nil
}
