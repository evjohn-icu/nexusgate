package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

// repurposeCJKRE matches the CJK unified-ideograph and CJK-punctuation blocks.
// The page source must carry none of them: every piece of product copy now
// comes from the locale catalog, so a raw Chinese literal in
// repurposeWorkspaceHTML is a migration regression.
var repurposeCJKRE = regexp.MustCompile(`[\x{3000}-\x{303f}\x{3400}-\x{4dbf}\x{4e00}-\x{9fff}\x{f900}-\x{faff}]`)

// repurposeFragmentKeys loads the page's fragment file and returns its zh-CN
// key set. The fragment is not merged into the embedded catalogs yet, so the
// page tests assert the key wiring against the fragment on disk rather than a
// resolved zh-CN value.
func repurposeFragmentKeys(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("locales", "fragments", "repurpose.json"))
	if err != nil {
		t.Fatalf("read repurpose fragment: %v", err)
	}
	var frag struct {
		Keys map[string]map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(data, &frag); err != nil {
		t.Fatalf("parse repurpose fragment: %v", err)
	}
	keys := make(map[string]bool, len(frag.Keys[string(localeZhCN)]))
	for k := range frag.Keys[string(localeZhCN)] {
		keys[k] = true
	}
	return keys
}

func TestRepurposePageI18nHasNoHardcodedChinese(t *testing.T) {
	if hits := repurposeCJKRE.FindAllString(repurposeWorkspaceHTML, -1); len(hits) > 0 {
		t.Fatalf("repurposeWorkspaceHTML still carries hardcoded CJK copy: %q", hits)
	}
}

func TestRepurposePageI18nUsesSharedHelpers(t *testing.T) {
	if strings.Contains(repurposeWorkspaceHTML, "apiErrMsg") {
		t.Fatal("page-local apiErrMsg must be deleted in favor of the shared tdApiErrorMessage")
	}
	if !strings.Contains(repurposeWorkspaceHTML, "tdApiErrorMessage(") {
		t.Fatal("repurpose page must call the shared tdApiErrorMessage helper")
	}
	for _, legacy := range []string{"toLocaleTimeString", "toLocaleString("} {
		if strings.Contains(repurposeWorkspaceHTML, legacy) {
			t.Fatalf("repurpose page still uses browser-locale formatter %q", legacy)
		}
	}
	if !strings.Contains(repurposeWorkspaceHTML, "tdFormatDateTime") {
		t.Fatal("repurpose page must use the shared tdFormatDateTime formatter")
	}
	// The hand-rolled date formatter must delegate to the shared helper, not
	// re-implement string assembly.
	if strings.Contains(repurposeWorkspaceHTML, "getFullYear()") {
		t.Fatal("repurpose page still hand-rolls a date formatter")
	}
}

// Every static [[i18n:key]] marker and every literal key passed to
// tdT/tdPlural (including the value→key enum maps) must resolve: to a key the
// page fragment carries in all five locales, or to one of the shared catalog
// keys (common.* / status.* / shell.*) that every locale already ships.
// Plural bases are resolved through their .one/.other siblings.
func TestRepurposePageI18nKeysResolveFromFragment(t *testing.T) {
	fragKeys := repurposeFragmentKeys(t)

	markerRE := regexp.MustCompile(`\[\[i18n:([a-zA-Z0-9._-]+)\]\]`)
	callRE := regexp.MustCompile(`td(?:T|Plural)\('([a-zA-Z0-9._-]+)'`)
	keyLitRE := regexp.MustCompile(`'(repurpose|common|api|status|shell|facet)\.[a-zA-Z0-9._-]+'`)

	seen := make(map[string]bool)
	for _, m := range markerRE.FindAllStringSubmatch(repurposeWorkspaceHTML, -1) {
		seen[m[1]] = true
	}
	for _, m := range callRE.FindAllStringSubmatch(repurposeWorkspaceHTML, -1) {
		seen[m[1]] = true
	}
	for _, m := range keyLitRE.FindAllStringSubmatch(repurposeWorkspaceHTML, -1) {
		seen[m[0][1:len(m[0])-1]] = true
	}

	if len(seen) == 0 {
		t.Fatal("no keys detected in repurposeWorkspaceHTML; the scan is broken")
	}

	var unresolved []string
	for key := range seen {
		if fragKeys[key] {
			continue
		}
		// A plural base key resolves via its category siblings.
		if fragKeys[key+".one"] || fragKeys[key+".other"] {
			continue
		}
		if catalogs[localeZhCN].has(key) {
			continue
		}
		unresolved = append(unresolved, key)
	}
	sort.Strings(unresolved)
	if len(unresolved) > 0 {
		t.Fatalf("repurpose keys that resolve to nothing (not in fragment, its plural siblings, or a shared catalog): %v", unresolved)
	}
}

// The skeleton migration moved the workspace onto the shared design classes:
// pagehead header, .panel candidate cards, page-local list-row buttons, and
// .callout/.status/.empty for the banner/status/empty surfaces. The legacy
// page-local classes must be gone so the shell's compat layer can be deleted
// for them, and the inbox/workspace empty states must carry a New-plan action.
func TestRepurposePageMigratedClasses(t *testing.T) {
	for _, legacy := range []string{".repurpose-head", "plan-list-item", "revision-item", ".plan-head", ".history-banner", ".statusline", ".ui-empty", "class=\"candidate ", ".candidate{"} {
		if strings.Contains(repurposeWorkspaceHTML, legacy) {
			t.Fatalf("repurpose page still carries legacy class %q", legacy)
		}
	}
	for _, want := range []string{
		`<header class="pagehead">`,
		`class="plan-row"`,
		`class="revision-row"`,
		`class="panel candidate-card`,
		`class="plan-titlebar"`,
		`class="callout" id="history-banner"`,
		`class="status" id="statusline"`,
		`class="empty" id="workspace-empty"`,
		`data-open-new-plan`,
		`tdT('repurpose.inboxEmptyWhy')`,
		`[[i18n:repurpose.workspaceEmptyWhy]]`,
	} {
		if !strings.Contains(repurposeWorkspaceHTML, want) {
			t.Fatalf("repurpose page missing migrated marker %q", want)
		}
	}
}

// The value→key tables must cover every wire value the page renders and keep
// the raw wire role as the fallback for unknown roles. Adding a new plan
// status, revision state, or section role must fail this test until the map
// and the fragment both cover it.
func TestRepurposePageI18nEnumMapsCoverWireValues(t *testing.T) {
	for _, role := range []string{"opening", "hook", "body", "ending", "transition", "broll", "cta"} {
		if !strings.Contains(repurposeWorkspaceHTML, role+":'repurpose.role."+role+"'") {
			t.Fatalf("roleKeys missing section role %q", role)
		}
	}
	if !strings.Contains(repurposeWorkspaceHTML, "tdT(roleKeys[role]||role)") {
		t.Fatal("roleName must fall back to the raw wire role for unknown roles")
	}
	for _, pair := range []string{
		"'repurpose.status.approved':'repurpose.status.pending'",
		"'repurpose.status.approved':'repurpose.revision.draft'",
	} {
		if !strings.Contains(repurposeWorkspaceHTML, pair) {
			t.Fatalf("status/revision value→key mapping missing %q", pair)
		}
	}
	for _, action := range []string{"choose", "exclude", "preview", "alternatives"} {
		if !strings.Contains(repurposeWorkspaceHTML, `data-action="`+action+`"`) {
			t.Fatalf("candidate/section actions missing data-action=%q", action)
		}
	}
	if !strings.Contains(repurposeWorkspaceHTML, `data-action="'+(s.locked?'unlock':'lock')+'"`) {
		t.Fatal("section lock/unlock action is not wired dynamically")
	}
}

func TestRepurposePageI18nFragmentParityAcrossLocales(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("locales", "fragments", "repurpose.json"))
	if err != nil {
		t.Fatal(err)
	}
	var frag struct {
		Page string                       `json:"page"`
		Keys map[string]map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(data, &frag); err != nil {
		t.Fatal(err)
	}
	if frag.Page != "repurpose" {
		t.Fatalf("fragment page=%q, want repurpose", frag.Page)
	}
	for _, loc := range supportedLocales {
		if _, ok := frag.Keys[string(loc)]; !ok {
			t.Fatalf("fragment missing locale %s", loc)
		}
	}

	base := frag.Keys[string(localeZhCN)]
	for _, loc := range supportedLocales[1:] {
		other := frag.Keys[string(loc)]
		if len(other) != len(base) {
			t.Fatalf("locale %s has %d keys, zh-CN has %d", loc, len(other), len(base))
		}
		for k := range base {
			if _, ok := other[k]; !ok {
				t.Fatalf("locale %s missing key %q", loc, k)
			}
		}
	}

	phRE := regexp.MustCompile(`\{[a-zA-Z]+\}`)
	phSet := func(s string) string {
		parts := phRE.FindAllString(s, -1)
		sort.Strings(parts)
		return strings.Join(parts, ",")
	}
	for k, zh := range base {
		want := phSet(zh)
		for _, loc := range supportedLocales[1:] {
			if got := phSet(frag.Keys[string(loc)][k]); got != want {
				t.Fatalf("placeholder set differs for %s in %s: %q vs zh-CN %q", k, loc, frag.Keys[string(loc)][k], zh)
			}
		}
	}

	// The fragment must define page-prefixed keys only, never the shared ones.
	for k := range base {
		for _, prefix := range []string{"common.", "api.", "status.", "facet.", "shell."} {
			if strings.HasPrefix(k, prefix) {
				t.Fatalf("fragment redefines shared key %q", k)
			}
		}
	}
}

// Served in the default zh-CN locale, every static marker must be resolved by
// the server (to the zh-CN value once the fragment is merged, to the bare key
// before that) rather than leaking as [[i18n:...]], and the structural
// anchors the other pages and tests rely on must survive.
func TestRepurposePageI18nServedWithoutUnresolvedMarkers(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "repurpose-page.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewService(repo, config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodGet, "/repurpose", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, "[[i18n:") {
		t.Fatalf("unresolved marker leaked into the served page: %s", body)
	}
	for _, want := range []string{
		`<html lang="zh-CN"`,
		`data-app-shell`,
		`id="plan-list"`, `id="workspace"`, `id="workspace-empty"`, `id="plan-title"`,
		`id="plan-status"`, `id="plan-meta"`, `id="plan-id"`, `id="copy-id"`,
		`id="revision-history"`, `id="history-banner"`, `id="result"`, `id="statusline"`,
		`id="save-rev"`, `id="approve"`, `id="new-plan"`, `id="refresh-inbox"`,
		`id="new-plan-dialog"`, `id="plan-shot-dialog"`, `id="revision-dialog"`,
		`id="plan-form"`, `id="brief"`, `id="duration"`, `id="style"`, `id="audience"`,
		`id="error"`, `id="create"`, `id="shot-video"`, `id="shot-asset"`,
		`id="shot-time"`, `id="shot-duration"`, `id="shot-score"`, `id="revision-list"`,
		`data-filter="draft"`, `data-filter="approved"`, `data-filter="all"`,
		`data-action="choose"`, `data-action="exclude"`,
		`data-action="'+(s.locked?'unlock':'lock')+'"`,
		`data-action="alternatives"`, `data-action="preview"`,
		`data-role=`,
		`brief`, `duration_ms`, `style`, `audience`, `sections`, `editor_note`,
		`latest_revision`, `missing_needs_count`, `selected_shot_id`, `excluded_shot_ids`,
		`/api/v1/repurpose/plans`, `/revisions`, `/approve`,
		`/api/v1/search/shots/hybrid`, `/api/v1/assets/`,
		`admin-token`, `X-CSRF-Token`, `__Host-nexusgate_csrf`,
		`tdApiErrorMessage`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("served /repurpose missing %q", want)
		}
	}
	// The static title marker resolves through the selected catalog (to the
	// zh-CN value once the fragment is merged, to the bare key before that),
	// never leaking as an unresolved marker.
	if wantTitle := catalogs[localeZhCN].resolveMarkers("[[i18n:repurpose.title]]"); !strings.Contains(body, wantTitle) {
		t.Fatalf("served /repurpose missing resolved title %q", wantTitle)
	}
}
