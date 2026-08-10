package e2e

import "testing"

// TestFixtureManifestIsComplete encodes every core fixture in CI mode before
// the pipeline tests run. This catches a new manifest row that silently never
// reaches the deterministic end-to-end chain.
func TestFixtureManifestIsComplete(t *testing.T) {
	for _, fixture := range CoreFixtures {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			path := generateClip(t, t.TempDir(), fixture.Name, fixture.Opts)
			if path == "" {
				t.Fatal("fixture generator returned an empty path")
			}
		})
	}
}
