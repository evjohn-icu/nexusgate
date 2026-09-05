package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

func newCollectionsPageTestService(t *testing.T, name string) *app.Service {
	t.Helper()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// hanRE matches any CJK ideograph: the page constant must not contain a
// hardcoded Chinese string once the shared locale catalog owns all copy.
var hanRE = regexp.MustCompile(`[\p{Han}]`)

// The collections page serves the basket of pinned shots behind the shell
// sidebar: the marker must be replaced by the sidebar, the shell nav group
// must carry the /collections link, and the localization wiring (tdT keys for
// the empty/loading copy, tdApiErrorMessage for API failures) must be present
// in the served script. Like every other admin page here it never persists the
// Hub token.
func TestCollectionsPageServesShellAndEmptyCopy(t *testing.T) {
	service := newCollectionsPageTestService(t, "collections-page.db")
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/collections", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if ct := response.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type=%q", ct)
	}
	body := response.Body.String()
	for _, marker := range []string{
		`<aside class="shell-sidebar" data-app-shell>`,
		`href="/collections" data-nav="/collections" class="nav-link">收藏</a>`,
		`tdT('collections.empty')`,
		`tdT('collections.emptyShots')`,
		"id=\"admin-token\"",
		"type=\"password\"",
		"/api/v1/collections/",
		"/shots/reorder",
		"/api/v1/assets/",
		"/proxy#t=",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("collections page missing marker %q", marker)
		}
	}
	if strings.Contains(body, "localStorage") || strings.Contains(body, "sessionStorage") {
		t.Fatal("collections page must keep the admin token in page memory only")
	}
}

// Every product string in the page constant must go through the shared locale
// machinery: static HTML via [[i18n:*]] markers, dynamic copy via the runtime
// helpers, API failures via the shared tdApiErrorMessage, and date output via
// tdFormatDateTime. A hardcoded CJK string here means the migration regressed.
func TestCollectionsPageLocalizedCopy(t *testing.T) {
	for _, marker := range []string{
		`<title>NexusGate · [[i18n:collections.title]]</title>`,
		`<h1>[[i18n:collections.title]]</h1>`,
		`<p class="muted">[[i18n:collections.intro]]</p>`,
		`tdPlural('collections.shotCount'`,
		`tdT('collections.duration'`,
		`tdT('collections.createdAt'`,
		`tdT('collections.loadingShots')`,
		`tdT('collections.emptyWhy')`,
		`tdT('collections.deleteCollection')`,
		`tdT('collections.moveUp')`,
		`tdT('collections.moveDown')`,
		`tdT('collections.play')`,
		`tdT('collections.copyTimecode')`,
		`tdT('common.remove')`,
		`tdT('collections.deleteConfirm')`,
		`tdT('collections.timecodeCopied',{tc:text})`,
		`tdApiErrorMessage(`,
		`tdFormatDateTime(new Date(c.created_at))`,
	} {
		if !strings.Contains(collectionsHTML, marker) {
			t.Fatalf("collections page missing localization wiring %q", marker)
		}
	}
	if strings.Contains(collectionsHTML, "function apiErrMsg") {
		t.Fatal("collections page must use the shared tdApiErrorMessage, not a page-local apiErrMsg")
	}
	if strings.Contains(collectionsHTML, "toLocaleString") {
		t.Fatal("collections page must use tdFormatDateTime instead of toLocaleString")
	}
	if hit := hanRE.FindString(collectionsHTML); hit != "" {
		t.Fatalf("collections page constant contains hardcoded CJK copy %q; every string must go through the locale catalog", hit)
	}
}

// The skeleton migration moved the page onto the shared design classes: the
// header is a .pagehead, the collection card is a .panel (with the expand
// toggle composed as a .panel-head button), and the empty state is a
// three-part .empty block. The legacy page-local classes must be gone so the
// shell's compat layer can be deleted for them.
func TestCollectionsPageMigratedClasses(t *testing.T) {
	for _, legacy := range []string{"coll-card", "coll-head", "coll-empty"} {
		if strings.Contains(collectionsHTML, legacy) {
			t.Fatalf("collections page still carries legacy class %q", legacy)
		}
	}
	for _, want := range []string{
		`<header class="pagehead">`,
		`class="panel coll-panel" data-coll=`,
		`class="panel-head coll-toggle" data-action="toggle-collection"`,
		`'<div class="empty"><b>'`,
		`tdT('collections.emptyWhy')`,
		`class="btn btn--primary" href="/"`,
	} {
		if !strings.Contains(collectionsHTML, want) {
			t.Fatalf("collections page missing migrated marker %q", want)
		}
	}
}

// The shell injection depends on the same structural guarantees every other
// page constant carries: the marker first inside <body>, and exactly one
// style close and one body close so the replace-first anchors stay exact.
func TestCollectionsPageConstantStructure(t *testing.T) {
	bodyStart := strings.Index(collectionsHTML, "<body>")
	if bodyStart < 0 {
		t.Fatal("page constant has no <body>")
	}
	rest := collectionsHTML[bodyStart:]
	if !strings.HasPrefix(rest, "<body><!--SHELL_HEADER-->") {
		t.Fatal("the SHELL_HEADER marker must be the first element inside <body>")
	}
	if got := strings.Count(collectionsHTML, "</style>"); got != 1 {
		t.Fatalf("expected exactly one </style>, got %d", got)
	}
	if got := strings.Count(collectionsHTML, "</body>"); got != 1 {
		t.Fatalf("expected exactly one </body>, got %d", got)
	}
}

// The collections status line must be visible mutation feedback, not a
// display:none trap: collSay() renders the shared callout on a role="status"
// aria-live region, and a fresh successful load clears it while a delete's
// confirmation is re-asserted after the reload so it stays visible.
func TestCollectionsPageMutationFeedbackIsVisibleCallout(t *testing.T) {
	if strings.Contains(collectionsHTML, `.status{display:none}`) {
		t.Fatal("collections page must not hide its status line")
	}
	for _, marker := range []string{
		`id="collections-status" class="status" role="status" aria-live="polite"`,
		`function collSay(message,ok){const el=document.getElementById('collections-status');el.className='callout '+(ok?'callout--confirmed':'callout--contradicted');el.textContent=message}`,
		`st.className='status';st.textContent=''`,
		`collSay(tdT('collections.deleted'),true)`,
		`collSay(tdT('collections.loadError',{message:e.message}),false)`,
		`collSay(tdT('collections.reorderError',{message:e.message}),false)`,
		`collSay(tdT('collections.removeShotError',{message:e.message}),false)`,
		`collSay(tdT('collections.timecodeCopied',{tc:text}),true)`,
		`collSay(tdT('collections.copyTimecodeError',{message:e2.message}),false)`,
	} {
		if !strings.Contains(collectionsHTML, marker) {
			t.Fatalf("collections page missing callout marker %q", marker)
		}
	}
	// The delete confirmation must survive the reload that re-renders the
	// list, so the message is set after the load, not before it.
	if !strings.Contains(collectionsHTML, `await collectionsLoad();collSay(tdT('collections.deleted'),true)`) {
		t.Fatalf("delete confirmation must be shown after the reload")
	}
}
