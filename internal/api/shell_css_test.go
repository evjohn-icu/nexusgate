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
		served := brandedPage(shelledPage(page, localeZhCN))
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

// The shared table wrapper scrolls internally on narrow viewports: the
// document must never overflow horizontally because a wide table was not
// contained. .table-scroll is capped at the viewport width and scrolls its
// own box.
func TestShellTableScrollIsContained(t *testing.T) {
	if !strings.Contains(shellCSS, ".table-scroll{overflow-x:auto;max-width:100%}") {
		t.Fatal("shared .table-scroll must carry max-width:100% so wide tables scroll internally")
	}
}

// The jobs table, the library-root health table and all three Tags tables must
// use the shared containment wrapper; Tags' page-local table{display:block}
// overflow rule that fought the shell is deleted.
func TestResponsiveTableWrappers(t *testing.T) {
	if !strings.Contains(progressHTML, `'<div class="table-scroll"><table class="table"><tr><th>'+esc(tdT('progress.jobs.type'))`) {
		t.Fatal("progress jobs table must be wrapped in the shared table-scroll")
	}
	if !strings.Contains(progressHTML, `+'</table></div>':tdT('progress.jobs.emptyPrefix')`) {
		t.Fatal("progress jobs table wrapper must close after the table")
	}
	if !strings.Contains(libraryRootsHTML, `'<div class="table-scroll"><table class="table"><tr><th>'+esc(tdT('roots.colPath'))`) {
		t.Fatal("library-roots health table must be wrapped in the shared table-scroll")
	}
	for _, fn := range []string{"unresolvedTable", "proposalTable", "tagTable"} {
		if !strings.Contains(tagsHTML, `function `+fn+`(xs){return '<div class="table-scroll"><table class="table">`) {
			t.Fatalf("tags %s table must be wrapped in the shared table-scroll", fn)
		}
	}
	if strings.Contains(tagsHTML, "table{display:block;overflow-x:auto") {
		t.Fatal("Tags' page-local table overflow rule must be deleted")
	}
}
