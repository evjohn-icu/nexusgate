package app

import (
	"testing"

	"github.com/ev/timingdex/internal/domain"
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
