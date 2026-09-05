package api

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

func throttleTestService(t *testing.T, name string, adminAuth ...string) *app.Service {
	t.Helper()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: secureTestDataDir(t), Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}}
	if len(adminAuth) > 0 {
		cfg.HubSecurity.AdminAuth = adminAuth[0]
	}
	service, err := app.NewService(repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// The settings page is the only place these limits are discoverable, so the
// throttle must be readable without a token from the LAN while changing it stays
// administrative.
func TestThrottleEndpointReadableWithoutTokenAndWritableOnlyByAdmin(t *testing.T) {
	service := throttleTestService(t, "throttle-api.db", "required")
	handler := NewServer("", service).Handler()

	read := httptest.NewRecorder()
	handler.ServeHTTP(read, lanRequest(http.MethodGet, "/api/v1/pipeline/throttle", nil))
	if read.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", read.Code, read.Body.String())
	}
	var initial struct {
		Throttle    domain.PipelineThrottle `json:"throttle"`
		OffPeakOpen bool                    `json:"off_peak_open"`
		ServerZone  string                  `json:"server_zone"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	// A fresh install must be unthrottled: turning this on by default would
	// silently slow every existing library after an upgrade.
	if initial.Throttle.ReadRate != 0 || initial.Throttle.CooldownSeconds != 0 || initial.Throttle.OffPeakEnabled {
		t.Fatalf("default must be unthrottled: %+v", initial.Throttle)
	}
	if !initial.OffPeakOpen {
		t.Fatal("a disabled window must report as open, or held work would look stranded")
	}
	// The window is judged in the Hub's zone, so the page has to be able to say
	// which zone that is.
	if !strings.HasPrefix(initial.ServerZone, "UTC") {
		t.Fatalf("server_zone=%q should name the Hub offset", initial.ServerZone)
	}

	body := `{"read_rate":2,"cooldown_seconds":15,"off_peak_enabled":true,"off_peak_start":"01:00","off_peak_end":"07:00","defer_above_bytes":524288000,"immediate_max_bytes":52428800}`
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, lanRequest(http.MethodPut, "/api/v1/pipeline/throttle", strings.NewReader(body)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("write without a token status=%d", unauthorized.Code)
	}

	saved := httptest.NewRecorder()
	handler.ServeHTTP(saved, hubAdminRequest(service, http.MethodPut, "/api/v1/pipeline/throttle", strings.NewReader(body)))
	if saved.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", saved.Code, saved.Body.String())
	}
	var stored struct {
		Throttle     domain.PipelineThrottle `json:"throttle"`
		HoldingAbove int64                   `json:"holding_above"`
		NextOffPeak  string                  `json:"next_off_peak"`
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Throttle.ReadRate != 2 || stored.Throttle.CooldownSeconds != 15 || stored.Throttle.DeferAboveBytes != 524288000 {
		t.Fatalf("save did not round-trip: %+v", stored.Throttle)
	}
	if stored.NextOffPeak == "" {
		t.Fatal("an enabled window must report when it next opens; the page shows it instead of leaving the queue looking stuck")
	}
}

// Every rejection must leave the stored value alone. A partially applied throttle
// would throttle at values the operator never chose.
func TestThrottleValidationRejectionsLeaveStoredValueIntact(t *testing.T) {
	service := throttleTestService(t, "throttle-reject.db")
	handler := NewServer("", service).Handler()

	good := `{"read_rate":2,"cooldown_seconds":10}`
	accepted := httptest.NewRecorder()
	handler.ServeHTTP(accepted, hubAdminRequest(service, http.MethodPut, "/api/v1/pipeline/throttle", strings.NewReader(good)))
	if accepted.Code != http.StatusOK {
		t.Fatalf("baseline save status=%d body=%s", accepted.Code, accepted.Body.String())
	}

	for name, payload := range map[string]string{
		"read rate below the floor":    `{"read_rate":0.1}`,
		"negative read rate":           `{"read_rate":-1}`,
		"cooldown beyond the limit":    `{"cooldown_seconds":9000}`,
		"exemption above the deferral": `{"immediate_max_bytes":900,"defer_above_bytes":100}`,
		"empty window":                 `{"off_peak_enabled":true,"off_peak_start":"03:00","off_peak_end":"03:00"}`,
		"malformed clock":              `{"off_peak_enabled":true,"off_peak_start":"25:99","off_peak_end":"07:00"}`,
		"not json":                     `{`,
		"negative deferral":            `{"defer_above_bytes":-5}`,
		"negative free space floor":    `{"minimum_free_space_bytes":-5}`,
		"negative daily cost guide":    `{"daily_cost_guide":-5}`,
		"negative monthly cost guide":  `{"monthly_cost_guide":-5}`,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPut, "/api/v1/pipeline/throttle", strings.NewReader(payload)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s (want 400)", name, response.Code, response.Body.String())
		}
	}

	after := httptest.NewRecorder()
	handler.ServeHTTP(after, lanRequest(http.MethodGet, "/api/v1/pipeline/throttle", nil))
	var stored struct {
		Throttle domain.PipelineThrottle `json:"throttle"`
	}
	if err := json.Unmarshal(after.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Throttle.ReadRate != 2 || stored.Throttle.CooldownSeconds != 10 {
		t.Fatalf("a rejected write changed the stored throttle: %+v", stored.Throttle)
	}
}

// The disk-space panel shares the throttle's save/load endpoints, so its
// value must survive an admin write and a tokenless read exactly like the
// load levers — and the page must render it back in GB.
func TestSettingsDiskSpaceProtectionRoundTripsInGB(t *testing.T) {
	service := throttleTestService(t, "settings-disk.db")
	handler := NewServer("", service).Handler()

	// 5 GB in bytes; the page converts with GB=1073741824 before sending.
	const fiveGB = int64(5) * 1073741824
	body := `{"minimum_free_space_bytes":` + fmt.Sprint(fiveGB) + `}`
	saved := httptest.NewRecorder()
	handler.ServeHTTP(saved, hubAdminRequest(service, http.MethodPut, "/api/v1/pipeline/throttle", strings.NewReader(body)))
	if saved.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", saved.Code, saved.Body.String())
	}
	var stored struct {
		Throttle domain.PipelineThrottle `json:"throttle"`
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Throttle.MinimumFreeSpaceBytes != fiveGB {
		t.Fatalf("save did not round-trip minimum_free_space_bytes: %+v", stored.Throttle)
	}

	read := httptest.NewRecorder()
	handler.ServeHTTP(read, lanRequest(http.MethodGet, "/api/v1/pipeline/throttle", nil))
	if read.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", read.Code, read.Body.String())
	}
	if err := json.Unmarshal(read.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Throttle.MinimumFreeSpaceBytes != fiveGB {
		t.Fatalf("the LAN read must show the persisted floor: %+v", stored.Throttle)
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/settings", nil))
	for _, marker := range []string{
		"settings.performance", // the panel that holds the field (marker resolves to the key pre-merge)
		"min-free-space",
		"minimum_free_space_bytes", // the field both directions carry
		"GB=1073741824",            // and the bytes conversion the page promises
	} {
		if !strings.Contains(page.Body.String(), marker) {
			t.Fatalf("settings page is missing %q", marker)
		}
	}
}

// The cost-guide panel shares the throttle's save/load endpoints, so its
// values must survive an admin write and a tokenless read exactly like the
// disk panel's — and the page must render both fields.
func TestSettingsCostGuidePanelRoundTrips(t *testing.T) {
	service := throttleTestService(t, "settings-cost-guide.db")
	handler := NewServer("", service).Handler()

	const daily, monthly = 25.5, 300
	body := fmt.Sprintf(`{"daily_cost_guide":%v,"monthly_cost_guide":%v}`, daily, monthly)
	saved := httptest.NewRecorder()
	handler.ServeHTTP(saved, hubAdminRequest(service, http.MethodPut, "/api/v1/pipeline/throttle", strings.NewReader(body)))
	if saved.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", saved.Code, saved.Body.String())
	}
	var stored struct {
		Throttle domain.PipelineThrottle `json:"throttle"`
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Throttle.DailyCostGuide != daily || stored.Throttle.MonthlyCostGuide != monthly {
		t.Fatalf("save did not round-trip the cost guides: %+v", stored.Throttle)
	}

	read := httptest.NewRecorder()
	handler.ServeHTTP(read, lanRequest(http.MethodGet, "/api/v1/pipeline/throttle", nil))
	if read.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", read.Code, read.Body.String())
	}
	if err := json.Unmarshal(read.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Throttle.DailyCostGuide != daily || stored.Throttle.MonthlyCostGuide != monthly {
		t.Fatalf("the LAN read must show the persisted cost guides: %+v", stored.Throttle)
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/settings", nil))
	for _, marker := range []string{
		"settings.cost", // the panel that holds the fields (marker resolves to the key pre-merge)
		"daily-cost-guide",
		"monthly-cost-guide",
		"daily_cost_guide", // the field both directions carry
		"monthly_cost_guide",
		"settings.costHint.lead", // guides never park work (marker resolves to the key pre-merge)
	} {
		if !strings.Contains(page.Body.String(), marker) {
			t.Fatalf("settings page is missing %q", marker)
		}
	}
}

func TestSettingsPageCarriesTokenPlumbingAndBothLevers(t *testing.T) {
	service := throttleTestService(t, "settings-page.db")
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	page := response.Body.String()
	for _, marker := range []string{
		"admin-token", // the token input
		"authHeaders", // and it is actually attached to requests
		"/api/v1/pipeline/throttle",
		"read-rate", "cooldown", "off-peak", "defer-mb", "immediate-mb",
		"server-time", "server-zone", // the window is judged in the Hub's zone
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("settings page is missing %q", marker)
		}
	}
	// The token must never be persisted by the browser; that promise is repeated
	// on every page in this project.
	if strings.Contains(page, "localStorage") || strings.Contains(page, "sessionStorage") {
		t.Fatal("settings page must keep the admin token in page memory only")
	}
}

// settingsFragmentKeys loads the page's fragment file and returns its zh-CN
// key set. The fragment is not merged into the embedded catalogs yet, so the
// page tests assert the key wiring against the fragment on disk rather than a
// resolved zh-CN value.
func settingsFragmentKeys(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("locales", "fragments", "settings.json"))
	if err != nil {
		t.Fatalf("read settings fragment: %v", err)
	}
	var frag struct {
		Keys map[string]map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(data, &frag); err != nil {
		t.Fatalf("parse settings fragment: %v", err)
	}
	keys := make(map[string]bool, len(frag.Keys[string(localeZhCN)]))
	for k := range frag.Keys[string(localeZhCN)] {
		keys[k] = true
	}
	return keys
}

func TestSettingsPageI18nHasNoHardcodedChinese(t *testing.T) {
	if hits := cjkRE.FindAllString(settingsHTML, -1); len(hits) > 0 {
		t.Fatalf("settingsHTML still carries hardcoded CJK copy: %q", hits)
	}
}

func TestSettingsPageI18nUsesSharedHelpers(t *testing.T) {
	if strings.Contains(settingsHTML, "apiErrMsg") {
		t.Fatal("page-local apiErrMsg must be deleted in favor of the shared tdApiErrorMessage")
	}
	if !strings.Contains(settingsHTML, "tdApiErrorMessage(") {
		t.Fatal("settings page must call the shared tdApiErrorMessage helper")
	}
	for _, legacy := range []string{"toLocaleTimeString", "toLocaleString("} {
		if strings.Contains(settingsHTML, legacy) {
			t.Fatalf("settings page still uses browser-locale formatter %q", legacy)
		}
	}
	if !strings.Contains(settingsHTML, "tdFormatNumber(") {
		t.Fatal("settings page must use the shared tdFormatNumber formatter for UI numbers")
	}
}

// Every static [[i18n:key]] marker and every literal key passed to
// tdT/tdPlural (including the storage-row label/note keys) must resolve: to a
// key the page fragment carries in all five locales, or to one of the shared
// catalog keys (common.*) that every locale already ships. Plural bases are
// resolved through their .one/.other siblings.
func TestSettingsPageI18nKeysResolveFromFragment(t *testing.T) {
	fragKeys := settingsFragmentKeys(t)

	markerRE := regexp.MustCompile(`\[\[i18n:([a-zA-Z0-9._-]+)\]\]`)
	callRE := regexp.MustCompile(`td(?:T|Plural)\('([a-zA-Z0-9._-]+)'`)
	keyLitRE := regexp.MustCompile(`'(settings|common)\.[a-zA-Z0-9._-]+'`)

	seen := make(map[string]bool)
	for _, m := range markerRE.FindAllStringSubmatch(settingsHTML, -1) {
		seen[m[1]] = true
	}
	for _, m := range callRE.FindAllStringSubmatch(settingsHTML, -1) {
		seen[m[1]] = true
	}
	for _, m := range keyLitRE.FindAllStringSubmatch(settingsHTML, -1) {
		seen[m[0][1:len(m[0])-1]] = true
	}

	if len(seen) == 0 {
		t.Fatal("no keys detected in settingsHTML; the scan is broken")
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
		t.Fatalf("settings keys that resolve to nothing (not in fragment, its plural siblings, or a shared catalog): %v", unresolved)
	}
}

func TestSettingsPageI18nFragmentParityAcrossLocales(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("locales", "fragments", "settings.json"))
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
	if frag.Page != "settings" {
		t.Fatalf("fragment page=%q, want settings", frag.Page)
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
// the server (to the bare key before the fragment is merged, to the zh-CN
// value after) rather than leaking as [[i18n:...]], and the structural
// anchors the tests and wire rely on must survive.
func TestSettingsPageI18nServedWithoutUnresolvedMarkers(t *testing.T) {
	service := throttleTestService(t, "settings-i18n-serve.db")
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	body := response.Body.String()
	if strings.Contains(body, "[[i18n:") {
		t.Fatalf("unresolved marker leaked into the served page: %s", body)
	}
	for _, want := range []string{
		`<html lang="zh-CN"`,
		`data-app-shell`,
		`id="read-rate"`, `id="cooldown"`, `id="off-peak"`, `id="off-start"`, `id="off-end"`,
		`id="defer-mb"`, `id="immediate-mb"`, `id="min-free-space"`,
		`id="daily-cost-guide"`, `id="monthly-cost-guide"`,
		`id="state"`, `id="storage-rows"`, `id="server-time"`, `id="server-zone"`,
		`id="save"`, `id="status"`,
		`id="sticky-save"`, `id="storage-bar"`, `id="storage-bar-fill"`,
		`/api/v1/pipeline/throttle`, `/api/v1/storage/overview`,
		`read_rate`, `cooldown_seconds`, `off_peak_start`, `off_peak_end`,
		`defer_above_bytes`, `immediate_max_bytes`, `minimum_free_space_bytes`,
		`daily_cost_guide`, `monthly_cost_guide`,
		`tdApiErrorMessage`, `tdFormatNumber`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("served /settings missing %q", want)
		}
	}
}

// The settings page regroups into four labelled panels (Storage, Performance,
// Schedule, Cost) with a sticky unsaved-changes bar, a storage-usage bar, and
// off-peak threshold rows that collapse until the schedule checkbox is on.
// The served page must carry the sticky bar hidden by default; the new group
// titles and bar copy are marker-driven in the source (the served page
// resolves them to the bare key before the fragment is merged), and the
// off-peak rows must ship hidden in the markup, not wait for JS.
func TestSettingsPageGroupsAndStickySave(t *testing.T) {
	service := throttleTestService(t, "settings-groups.db")
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	body := response.Body.String()
	for _, want := range []string{
		`<div id="sticky-save" class="sticky-save" hidden>`, // hidden until a change drifts from the snapshot
		`id="discard-btn"`, `id="save-changes-btn"`,
		`class="storage-bar"`, `id="storage-bar-fill"`, `id="storage-bar-pct"`,
		`<header class="pagehead">`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("served /settings missing %q", want)
		}
	}
	// Off-peak defaults unchecked, so its threshold rows start hidden: the
	// page ships the collapsed state in the markup rather than relying on JS.
	if !strings.Contains(body, `id="off-peak" type="checkbox">`) {
		t.Fatalf("off-peak checkbox must default unchecked")
	}
	if got := strings.Count(body, `class="form-row" hidden>`); got != 3 {
		t.Fatalf("off-peak threshold rows must start hidden, got %d hidden form-rows", got)
	}
	for _, marker := range []string{
		"[[i18n:settings.storage]]",
		"[[i18n:settings.performance]]",
		"[[i18n:settings.schedule]]",
		"[[i18n:settings.cost]]",
		"[[i18n:settings.unsavedChanges]]",
		"[[i18n:settings.saveChanges]]",
	} {
		if !strings.Contains(settingsHTML, marker) {
			t.Fatalf("settings source missing group marker %q", marker)
		}
	}
}

// The settings status line must be visible mutation feedback, not a
// display:none trap: say() renders the shared callout (confirmed/contradicted)
// on a role="status" aria-live region, and a fresh successful load clears it.
// The page-local .status{display:none} rule that hid every message is deleted.
func TestSettingsPageMutationFeedbackIsVisibleCallout(t *testing.T) {
	if strings.Contains(settingsHTML, `.status{display:none}`) {
		t.Fatal("settings page must not hide its status line")
	}
	for _, marker := range []string{
		`id="status" class="status" role="status" aria-live="polite"`,
		`function say(message,ok){const el=document.getElementById('status');el.className='callout '+(ok?'callout--confirmed':'callout--contradicted');el.textContent=message}`,
		`st.className='status';st.textContent=''`,
		`say(tdT('settings.saved'),true)`,
		`say(tdT('settings.saveError',{message:e.message}),false)`,
		`say(tdT('settings.loadError',{message:e.message}),false)`,
	} {
		if !strings.Contains(settingsHTML, marker) {
			t.Fatalf("settings page missing callout marker %q", marker)
		}
	}
}
