package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	PreviewErrorLUTRequired         = "lut_required"
	PreviewErrorRendererUnavailable = "renderer_unavailable"
)

// Both preview filters cap one dimension and round the other to an even number
// with `-2`, and both must stay that way: libx264 rejects an odd dimension
// under yuv420p, so an odd result is not a cosmetic difference but a derive
// that fails outright.
//
// Do not reintroduce `force_original_aspect_ratio=decrease` here. It recomputes
// the dimension `-2` was asked to round, which silently reproduces the odd
// value — that is what made every 16:9 proxy fail to encode, 1920x1080 and
// 3840x2160 included, while the 4:3 test fixture happened to survive it.
//
// `min` keeps a source smaller than the cap from being upscaled into a preview
// larger than its original. Its comma is escaped because a bare comma separates
// filters in an FFmpeg filter chain.
const (
	// proxyScaleFilter bounds the proxy by height: 720 here is the same 720 a
	// Worker advertises as MaxProxyHeight, not a width.
	proxyScaleFilter = `scale=-2:min(720\,ih)`
	// thumbnailScaleFilter bounds the thumbnail by width instead, because the
	// library grid lays thumbnails out by width.
	thumbnailScaleFilter = `scale=min(720\,iw):-2`
)

// PreviewRenderError reports a source that cannot safely be rendered by the
// current preview renderer. Recoverable errors are expected for sources that
// need an operator-provided LUT or a future RAW renderer.
type PreviewRenderError struct {
	Code        string
	SourceColor SourceColorClass
	Message     string
	Recoverable bool
}

func (e *PreviewRenderError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("preview render %s for %s", e.Code, e.SourceColor)
}

// PreviewRenderer turns a classified source into a safe SDR preview. LUTPath
// is deliberately supplied by the caller; it is never inferred from source
// metadata and is never written to source media.
type PreviewRenderer struct {
	LUTPath string
	// ReadRate caps how fast FFmpeg reads the source, as a multiple of realtime
	// playback. Zero means unlimited. It exists so a full-library derive can be
	// stopped from saturating a mechanical disk or a NAS link for hours; see
	// domain.PipelineThrottle for the policy that chooses the value.
	ReadRate float64
}

func NewPreviewRenderer(lutPath string) *PreviewRenderer {
	return &PreviewRenderer{LUTPath: strings.TrimSpace(lutPath)}
}

// WithReadRate returns a copy carrying the rate cap. It is a chained option
// rather than a constructor argument because the rate is per-job policy while
// the LUT is per-install configuration, and callers that do not throttle should
// not have to mention it.
func (r *PreviewRenderer) WithReadRate(rate float64) *PreviewRenderer {
	copied := *r
	if rate > 0 {
		copied.ReadRate = rate
	}
	return &copied
}

// ResolvePlan validates the external rendering prerequisite and marks an
// Apple Log plan available only after an explicit, readable LUT is supplied.
func (r *PreviewRenderer) ResolvePlan(plan PreviewRenderPlan) (PreviewRenderPlan, error) {
	if plan.SourceColor == SourceColorRAW || plan.Mode == PreviewRenderUnavailable {
		return plan, &PreviewRenderError{
			Code:        PreviewErrorRendererUnavailable,
			SourceColor: plan.SourceColor,
			Message:     "RAW preview renderer unavailable; no proxy was generated",
			Recoverable: true,
		}
	}
	if plan.SourceColor == SourceColorLogApple || plan.Mode == PreviewRenderLUT || plan.RequiresLUT {
		label := "Log"
		if plan.SourceColor == SourceColorLogApple {
			label = "Apple Log"
		}
		if r == nil || r.LUTPath == "" {
			return plan, &PreviewRenderError{
				Code:        PreviewErrorLUTRequired,
				SourceColor: plan.SourceColor,
				Message:     label + " preview needs LUT; no proxy was generated",
				Recoverable: true,
			}
		}
		info, err := os.Stat(r.LUTPath)
		if err != nil || !info.Mode().IsRegular() {
			return plan, &PreviewRenderError{
				Code:        PreviewErrorLUTRequired,
				SourceColor: plan.SourceColor,
				Message:     fmt.Sprintf("%s preview needs readable LUT %q; no proxy was generated", label, r.LUTPath),
				Recoverable: true,
			}
		}
		plan.Mode = PreviewRenderLUT
		plan.Filter = "lut3d"
		plan.RequiresLUT = true
		plan.Available = true
		return plan, nil
	}
	if plan.SourceColor == SourceColorHDRHLG || plan.SourceColor == SourceColorHDRPQ {
		plan.Mode = PreviewRenderToneMap
		plan.Filter = "tonemap"
		plan.Available = true
	}
	if plan.Mode == "" {
		plan.Mode = PreviewRenderDirect
	}
	if plan.Mode == PreviewRenderDirect {
		plan.Available = true
	}
	return plan, nil
}

// VideoFilter returns the complete filter chain for a thumbnail or proxy.
// HDR sources are converted through linear light and tone mapped to Rec.709;
// the conversion is intentionally explicit instead of relying on FFmpeg's
// implicit color negotiation.
func (r *PreviewRenderer) VideoFilter(plan PreviewRenderPlan, base string) (string, error) {
	resolved, err := r.ResolvePlan(plan)
	if err != nil {
		return "", err
	}
	var colorFilter string
	switch resolved.Mode {
	case PreviewRenderToneMap:
		colorFilter = "zscale=t=linear:npl=100,format=gbrpf32le,tonemap=tonemap=hable:desat=0,zscale=p=bt709:t=bt709:m=bt709,format=yuv420p"
	case PreviewRenderLUT:
		colorFilter = "lut3d=file='" + escapeFilterValue(r.LUTPath) + "'"
	}
	if colorFilter == "" {
		return base, nil
	}
	if base == "" {
		return colorFilter, nil
	}
	return colorFilter + "," + base, nil
}

func escapeFilterValue(value string) string {
	return strings.NewReplacer("\\", "\\\\", "'", "\\'", ":", "\\:", ",", "\\,").Replace(filepath.Clean(value))
}

func (r *PreviewRenderer) RenderThumbnail(ctx context.Context, src, dst string, hardware HardwarePlan, sourcePlan PreviewRenderPlan) error {
	if err := rejectSourceOverwrite(src, dst); err != nil {
		return err
	}
	resolved, err := r.ResolvePlan(sourcePlan)
	if err != nil {
		return err
	}
	filter, err := r.VideoFilter(resolved, thumbnailScaleFilter)
	if err != nil {
		return err
	}
	return atomicFFmpegOutput(dst, func(out string) error {
		args := append([]string{"-hide_banner", "-loglevel", "error", "-y", "-ss", "00:00:01"}, hardware.DecoderArgs...)
		args = append(args, "-i", src, "-frames:v", "1", "-vf", filter, out)
		return runWithFallback(ctx, "thumbnail", args, hardware, func() []string {
			return []string{"-hide_banner", "-loglevel", "error", "-y", "-ss", "00:00:01", "-i", src, "-frames:v", "1", "-vf", filter, out}
		})
	})
}

func (r *PreviewRenderer) RenderProxy(ctx context.Context, src, dst string, hardware HardwarePlan, sourcePlan PreviewRenderPlan) error {
	if err := rejectSourceOverwrite(src, dst); err != nil {
		return err
	}
	resolved, err := r.ResolvePlan(sourcePlan)
	if err != nil {
		return err
	}
	baseFilter := proxyScaleFilter
	filter, err := r.VideoFilter(resolved, baseFilter+hardware.ProxyFilter)
	if err != nil {
		return err
	}
	softwareFilter, err := r.VideoFilter(resolved, baseFilter)
	if err != nil {
		return err
	}
	// The proxy is the one derive step that reads the whole source, so it is
	// where a rate cap actually relieves the disk. The thumbnail decodes a
	// single frame and is deliberately left unthrottled.
	rate := readRateArgs(r.ReadRate)
	return atomicFFmpegOutput(dst, func(out string) error {
		args := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, hardware.DecoderArgs...)
		args = append(args, rate...)
		args = append(args, "-i", src, "-vf", filter)
		args = append(args, hardware.EncoderArgs...)
		args = append(args, "-c:a", "aac", "-b:a", "96k", "-movflags", "+faststart", out)
		return runWithFallback(ctx, "proxy", args, hardware, func() []string {
			fallback := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, rate...)
			return append(fallback, "-i", src, "-vf", softwareFilter, "-c:v", "libx264", "-preset", "veryfast", "-crf", "28", "-c:a", "aac", "-b:a", "96k", "-movflags", "+faststart", out)
		})
	})
}

func rejectSourceOverwrite(src, dst string) error {
	sourcePath, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("resolve original media path: %w", err)
	}
	destinationPath, err := filepath.Abs(dst)
	if err != nil {
		return fmt.Errorf("resolve derived media path: %w", err)
	}
	if sourcePath == destinationPath {
		return fmt.Errorf("refusing to overwrite original media %q", src)
	}

	sourceInfo, sourceErr := os.Stat(src)
	destinationInfo, destinationErr := os.Stat(dst)
	if sourceErr == nil && destinationErr == nil && os.SameFile(sourceInfo, destinationInfo) {
		return fmt.Errorf("refusing to overwrite original media %q", src)
	}
	return nil
}
