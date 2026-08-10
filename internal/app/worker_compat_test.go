package app

import (
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

func TestWorkerCompatibility(t *testing.T) {
	cases := []struct {
		name    string
		version string
		want    string
	}{
		{"at floor", "v0.26.0", domain.WorkerVerdictCompatible},
		{"patch above floor", "v0.26.5", domain.WorkerVerdictCompatible},
		{"higher minor same major", "v0.99.0", domain.WorkerVerdictCompatible},
		{"current-dev minor", "v0.29.1", domain.WorkerVerdictCompatible},
		{"pre-release of floor", "v0.26.0-alpha", domain.WorkerVerdictCompatible},
		{"no v prefix", "0.27.0", domain.WorkerVerdictCompatible},
		{"single component at floor", "0.26", domain.WorkerVerdictCompatible},

		{"empty string", "", domain.WorkerVerdictUpgradeRecommended},
		{"dev build default", "dev", domain.WorkerVerdictUpgradeRecommended},
		{"unparseable", "build-2026", domain.WorkerVerdictUpgradeRecommended},
		{"bare version name", "v0.x", domain.WorkerVerdictUpgradeRecommended},

		{"minor below floor", "v0.24.0", domain.WorkerVerdictIncompatible},
		{"one patch below floor", "v0.25.9", domain.WorkerVerdictIncompatible},
		{"single component below floor", "0.24", domain.WorkerVerdictIncompatible},
		{"major mismatch", "v1.0.0", domain.WorkerVerdictIncompatible},
		{"pre-release below floor", "v0.25.0-beta", domain.WorkerVerdictIncompatible},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WorkerCompatibility(tc.version)
			if got.Verdict != tc.want {
				t.Fatalf("WorkerCompatibility(%q) verdict=%q want %q", tc.version, got.Verdict, tc.want)
			}
			if got.MinVersion != MinWorkerVersion {
				t.Fatalf("WorkerCompatibility(%q) min_version=%q want %q", tc.version, got.MinVersion, MinWorkerVersion)
			}
			if got.WorkerVersion != tc.version {
				t.Fatalf("WorkerCompatibility(%q) worker_version=%q want %q", tc.version, got.WorkerVersion, tc.version)
			}
			if tc.want == domain.WorkerVerdictIncompatible && got.Message == "" {
				t.Fatalf("incompatible verdict for %q must carry a human message", tc.version)
			}
			if tc.want != domain.WorkerVerdictIncompatible && got.Message != "" {
				t.Fatalf("verdict %q for %q must not carry a message, got %q", tc.want, tc.version, got.Message)
			}
		})
	}
}

func TestWorkerCompatibilityIncompatibleMessage(t *testing.T) {
	got := WorkerCompatibility("v0.24.0")
	if !strings.Contains(got.Message, "v0.24.0") {
		t.Fatalf("message should name the worker version: %q", got.Message)
	}
	if !strings.Contains(got.Message, MinWorkerVersion) {
		t.Fatalf("message should name the minimum version: %q", got.Message)
	}
	if !strings.Contains(got.Message, "update the worker binary") {
		t.Fatalf("message should point at the remedy: %q", got.Message)
	}
}

// The repository lease gate cannot import app, so the verdict lives in
// domain and this alias is what the API uses. The two must never disagree:
// this test pins the alias to the domain implementation.
func TestWorkerCompatibilityAliasesDomain(t *testing.T) {
	for _, version := range []string{"", "dev", "v0.24.0", "v0.26.0", "v1.0.0", "garbage"} {
		if got, want := WorkerCompatibility(version), domain.WorkerCompatibility(version); got != want {
			t.Fatalf("WorkerCompatibility(%q)=%+v diverges from domain %+v", version, got, want)
		}
	}
}
