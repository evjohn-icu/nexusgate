package e2e

import "testing"

func TestFixtureFailureShouldSkip(t *testing.T) {
	tests := []struct {
		name          string
		mediaOptional string
		ci            string
		githubActions string
		wantSkip      bool
	}{
		{name: "optional locally", mediaOptional: "1", wantSkip: true},
		{name: "optional in CI", mediaOptional: "1", ci: "true", githubActions: "true", wantSkip: true},
		{name: "local default", wantSkip: true},
		{name: "CI default", ci: "true", wantSkip: false},
		{name: "GitHub Actions default", githubActions: "true", wantSkip: false},
		{name: "either CI marker is sufficient", ci: "true", githubActions: "true", wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fixtureFailureShouldSkip(tt.mediaOptional, tt.ci, tt.githubActions); got != tt.wantSkip {
				t.Fatalf("fixtureFailureShouldSkip(%q, %q, %q) = %t, want %t", tt.mediaOptional, tt.ci, tt.githubActions, got, tt.wantSkip)
			}
		})
	}
}
