package app

import (
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

func TestCostGuidesAreAdvisoryPipelineSettings(t *testing.T) {
	throttle := domain.PipelineThrottle{DailyCostGuide: 50, MonthlyCostGuide: 300}
	if err := throttle.Validate(); err != nil {
		t.Fatal(err)
	}
	// The pipeline no longer has a cost gate: these values are intentionally
	// configuration metadata, not admission controls for paid stages.
	if throttle.DailyCostGuide != 50 || throttle.MonthlyCostGuide != 300 {
		t.Fatalf("guides changed: %+v", throttle)
	}
}

func TestCostGuidesRejectNegativeValues(t *testing.T) {
	for name, throttle := range map[string]domain.PipelineThrottle{
		"daily":   {DailyCostGuide: -1},
		"monthly": {MonthlyCostGuide: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := throttle.Validate(); err == nil {
				t.Fatal("negative cost guide was accepted")
			}
		})
	}
}
