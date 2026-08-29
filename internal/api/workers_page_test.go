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

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// workersCJKRE matches the CJK unified-ideograph and CJK-punctuation blocks.
// The page source must carry none of them: every piece of product copy now
// comes from the locale catalog, so a raw Chinese literal in workersPageHTML
// is a migration regression.
var workersCJKRE = regexp.MustCompile(`[\x{3000}-\x{303f}\x{3400}-\x{4dbf}\x{4e00}-\x{9fff}\x{f900}-\x{faff}]`)

// workersFragmentKeys loads the page's fragment file and returns its zh-CN
// key set. The fragment is not merged into the embedded catalogs yet, so the
// page tests assert the key wiring against the fragment on disk rather than a
// resolved zh-CN value.
func workersFragmentKeys(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("locales", "fragments", "workers.json"))
	if err != nil {
		t.Fatalf("read workers fragment: %v", err)
	}
	var frag struct {
		Keys map[string]map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(data, &frag); err != nil {
		t.Fatalf("parse workers fragment: %v", err)
	}
	keys := make(map[string]bool, len(frag.Keys[string(localeZhCN)]))
	for k := range frag.Keys[string(localeZhCN)] {
		keys[k] = true
	}
	return keys
}

// The page source must be fully migrated: no hardcoded CJK copy, the shared
// tdApiErrorMessage instead of a page-local apiErrMsg, and no browser-locale
// number/date formatters.
func TestWorkersPageI18nHasNoHardcodedChinese(t *testing.T) {
	if hits := workersCJKRE.FindAllString(workersPageHTML, -1); len(hits) > 0 {
		t.Fatalf("workersPageHTML still carries hardcoded CJK copy: %q", hits)
	}
	if strings.Contains(workersPageHTML, "apiErrMsg") {
		t.Fatal("page-local apiErrMsg must be deleted in favor of the shared tdApiErrorMessage")
	}
	if !strings.Contains(workersPageHTML, "tdApiErrorMessage(") {
		t.Fatal("workers page must call the shared tdApiErrorMessage helper")
	}
	for _, legacy := range []string{"toLocaleString(", "toLocaleTimeString", "toLocaleDateString"} {
		if strings.Contains(workersPageHTML, legacy) {
			t.Fatalf("workers page still uses browser-locale formatter %q", legacy)
		}
	}
}

// Every product string goes through the shared locale machinery: static HTML
// via [[i18n:*]] markers, dynamic copy via tdT/tdPlural (including the
// value→key enum maps and the shared status/common keys), and API failures
// via tdApiErrorMessage. A hardcoded Chinese string here means the migration
// regressed.
func TestWorkersPageLocalizedCopy(t *testing.T) {
	for _, marker := range []string{
		`<title>Timingdex · [[i18n:workers.title]]</title>`,
		`<div class="eyebrow">[[i18n:workers.title]]</div>`,
		`<h1>[[i18n:workers.title]]</h1>`,
		`<p class="muted">[[i18n:workers.subtitle]]</p>`,
		`href="/worker-setup">[[i18n:workers.addWorker]]`,
		`id="refresh">[[i18n:common.refresh]]`,
		`id="auth-callout" hidden><span>[[i18n:workers.authRequired]]</span>`,
		`id="login-prompt">[[i18n:workers.loginToView]]`,
		`id="admin-auth-callout" hidden><span>[[i18n:workers.adminAuthWarning]]</span>`,
		`[[i18n:workers.jobsTitle]]`,
		`id="advanced-pairing"><summary>[[i18n:workers.advancedPairing]]</summary>`,
		`[[i18n:workers.advancedPairingNoteLead]] <a href="/worker-setup">[[i18n:workers.installWizard]]</a>[[i18n:workers.advancedPairingNoteTrail]]`,
		`id="pairing-btn">[[i18n:workers.generatePairingToken]]`,
		`tdT('workers.cap.preview')`,
		`tdT('workers.cap.thumbnail')`,
		`tdT('workers.cap.audio')`,
		`tdT('workers.cap.none')`,
		`tdT('workers.listSeparator')`,
		`tdT('workers.rootsNone')`,
		`tdT('workers.hardwareSoftware')`,
		`tdT('workers.platformUnknown')`,
		`tdT('workers.versionLabel')`,
		`tdT('workers.versionUnknown')`,
		`tdT('workers.compat.incompatibleWithHub')`,
		`tdT('workers.compat.notDispatched')`,
		`tdT('status.online')`,
		`tdT('status.offline')`,
		`tdT('workers.rootsLabel')`,
		`tdT('workers.hardwareLabel')`,
		`tdT('workers.jobStageQueued')`,
		`tdT('workers.assignAuto')`,
		`tdT('workers.assignPreferred')`,
		`tdT('workers.assignRequired')`,
		`tdT('workers.attempts',{attempt:attempt,maxAttempts:maxAttempts})`,
		`tdT('workers.lastFailureLabel')`,
		`tdT('workers.leaseOwnerLabel')`,
		`tdJobState(j.state)`,
		`tdPlural('workers.fleetOnline'`,
		`tdPlural('workers.fleetOffline'`,
		`tdPlural('workers.fleetIncompatible'`,
		`tdPlural('workers.fleetRunning'`,
		`[[i18n:workers.loading]]`,
		`tdT('workers.emptyTitle')`,
		`tdT('workers.emptyWhy')`,
		`tdT('workers.emptyAction')`,
		`tdT('jobs.emptyTitle')`,
		`tdT('jobs.emptyWhy')`,
		`tdT('jobs.emptyAction')`,
		`tdT('workers.loginToViewWorkers')`,
		`tdT('workers.loginToViewJobs')`,
		`tdT('workers.pairingGenerating')`,
		`tdT('workers.oneTimePairingToken'`,
		`tdT('workers.tokenExpiresIn15')`,
		`tdT('workers.tokenSingleUse')`,
		`tdT('common.copy')`,
		`tdT('common.copied')`,
		`tdT('workers.goToInstallWizard')`,
		`tdT('workers.pairingError'`,
		`tdT('workers.copyPairingTokenPrompt')`,
		`tdT('workers.assignError'`,
		`tdT('workers.loadWorkersError'`,
		`tdT('workers.loadJobsError'`,
		`tdApiErrorMessage(`,
	} {
		if !strings.Contains(workersPageHTML, marker) {
			t.Fatalf("workers page missing localization wiring %q", marker)
		}
	}
}

// The shell injection depends on the same structural guarantees every other
// page constant carries: the SHELL_HEADER marker first inside <body>, and
// exactly one style close and one body close so the replace-first anchors
// stay exact.
func TestWorkersPageConstantStructure(t *testing.T) {
	bodyStart := strings.Index(workersPageHTML, "<body data-workers-page>")
	if bodyStart < 0 {
		t.Fatal("page constant has no <body data-workers-page>")
	}
	rest := workersPageHTML[bodyStart:]
	if !strings.HasPrefix(rest, "<body data-workers-page><!--SHELL_HEADER-->") {
		t.Fatal("the SHELL_HEADER marker must be the first element inside <body>")
	}
	if got := strings.Count(workersPageHTML, "</style>"); got != 1 {
		t.Fatalf("expected exactly one </style>, got %d", got)
	}
	if got := strings.Count(workersPageHTML, "</body>"); got != 1 {
		t.Fatalf("expected exactly one </body>, got %d", got)
	}
}

// Every static [[i18n:key]] marker and every literal key passed to
// tdT/tdPlural (including the value→key enum maps) must resolve: to a key the
// page fragment carries in all five locales, or to one of the shared catalog
// keys (status.* / common.*) that every locale already ships. Plural bases are
// resolved through their .one/.other siblings.
func TestWorkersPageI18nKeysResolveFromFragment(t *testing.T) {
	fragKeys := workersFragmentKeys(t)

	markerRE := regexp.MustCompile(`\[\[i18n:([a-zA-Z0-9._-]+)\]\]`)
	callRE := regexp.MustCompile(`td(?:T|Plural)\('([a-zA-Z0-9._-]+)'`)
	keyLitRE := regexp.MustCompile(`'(workers|status|shell|common|api|facet)\.[a-zA-Z0-9._-]+'`)

	seen := make(map[string]bool)
	for _, m := range markerRE.FindAllStringSubmatch(workersPageHTML, -1) {
		seen[m[1]] = true
	}
	for _, m := range callRE.FindAllStringSubmatch(workersPageHTML, -1) {
		seen[m[1]] = true
	}
	for _, m := range keyLitRE.FindAllStringSubmatch(workersPageHTML, -1) {
		seen[m[0][1:len(m[0])-1]] = true
	}

	if len(seen) == 0 {
		t.Fatal("no keys detected in workersPageHTML; the scan is broken")
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
		t.Fatalf("workers keys that resolve to nothing (not in fragment, its plural siblings, or a shared catalog): %v", unresolved)
	}
}

// The value→key enum tables must cover every wire value the page renders.
// Adding a new compat verdict or job state must fail this test until the map
// and the fragment both cover it.
func TestWorkersPageI18nEnumMapsCoverWireValues(t *testing.T) {
	for _, verdict := range []string{"compatible", "upgrade_recommended", "incompatible"} {
		if !strings.Contains(workersPageHTML, verdict+":['workers.compat.") {
			t.Fatalf("vmap missing compat verdict %q", verdict)
		}
	}
	if !strings.Contains(workersPageHTML, "vmap[compat.verdict]||['workers.compat.unknown'") {
		t.Fatal("vmap must fall back to the localized workers.compat.unknown key, never a raw string")
	}
	for _, st := range []string{"pending", "running", "succeeded", "failed", "skipped"} {
		if !strings.Contains(workersPageHTML, "==='"+st+"'?") {
			t.Fatalf("tdJobState missing mapping for job state %q", st)
		}
	}
	// The fleet summary must count through tdPlural, never raw concatenation.
	if !strings.Contains(workersPageHTML, "tdPlural('workers.fleetOnline',on)") {
		t.Fatal("fleet summary must render online count through tdPlural")
	}
}

// The fragment must be a valid UTF-8 JSON catalog fragment: the page name
// matches, all five locales carry the identical key set, and every key's
// {placeholder} set matches zh-CN. It must define only page-prefixed keys.
func TestWorkersPageI18nFragmentParityAcrossLocales(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("locales", "fragments", "workers.json"))
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
	if frag.Page != "workers" {
		t.Fatalf("fragment page=%q, want workers", frag.Page)
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
func TestWorkersPageI18nServedWithoutUnresolvedMarkers(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "workers-page.db"))
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
	handler.ServeHTTP(response, lanRequest(http.MethodGet, "/workers", nil))
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
		`data-workers-page`,
		`id="workers"`, `id="jobs"`, `id="fleet-summary"`, `id="auth-callout"`,
		`id="admin-auth-callout"`, `id="advanced-pairing"`, `id="pairing-btn"`,
		`id="pairing"`, `id="refresh"`, `id="login-prompt"`,
		`/api/v1/hub/workers`, `/api/v1/jobs?limit=100`, `/api/v1/hub/worker-pairings`,
		`/api/v1/admin/worker-jobs/`, `/assignment`,
		`worker_id`, `mode`, `assigned_worker_id`, `preferred_worker_id`,
		`capabilities`, `compat.verdict`, `library_roots`, `hardware`,
		`platform`, `status`, `job_type`, `state`, `attempt_count`, `max_attempts`,
		`/worker-setup`,
		`admin-token`, `loginAdmin`, `X-CSRF-Token`, `__Host-timingdex_csrf`,
		`tdApiErrorMessage`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("served /workers missing %q", want)
		}
	}
}
