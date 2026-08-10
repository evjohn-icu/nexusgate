package api

import (
	"strings"
	"testing"
)

// TestShellSidebarMarkup pins the sidebar shape the shell now injects: the
// <aside> shell, the brand row the branding layer rewrites, the three
// navigation groups of the IA (素材/创作/系统), the admin-token input every
// page script reads, and the four status cells. It also pins the one thing
// the shell deliberately does NOT do — no legacy top header.
func TestShellSidebarMarkup(t *testing.T) {
	html := shellHeaderHTML()

	for _, want := range []string{
		`<aside class="shell-sidebar" data-app-shell>`,
		`<span class="brand">Timingdex</span>`,
		`<span class="shell-tagline">本地素材智能层</span>`,
		`<span class="nav-group-title">素材</span>`,
		`<span class="nav-group-title">创作</span>`,
		`<span class="nav-group-title">系统</span>`,
		`id="admin-token"`,
		`oninput="refreshStatus()"`,
		`class="status-strip" data-status-strip`,
		`id="status-hub"`,
		`id="status-pipeline"`,
		`id="status-providers"`,
		`id="status-workers"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("shell sidebar missing %q", want)
		}
	}

	// The IA's 素材 group carries 素材库 and 收藏; 搜索 lives on /.
	if got := strings.Count(html, `class="nav-link"`); got != 10 {
		t.Fatalf("expected 10 nav links, got %d", got)
	}
	if !strings.Contains(html, `href="/" data-nav="/" class="nav-link">素材库</a>`) {
		t.Fatal("素材库 link to / missing")
	}
	if !strings.Contains(html, `href="/collections" data-nav="/collections" class="nav-link">收藏</a>`) {
		t.Fatal("收藏 link to /collections missing")
	}

	// The shell opens with the aside and the status strip lives inside it,
	// after the nav — the strip must not be a sibling outside the sidebar.
	if !strings.HasPrefix(html, `<aside class="shell-sidebar"`) {
		t.Fatal("shell markup must open with the sidebar aside")
	}
	navAt, stripAt, closeAt := strings.Index(html, `class="shell-nav"`), strings.Index(html, `data-status-strip`), strings.Index(html, `</aside>`)
	if navAt < 0 || stripAt < 0 || closeAt < 0 {
		t.Fatal("nav, status strip or aside close missing")
	}
	if !(navAt < stripAt && stripAt < closeAt) {
		t.Fatal("status strip must be inside the sidebar, after the nav")
	}

	if strings.Contains(html, `class="shell-header"`) {
		t.Fatal("legacy top-header markup still present in the shell")
	}
}

// TestShellCSSSidebarLayout pins the layout contract: the sidebar is a fixed
// left rail with the body-padding gutter, and the narrow-screen media query
// collapses it into an in-flow top bar with the gutter removed.
func TestShellCSSSidebarLayout(t *testing.T) {
	for _, want := range []string{
		`.shell-sidebar{position:fixed`,
		`left:0`,
		`width:220px`,
		`z-index:30`,
		`body{padding-left:220px}`,
		`@media(max-width:860px)`,
	} {
		if !strings.Contains(shellCSS, want) {
			t.Fatalf("shell CSS missing %q", want)
		}
	}
	narrow := shellCSS[strings.Index(shellCSS, "@media(max-width:860px)"):]
	if !strings.Contains(narrow, `body{padding-left:0}`) {
		t.Fatal("narrow-screen media query must remove the body gutter")
	}
	if !strings.Contains(narrow, `.shell-sidebar{position:relative`) {
		t.Fatal("narrow-screen media query must make the sidebar in-flow")
	}
}

// TestShellSidebarServedOnEveryPage verifies the sidebar markup (not just the
// CSS anchor) survives the injection and branding pass on every page constant.
func TestShellSidebarServedOnEveryPage(t *testing.T) {
	pages := []string{libraryIndexHTML, progressHTML, providersHTML, settingsHTML, workersPageHTML, tagsHTML, repurposeWorkspaceHTML, libraryRootsHTML, setupHTML, workerSetupPageHTML, collectionsHTML}
	for _, page := range pages {
		served := brandedPage(shelledPage(page))
		if !strings.Contains(served, `<aside class="shell-sidebar" data-app-shell>`) {
			t.Fatal("shelled page lost the sidebar aside")
		}
		if !strings.Contains(served, `id="status-providers"`) {
			t.Fatal("shelled page lost the status cells")
		}
		if !strings.Contains(served, `<span class="brand">`+productName+`</span>`) {
			t.Fatal("shelled page lost the branded brand span")
		}
		if strings.Index(served, "<body") > strings.Index(served, `class="shell-sidebar"`) {
			t.Fatal("sidebar is not the first element inside body")
		}
	}
}
