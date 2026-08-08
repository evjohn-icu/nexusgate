package app

import (
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/media"
)

// A mistyped deferral must not reach RunUntilIdle. Zero or negative would
// re-arm an exhausted job instantly, so the pipeline would spin on a dead
// route making a paid provider call per pass — the exact failure the park
// exists to prevent. Flooring happens in the constructor, so no caller of the
// pipeline can feed the loop a value it cannot actually wait on.
func TestNewPipelineFloorsAUsableProviderRouteDeferral(t *testing.T) {
	tests := []struct {
		name  string
		given time.Duration
		want  time.Duration
	}{
		{name: "zero falls back to the default", given: 0, want: providerRouteDeferral},
		{name: "negative falls back to the default", given: -time.Hour, want: providerRouteDeferral},
		{name: "sub-floor value is raised to the floor", given: time.Second, want: minProviderRouteDeferral},
		{name: "a sane configured value is kept", given: 90 * time.Minute, want: 90 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPipeline(nil, "", nil, nil, nil, nil, nil, media.HardwarePlan{}, nil, tc.given)
			if p.routeDeferral != tc.want {
				t.Fatalf("routeDeferral=%s, want %s", p.routeDeferral, tc.want)
			}
		})
	}
}
