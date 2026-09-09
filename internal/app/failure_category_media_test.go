package app

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
)

// classifyJobFailure's media-decode branch is only worth having if it
// discriminates. Its answer is written into jobs.last_error_code, the single
// column /api/v1/issues aggregates from, so a branch that answered
// media_decode too eagerly would relabel provider, disk and plain failures as
// file corruption. Each row pins one property of that mapping: the positive
// through a wrapping chain, that a bare error and a non-sentinel ffprobe
// error stay Unknown, that the pre-existing disk branch keeps its own
// sentinel, and that branch order keeps an environmental disk failure from
// masquerading as a decode verdict.
func TestClassifyJobFailureMediaDecodeDiscriminates(t *testing.T) {
	cases := []struct {
		name string
		// property names the invariant this row exists to protect, so a
		// failure says which contract broke and not merely that two strings
		// differ.
		property string
		err      error
		want     domain.JobFailureCategory
	}{
		{
			name:     "probe-rejected sentinel wrapped",
			property: "media.ErrProbeRejected must classify as media_decode through the error chain (errors.Is, not equality)",
			err:      fmt.Errorf("probe /media/drone.mov: %w", media.ErrProbeRejected),
			want:     domain.JobFailureCategoryMediaDecode,
		},
		{
			name:     "plain error",
			property: "an error carrying no sentinel must stay unknown, not fall into media_decode",
			err:      errors.New("boom"),
			want:     domain.JobFailureCategoryUnknown,
		},
		{
			name:     "ffprobe binary missing is not a decode verdict",
			property: "only media.ErrProbeRejected maps to media_decode; *exec.Error (ffprobe never ran) must not become a decode verdict",
			err:      fmt.Errorf("probe /media/drone.mov: %w", &exec.Error{Name: "ffprobe", Err: exec.ErrNotFound}),
			want:     domain.JobFailureCategoryUnknown,
		},
		{
			name:     "disk-space sentinel still wins its own sentinel",
			property: "the pre-existing errDiskSpaceLow branch must keep classifying as disk_space_low",
			err:      fmt.Errorf("cache volume full: %w", errDiskSpaceLow),
			want:     domain.JobFailureCategoryDiskSpaceLow,
		},
		{
			name:     "disk-space sentinel wrapping probe rejection",
			property: "branch order: a full disk is environmental and must not be reported as file corruption",
			err:      fmt.Errorf("cache volume full while probing: %w: %w", errDiskSpaceLow, media.ErrProbeRejected),
			want:     domain.JobFailureCategoryDiskSpaceLow,
		},
		{
			name:     "RAW renderer-unavailable preview error wrapped once",
			property: "a *media.PreviewRenderError with Code media.PreviewErrorRendererUnavailable must classify as unsupported_media through the error chain (errors.As, not equality)",
			err: fmt.Errorf("render /media/drone.braw: %w", &media.PreviewRenderError{
				Code:        media.PreviewErrorRendererUnavailable,
				SourceColor: media.SourceColorRAW,
			}),
			want: domain.JobFailureCategoryUnsupportedMedia,
		},
		{
			name:     "LUT-required preview error keeps unknown",
			property: "only Code media.PreviewErrorRendererUnavailable maps to unsupported_media; media.PreviewErrorLUTRequired is a configuration gap (the LUT is a config.json key) and must stay unknown so no one relabels the whole PreviewRenderError type and points the repair link at the wrong page; same SourceColor as the row above so Code is the only variable",
			err: fmt.Errorf("render /media/drone.braw: %w", &media.PreviewRenderError{
				Code:        media.PreviewErrorLUTRequired,
				SourceColor: media.SourceColorRAW,
			}),
			want: domain.JobFailureCategoryUnknown,
		},
		{
			name:     "disk-space sentinel beats renderer-unavailable preview error",
			property: "branch order: with both sentinels in one chain the environmental disk-space fault keeps its own category and must not be reported as a file verdict",
			err: fmt.Errorf("cache volume full while rendering: %w: %w", errDiskSpaceLow, &media.PreviewRenderError{
				Code:        media.PreviewErrorRendererUnavailable,
				SourceColor: media.SourceColorRAW,
			}),
			want: domain.JobFailureCategoryDiskSpaceLow,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyJobFailure(tc.err); got != tc.want {
				t.Fatalf("classifyJobFailure(%v) = %q, want %q — broken property: %s", tc.err, got, tc.want, tc.property)
			}
		})
	}
}
