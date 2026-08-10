package api

import (
	"strings"
	"testing"
)

// TestShellCSSLandsInsideStyleBlock pins the injection bug: the shell CSS
// must end up INSIDE the page's <style> element, not after </style> where
// the browser treats it as raw text and the shell silently loses all its
// styling (every page's shell then depended on the legacy header{} rules by
// accident).
func TestShellCSSLandsInsideStyleBlock(t *testing.T) {
	pages := []string{libraryIndexHTML, progressHTML, providersHTML, settingsHTML, workersPageHTML, tagsHTML, repurposeWorkspaceHTML, libraryRootsHTML, setupHTML, workerSetupPageHTML, collectionsHTML}
	for _, page := range pages {
		served := brandedPage(shelledPage(page))
		styleOpen := strings.Index(served, "<style>")
		styleClose := strings.Index(served, "</style>")
		shellAt := strings.Index(served, ".shell-sidebar{")
		if styleOpen < 0 || styleClose < 0 {
			t.Fatal("page has no style block")
		}
		if shellAt < 0 || shellAt > styleClose {
			t.Fatalf("shell CSS not inside the style block (shellAt=%d close=%d)", shellAt, styleClose)
		}
		if strings.Count(served, "</style>") != 1 {
			t.Fatalf("expected exactly one style close, got %d", strings.Count(served, "</style>"))
		}
	}
}
