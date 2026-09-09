package app

import (
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

func TestValidateAnalysisShotsRejectsOutOfBoundsAndBlankDescriptions(t *testing.T) {
	for name, shots := range map[string][]domain.AssetShot{
		"past asset duration": {{StartMS: 900, EndMS: 1200, Description: "late shot"}},
		"blank description":   {{StartMS: 100, EndMS: 200, Description: "  "}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateAnalysisShots(shots, 1000); err == nil {
				t.Fatal("expected invalid untrusted shot to be rejected before persistence")
			}
		})
	}
}

// TestValidateAnalysisShotsClampsRoundingNoise is the regression for the shot
// that failed permanently 3ms past a 94997ms asset: a straddling shot within
// shotBoundaryToleranceMS of 0 or the asset duration is accepted AND the
// shot slice itself is corrected in place, because it is that corrected
// value — not merely a nil error — that StageModelRun/CommitAnalysisWithShots
// persist. A test that only checked err == nil would stay green even if the
// clamp assignment were deleted and 95000 reached asset_shots verbatim.
func TestValidateAnalysisShotsClampsRoundingNoise(t *testing.T) {
	t.Run("end just past duration is clamped to duration", func(t *testing.T) {
		shots := []domain.AssetShot{{StartMS: 0, EndMS: 95000, Description: "x"}}
		if err := validateAnalysisShots(shots, 94997); err != nil {
			t.Fatalf("expected a 3ms overshoot to be treated as rounding, got %v", err)
		}
		if shots[0].EndMS != 94997 {
			t.Fatalf("expected the committed shot to be clamped to the asset duration, got EndMS=%d", shots[0].EndMS)
		}
	})

	t.Run("start just before zero is clamped to zero", func(t *testing.T) {
		shots := []domain.AssetShot{{StartMS: -3, EndMS: 1000, Description: "x"}}
		if err := validateAnalysisShots(shots, 60000); err != nil {
			t.Fatalf("expected a 3ms undershoot to be treated as rounding, got %v", err)
		}
		if shots[0].StartMS != 0 {
			t.Fatalf("expected the committed shot to be clamped to 0, got StartMS=%d", shots[0].StartMS)
		}
	})

	t.Run("exactly the tolerance is still accepted", func(t *testing.T) {
		shots := []domain.AssetShot{{StartMS: 0, EndMS: 94997 + shotBoundaryToleranceMS, Description: "x"}}
		if err := validateAnalysisShots(shots, 94997); err != nil {
			t.Fatalf("expected exactly shotBoundaryToleranceMS over to be accepted, got %v", err)
		}
		if shots[0].EndMS != 94997 {
			t.Fatalf("expected the committed shot to be clamped to the asset duration, got EndMS=%d", shots[0].EndMS)
		}
	})

	t.Run("one millisecond past the tolerance is rejected", func(t *testing.T) {
		shots := []domain.AssetShot{{StartMS: 0, EndMS: 94997 + shotBoundaryToleranceMS + 1, Description: "x"}}
		err := validateAnalysisShots(shots, 94997)
		if err == nil {
			t.Fatal("expected a miss one millisecond beyond the tolerance to still be rejected")
		}
		if isRetryableJobError(err) {
			t.Fatalf("rejection beyond the tolerance was classified retryable (%v)", err)
		}
	})

	t.Run("a shot wholly past the asset end is not clamped into a false positive", func(t *testing.T) {
		shots := []domain.AssetShot{{StartMS: 94998, EndMS: 95000, Description: "x"}}
		err := validateAnalysisShots(shots, 94997)
		if err == nil {
			t.Fatal("expected a shot that starts after the asset already ended to be rejected")
		}
		if !strings.Contains(err.Error(), "ends after asset duration") {
			t.Fatalf("expected the honest diagnosis for a shot wholly past the end, got %v", err)
		}
	})

	t.Run("start/end merely out of order stays strict, not a rounding case", func(t *testing.T) {
		shots := []domain.AssetShot{{StartMS: 1000, EndMS: 999, Description: "x"}}
		if err := validateAnalysisShots(shots, 60000); err == nil {
			t.Fatal("expected an inverted shot range to be rejected regardless of how small the inversion is")
		}
	})
}
