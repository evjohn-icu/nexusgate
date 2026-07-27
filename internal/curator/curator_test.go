package curator

import (
	"testing"

	"github.com/ev/timingdex/internal/domain"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"Night City":        "night_city",
		"behind-the-scenes": "behind_the_scenes",
		" 城市夜景 ":            "城市夜景",
	}
	for input, want := range cases {
		if got := Normalize(input); got != want {
			t.Fatalf("Normalize(%q)=%q want %q", input, got, want)
		}
	}
}

func TestBuildProposalsGroupsAliases(t *testing.T) {
	items := []domain.UnresolvedTag{
		{NormalizedTag: "night_city", UsageCount: 4, AssetCount: 4},
		{NormalizedTag: "城市夜景", UsageCount: 3, AssetCount: 3},
	}
	proposals := BuildProposals(items, nil)
	if len(proposals) != 1 {
		t.Fatalf("got %d proposals", len(proposals))
	}
	if proposals[0].CanonicalName != "urban_night" {
		t.Fatalf("canonical=%q", proposals[0].CanonicalName)
	}
}
