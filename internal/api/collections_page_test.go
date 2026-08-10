package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
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
	service, err := app.NewService(repo, config.Config{DataDir: t.TempDir(), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// The collections page serves the basket of pinned shots behind the shell
// sidebar: the marker must be replaced by the sidebar, the 素材 nav group must
// carry the 收藏 link, and the empty copy tells an operator where shots get
// pinned. Like every other admin page here it never persists the Hub token.
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
		"还没有收藏。在素材库的搜索结果里把镜头加入收藏。",
		"这个收藏还没有镜头。",
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
