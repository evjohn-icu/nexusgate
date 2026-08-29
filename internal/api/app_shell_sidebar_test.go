package api

import (
	"strings"
	"testing"
)

// TestShellSidebarMarkup pins the sidebar shape the shell now injects: the
// <aside> shell, the brand row the branding layer rewrites, the three
// navigation groups of the IA (core links, a collapsible system group, and
// the Labs group), the admin login collapsed into a dialog behind a trigger,
// the four status cells, and the locale selector. The header is locale-aware,
// so the Chinese assertions run against the marker-resolved zh-CN rendering
// (the plan's rule: page markers are asserted as localized rendering, not as
// raw literals).
func TestShellSidebarMarkup(t *testing.T) {
	html := catalogs[localeZhCN].resolveMarkers(shellHeaderHTML(localeZhCN))

	for _, want := range []string{
		`<aside class="shell-sidebar" data-app-shell>`,
		`<span class="brand">Timingdex</span>`,
		`<span class="shell-tagline">LOCAL FOOTAGE INDEX</span>`,
		`<span class="nav-group-title">核心</span>`,
		`<details class="nav-group nav-group-system"><summary class="nav-group-title">系统</summary>`,
		`id="admin-token"`,
		`id="admin-trigger"`,
		`id="admin-dialog"`,
		`id="admin-login"`,
		`onclick="loginAdmin()"`,
		`id="admin-logout"`,
		`class="status-strip" data-status-strip`,
		`id="status-hub"`,
		`id="status-pipeline"`,
		`id="status-providers"`,
		`id="status-workers"`,
		`id="status-access"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("shell sidebar missing %q", want)
		}
	}

	// The collapsed admin login lives behind the trigger inside a <dialog>:
	// the token input is a dialog field, not a bare sidebar input.
	if !strings.Contains(html, `管理员访问`) {
		t.Fatal("admin dialog title not resolved in zh-CN rendering")
	}

	// The locale selector must expose every canonical tag with its native
	// name and preselect the rendered locale.
	if !strings.Contains(html, `id="shell-locale"`) {
		t.Fatal("shell sidebar missing the locale selector")
	}
	for _, loc := range supportedLocales {
		if !strings.Contains(html, `value="`+string(loc)+`" data-locale="`+string(loc)+`"`) {
			t.Fatalf("locale selector missing option %q", loc)
		}
	}
	if !strings.Contains(html, `<option value="zh-CN" data-locale="zh-CN" selected>简体中文</option>`) {
		t.Fatal("locale selector must preselect the rendered zh-CN locale")
	}
	if !strings.Contains(html, `<option value="ja-JP" data-locale="ja-JP">日本語</option>`) {
		t.Fatal("locale selector must list 日本語 with its canonical value")
	}

	// The core group carries the normal user loop; Search lives on /.
	if got := strings.Count(html, `class="nav-link"`); got != 9 {
		t.Fatalf("expected 9 nav links, got %d", got)
	}
	if !strings.Contains(html, `href="/" data-nav="/" class="nav-link">素材库</a>`) {
		t.Fatal("素材库 link to / missing")
	}
	if !strings.Contains(html, `href="/collections" data-nav="/collections" class="nav-link">收藏</a>`) {
		t.Fatal("收藏 link to /collections missing")
	}
	if !strings.Contains(html, `href="/progress" data-nav="/progress" class="nav-link">处理</a>`) {
		t.Fatal("处理 link to /progress missing")
	}
	// The system group folds the admin destinations under its collapsible
	// summary; the setup wizards leave the sidebar entirely.
	if !strings.Contains(html, `href="/library-roots" data-nav="/library-roots" class="nav-link">素材目录</a>`) {
		t.Fatal("素材目录 link to /library-roots missing")
	}
	if !strings.Contains(html, `href="/providers" data-nav="/providers" class="nav-link">模型服务</a>`) {
		t.Fatal("模型服务 link to /providers missing")
	}
	if !strings.Contains(html, `href="/workers" data-nav="/workers" class="nav-link">处理节点</a>`) {
		t.Fatal("处理节点 link to /workers missing")
	}
	if !strings.Contains(html, `href="/tags" data-nav="/tags" class="nav-link">Tags</a>`) {
		t.Fatal("Tags link to /tags missing")
	}
	if !strings.Contains(html, `href="/settings" data-nav="/settings" class="nav-link">设置</a>`) {
		t.Fatal("设置 link to /settings missing")
	}
	if strings.Contains(html, `href="/worker-setup"`) || strings.Contains(html, `href="/setup"`) {
		t.Fatal("setup wizard links must not appear in the sidebar")
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
		`width:var(--rail-w)`,
		`z-index:30`,
		`body{padding-left:var(--rail-w)}`,
		`--rail-w:240px`,
		`@media(max-width:900px)`,
	} {
		if !strings.Contains(shellCSS, want) {
			t.Fatalf("shell CSS missing %q", want)
		}
	}
	narrow := shellCSS[strings.Index(shellCSS, "@media(max-width:900px)"):]
	if !strings.Contains(narrow, `body{padding-left:0`) || !strings.Contains(narrow, `overflow-x:hidden`) {
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
		served := brandedPage(shelledPage(page, localeZhCN))
		if !strings.Contains(served, `<aside class="shell-sidebar" data-app-shell>`) {
			t.Fatal("shelled page lost the sidebar aside")
		}
		if !strings.Contains(served, `id="status-providers"`) {
			t.Fatal("shelled page lost the status cells")
		}
		if !strings.Contains(served, `<span class="brand">Re<i>:</i>Footage</span>`) {
			t.Fatal("shelled page lost the branded brand span")
		}
		if strings.Index(served, "<body") > strings.Index(served, `class="shell-sidebar"`) {
			t.Fatal("sidebar is not the first element inside body")
		}
	}
}
