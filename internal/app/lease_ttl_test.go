package app

import (
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// TestLeaseTTLByStage pins the stage-specific lease ceilings the pipeline
// hands to LeaseNextJob: derive encodes, windowed analysis and ASR routinely
// outlive the old flat 2-minute lease, and an expired lease is what lets a
// second executor reclaim the job and burn the same paid calls twice.
func TestLeaseTTLByStage(t *testing.T) {
	cases := []struct {
		typ domain.JobType
		ttl time.Duration
	}{
		{domain.JobProbe, 2 * time.Minute},
		{domain.JobDerive, 30 * time.Minute},
		{domain.JobTranscribe, 15 * time.Minute},
		{domain.JobAnalyze, 20 * time.Minute},
		{domain.JobSpeechGate, 2 * time.Minute},
		{domain.JobAlign, 2 * time.Minute},
		{domain.JobNormalize, 2 * time.Minute},
		{domain.JobIndex, 2 * time.Minute},
	}
	for _, c := range cases {
		if got := leaseTTL(c.typ); got != c.ttl {
			t.Errorf("leaseTTL(%s) = %v, want %v", c.typ, got, c.ttl)
		}
	}
}
