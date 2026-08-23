package api

import (
	"strings"
	"testing"
)

// brandedPage rewrites brand copy on every page it serves. Its anchors are the
// pre-branding text on the page constants; a stale anchor makes
// strings.NewReplacer no-op silently, the page still builds and serves and the
// branding is just gone. This test pins the anchors as data by requiring each
// to occur in at least one served page — the same guard
// TestLibraryPagePatchesApplyInOrderAndBite gives the library overlay.
func TestBrandedPageAnchorsBite(t *testing.T) {
	all := strings.Join([]string{
		libraryIndexHTML,
		providersHTML,
		setupHTML,
		progressHTML,
		repurposeWorkspaceHTML,
		tagsHTML,
		libraryRootsHTML,
		workersPageHTML,
		settingsHTML,
		workerSetupPageHTML,
		// The brand span now lives in the shared shell header rather than in
		// each page constant (app_shell.go): the shell header is served on
		// every page, so its markup is a branding carrier too.
		shellHeaderHTML(localeZhCN),
	}, "")
	for i, p := range brandReplacements {
		if !strings.Contains(all, p.anchor) {
			name := p.anchor
			if len(name) > 80 {
				name = name[:80] + "…"
			}
			t.Fatalf("branding replacement %d: anchor found in no served page (silent no-op): %q", i, name)
		}
	}
}
