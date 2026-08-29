package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The /tags curator serves its HTML shell like every other page and keeps its
// static [[i18n:tags.*]] markers as markers (the server resolves them to the
// locale value once the tags fragment is merged, to the bare key before that).
// The ids the page script pins -- the three count cells, the two header
// actions, the cluster control, and the three list containers -- must all
// survive.
func TestTagsPageRendersWithMarkers(t *testing.T) {
	service := newLibraryRootsTestService(t, "tags-page.db")
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/tags", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if ct := response.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type=%q", ct)
	}
	body := response.Body.String()
	if strings.Contains(body, "[[i18n:") {
		t.Fatalf("unresolved marker leaked into the served page: %s", body)
	}
	// The shell is injected server-side, not in the constant.
	for _, shellMarker := range []string{"admin-token", `type="password"`} {
		if !strings.Contains(body, shellMarker) {
			t.Fatalf("served page missing shell marker %q", shellMarker)
		}
	}
}

// The skeleton migration (UI-005): the header is the shared .pagehead, the
// status line is a design .callout that maps success to callout--confirmed
// and failure to callout--contradicted, the summary pills ride the design
// .state modifiers, and the three list containers keep their ids.
func TestTagsPageSkeletonUsesDesignClasses(t *testing.T) {
	body := brandedPage(shelledPage(tagsHTML, localeZhCN))
	for _, marker := range []string{
		`<header class="pagehead">`,
		`<div class="eyebrow">[[i18n:tags.title]]</div>`,
		`[[i18n:tags.subtitle]]`,
		`id="curate-btn"`,
		`id="refresh-tags"`,
		`class="btn btn--primary" id="curate-btn"`,
		`class="state state--attention"`,
		`class="state state--draft"`,
		`class="state state--confirmed"`,
		`id="count-unresolved"`,
		`id="count-proposals"`,
		`id="count-canonical"`,
		`<div class="callout" id="tags-status"></div>`,
		`callout--confirmed`,
		`callout--contradicted`,
		`id="cluster-btn"`,
		`id="cluster-threshold"`,
		`id="unresolved"`,
		`id="proposals"`,
		`id="tags"`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("tags page missing design marker %q", marker)
		}
	}
}

// The curator must be off the legacy classes: the header is now the shared
// .pagehead and the status line is a design .callout, so the app_shell
// compat-layer rules for .head / .tags-status become deletable once no page
// uses them.
func TestTagsPageHasNoLegacyHeadOrTagsStatus(t *testing.T) {
	if strings.Contains(tagsHTML, `class="head"`) {
		t.Fatal("tags page still uses the legacy .head class")
	}
	if strings.Contains(tagsHTML, `class="tags-status"`) || strings.Contains(tagsHTML, `.tags-status`) || strings.Contains(tagsHTML, `'tags-status '`) {
		t.Fatal("tags page still uses the legacy .tags-status class")
	}
}

// The curate/cluster/review wiring and the loader must stay intact: the three
// endpoints, the empty-state rendering (three-part .empty blocks whose action
// runs curate()), and the button bindings.
func TestTagsPageWiringIntact(t *testing.T) {
	for _, marker := range []string{
		`async function safeLoad()`,
		`/api/v1/tags/unresolved`,
		`/api/v1/tags/proposals?state=pending`,
		`/api/v1/tags'`,
		`emptyBlock('tags.emptyUnresolved','tags.emptyUnresolvedWhy')`,
		`emptyBlock('tags.emptyProposals','tags.emptyProposalsWhy')`,
		`emptyBlock('tags.emptyCanonical','tags.emptyCanonicalWhy')`,
		`'<div class="empty"><span class="label">'`,
		`async function curate()`,
		`/api/v1/tags/curate`,
		`async function cluster()`,
		`/api/v1/tags/clusters?threshold=`,
		`async function review(id,action)`,
		`/api/v1/tags/proposals/`,
		`getElementById('curate-btn').addEventListener('click',curate)`,
		`getElementById('cluster-btn').addEventListener('click',cluster)`,
		`getElementById('refresh-tags').addEventListener('click',safeLoad)`,
		`tagStatus(tdT('tags.loadError',{msg:e.message}),false)`,
		`tagStatus(tdT('tags.curateError',{msg:e.message}),false)`,
		`tagStatus(tdT('tags.clusterError',{msg:e.message}),false)`,
		`tagStatus(tdT('tags.reviewError',{msg:e.message}),false)`,
	} {
		if !strings.Contains(tagsHTML, marker) {
			t.Fatalf("tags page missing wiring marker %q", marker)
		}
	}
}
