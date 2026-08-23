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

// cjkRE matches the CJK unified-ideograph and CJK-punctuation blocks. The page
// source must carry none of them: every piece of product copy now comes from
// the locale catalog, so a raw Chinese literal in progressHTML is a migration
// regression.
var cjkRE = regexp.MustCompile(`[\x{3000}-\x{303f}\x{3400}-\x{4dbf}\x{4e00}-\x{9fff}\x{f900}-\x{faff}]`)

// progressFragmentKeys loads the page's fragment file and returns its zh-CN
// key set. The fragment is not merged into the embedded catalogs yet, so the
// page tests assert the key wiring against the fragment on disk rather than a
// resolved zh-CN value.
func progressFragmentKeys(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("locales", "fragments", "progress.json"))
	if err != nil {
		t.Fatalf("read progress fragment: %v", err)
	}
	var frag struct {
		Keys map[string]map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(data, &frag); err != nil {
		t.Fatalf("parse progress fragment: %v", err)
	}
	keys := make(map[string]bool, len(frag.Keys[string(localeZhCN)]))
	for k := range frag.Keys[string(localeZhCN)] {
		keys[k] = true
	}
	return keys
}

func TestProgressPageI18nHasNoHardcodedChinese(t *testing.T) {
	if hits := cjkRE.FindAllString(progressHTML, -1); len(hits) > 0 {
		t.Fatalf("progressHTML still carries hardcoded CJK copy: %q", hits)
	}
}

func TestProgressPageI18nUsesSharedHelpers(t *testing.T) {
	if strings.Contains(progressHTML, "apiErrMsg") {
		t.Fatal("page-local apiErrMsg must be deleted in favor of the shared tdApiErrorMessage")
	}
	if !strings.Contains(progressHTML, "tdApiErrorMessage(") {
		t.Fatal("progress page must call the shared tdApiErrorMessage helper")
	}
	for _, legacy := range []string{"toLocaleTimeString", "toLocaleString("} {
		if strings.Contains(progressHTML, legacy) {
			t.Fatalf("progress page still uses browser-locale formatter %q", legacy)
		}
	}
	for _, helper := range []string{"tdFormatTime(", "tdFormatDateTime("} {
		if !strings.Contains(progressHTML, helper) {
			t.Fatalf("progress page must use shared formatter %q", helper)
		}
	}
}

// Every static [[i18n:key]] marker and every literal key passed to
// tdT/tdPlural (including the value→key enum maps) must resolve: to a key the
// page fragment carries in all five locales, or to one of the shared catalog
// keys (status.* / shell.*) that every locale already ships. Plural bases are
// resolved through their .one/.other siblings.
func TestProgressPageI18nKeysResolveFromFragment(t *testing.T) {
	fragKeys := progressFragmentKeys(t)

	markerRE := regexp.MustCompile(`\[\[i18n:([a-zA-Z0-9._-]+)\]\]`)
	callRE := regexp.MustCompile(`td(?:T|Plural)\('([a-zA-Z0-9._-]+)'`)
	keyLitRE := regexp.MustCompile(`'(progress|status|shell|common|api|facet)\.[a-zA-Z0-9._-]+'`)

	seen := make(map[string]bool)
	for _, m := range markerRE.FindAllStringSubmatch(progressHTML, -1) {
		seen[m[1]] = true
	}
	for _, m := range callRE.FindAllStringSubmatch(progressHTML, -1) {
		seen[m[1]] = true
	}
	for _, m := range keyLitRE.FindAllStringSubmatch(progressHTML, -1) {
		seen[m[0][1:len(m[0])-1]] = true
	}

	if len(seen) == 0 {
		t.Fatal("no keys detected in progressHTML; the scan is broken")
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
		t.Fatalf("progress keys that resolve to nothing (not in fragment, its plural siblings, or a shared catalog): %v", unresolved)
	}
}

// The value→key enum tables must cover every wire value the page renders.
// Adding a new job state, pipeline job type, or issue category must fail this
// test until the map and the fragment both cover it.
func TestProgressPageI18nEnumMapsCoverWireValues(t *testing.T) {
	jobStates := []string{"pending", "running", "succeeded", "failed", "skipped"}
	for _, st := range jobStates {
		if !strings.Contains(progressHTML, "==='"+st+"'?") {
			t.Fatalf("tdJobState missing mapping for job state %q", st)
		}
	}
	jobTypes := []string{"probe", "derive", "speech_gate", "transcribe", "align", "analyze", "index", "normalize"}
	for _, jt := range jobTypes {
		if !strings.Contains(progressHTML, jt+":'progress.jobType."+jt+"'") {
			t.Fatalf("jobTypeKeys missing pipeline job type %q", jt)
		}
	}
	issueCats := []string{"provider_quota", "provider_auth", "provider_unavailable", "provider_route_exhausted", "media_decode", "unsupported_media", "disk_space_low", "budget_exhausted", "source_missing", "worker_offline", "configuration", "unknown"}
	for _, cat := range issueCats {
		if !strings.Contains(progressHTML, "'"+cat+"':") {
			t.Fatalf("issueLabels missing issue category %q", cat)
		}
	}
	// The auto-recover map stays a boolean data map; only its rendering is
	// localized through the yes/no keys.
	for _, code := range []string{"provider_route_exhausted", "disk_space_low", "budget_exhausted"} {
		if !strings.Contains(progressHTML, "'"+code+"':true") {
			t.Fatalf("issueAutoRecover missing deferral code %q", code)
		}
	}
	if !strings.Contains(progressHTML, "tdT(issueAutoRecover[i.category]?'progress.yes':'progress.no')") {
		t.Fatal("issue auto-recover must render through progress.yes/progress.no keys, not raw booleans")
	}
}

func TestProgressPageI18nFragmentParityAcrossLocales(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("locales", "fragments", "progress.json"))
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
	if frag.Page != "progress" {
		t.Fatalf("fragment page=%q, want progress", frag.Page)
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
func TestProgressPageI18nServedWithoutUnresolvedMarkers(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "progress-page.db"))
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
	handler.ServeHTTP(response, lanRequest(http.MethodGet, "/progress", nil))
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
		`id="hero-job"`, `id="hero-meta"`, `id="supervisor"`, `id="pipeline-chain"`,
		`id="metrics"`, `id="issues"`, `id="issues-body"`, `id="jobs"`, `id="log"`,
		`id="run"`, `id="resume"`, `id="retry"`,
		`data-stage="probe"`, `data-stage="derive"`, `data-stage="speech_gate"`,
		`data-stage="transcribe"`, `data-stage="analyze"`, `data-stage="index"`,
		`data-action="retry-issue"`,
		`deferred_reason`,
		`/api/v1/jobs?limit=100`, `/api/v1/jobs/summary`, `/api/v1/issues`,
		`/api/v1/pipeline/run`, `/api/v1/pipeline/retry-failed`, `/api/v1/pipeline/resume-deferred`,
		`/api/v1/pipeline/supervisor`,
		`admin-token`, `loginAdmin`, `X-CSRF-Token`, `__Host-timingdex_csrf`,
		`tdApiErrorMessage`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("served /progress missing %q", want)
		}
	}
}
