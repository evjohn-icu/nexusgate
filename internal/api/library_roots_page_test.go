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
	"time"

	"github.com/evjohn-icu/nexusslate/internal/app"
	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/media"
	"github.com/evjohn-icu/nexusslate/internal/mount"
	"github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
	"github.com/evjohn-icu/nexusslate/internal/smbdiscover"
)

// rootsCJKRE matches the CJK unified-ideograph and CJK-punctuation blocks.
// The page source must carry none of them: every piece of product copy now
// comes from the locale catalog, so a raw Chinese literal in libraryRootsHTML
// is a migration regression.
var rootsCJKRE = regexp.MustCompile(`[\x{3000}-\x{303f}\x{3400}-\x{4dbf}\x{4e00}-\x{9fff}\x{f900}-\x{faff}]`)

func newLibraryRootsTestService(t *testing.T, name string, adminAuth ...string) *app.Service {
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

// rootsFragment loads the page's fragment file and returns its per-locale
// key→value maps. The fragment is not merged into the embedded catalogs yet,
// so the page tests assert the key wiring against the fragment on disk rather
// than a resolved zh-CN value.
func rootsFragment(t *testing.T) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("locales", "fragments", "roots.json"))
	if err != nil {
		t.Fatalf("read roots fragment: %v", err)
	}
	var frag struct {
		Page string                       `json:"page"`
		Keys map[string]map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(data, &frag); err != nil {
		t.Fatalf("parse roots fragment: %v", err)
	}
	if frag.Page != "roots" {
		t.Fatalf("fragment page=%q, want roots", frag.Page)
	}
	return frag.Keys
}

// rootsHasCJK reports whether s contains a CJK unified ideograph -- the
// signal that a value is real translated copy and not a bare catalog key.
func rootsHasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// The wizard exists because /setup told operators to "add a mount directory
// on the progress page" and /progress never had such a form -- a promise the
// UI did not keep. This checks the page actually renders its four steps and,
// like every other admin page here, never persists the Hub token.
func TestLibraryRootsPageRendersWithStepMarkers(t *testing.T) {
	service := newLibraryRootsTestService(t, "library-roots-page.db")
	response := httptest.NewRecorder()
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/library-roots", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if ct := response.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type=%q", ct)
	}
	body := response.Body.String()
	// Every static step label is a [[i18n:roots.*]] marker in the source, and
	// the server resolves markers (to the zh-CN value once the fragment is
	// merged, to the bare key before that) rather than leaking [[i18n:...]].
	for _, marker := range []string{
		"[[i18n:roots.step1]]",
		"[[i18n:roots.step2]]",
		"[[i18n:roots.step3]]",
		"[[i18n:roots.step4]]",
		"[[i18n:roots.discoverTitle]]",
		"[[i18n:roots.healthTitle]]",
		"data-library-roots-wizard",
		"/api/v1/roots/inspect",
		"compose-section",
		"compose_volume",
		// SMB discovery panel: the page must advertise the discover button,
		// the endpoint it calls, and the click-a-share affordance.
		"discover-btn",
		"runDiscover()",
		"/api/v1/roots/discover",
		"useShare(",
	} {
		if !strings.Contains(libraryRootsHTML, marker) {
			t.Fatalf("page missing marker %q", marker)
		}
	}
	if strings.Contains(body, "[[i18n:") {
		t.Fatalf("unresolved marker leaked into the served page: %s", body)
	}
	// The shell injects the memory-only admin token input; the served page
	// must carry it (the shell is injected server-side, not in the constant).
	for _, shellMarker := range []string{"admin-token", `type="password"`} {
		if !strings.Contains(body, shellMarker) {
			t.Fatalf("served page missing shell marker %q", shellMarker)
		}
	}
	// The token must never be persisted by the browser; that promise is
	// repeated on every admin page in this project.
	if strings.Contains(body, "localStorage") || strings.Contains(body, "sessionStorage") {
		t.Fatal("library roots page must keep the admin token in page memory only")
	}
}

// The page source must be fully migrated: no hardcoded CJK copy, the shared
// tdApiErrorMessage instead of a page-local apiErrMsg, and no browser-locale
// number/date formatters.
func TestLibraryRootsPageI18nHasNoHardcodedChinese(t *testing.T) {
	if hits := rootsCJKRE.FindAllString(libraryRootsHTML, -1); len(hits) > 0 {
		t.Fatalf("libraryRootsHTML still carries hardcoded CJK copy: %q", hits)
	}
	if strings.Contains(libraryRootsHTML, "apiErrMsg") {
		t.Fatal("page-local apiErrMsg must be deleted in favor of the shared tdApiErrorMessage")
	}
	if !strings.Contains(libraryRootsHTML, "tdApiErrorMessage(") {
		t.Fatal("library roots page must call the shared tdApiErrorMessage helper")
	}
	for _, legacy := range []string{"toLocaleString(", "toLocaleTimeString", "toLocaleDateString"} {
		if strings.Contains(libraryRootsHTML, legacy) {
			t.Fatalf("library roots page must use the shared tdFormat* helpers, not %s", legacy)
		}
	}
}

// The wizard now opens with a status table for the roots that already exist:
// an operator coming to add a NAS share should first see whether the mounted
// roots are still mounted. The unavailable verdict carries the exact copy
// about the reconciliation pause — the scan service's gate, echoed here — and
// the preserved "last healthy" time MarkRootUnavailable never clears.
func TestLibraryRootsPageShowsRootHealthSection(t *testing.T) {
	body := brandedPage(shelledPage(libraryRootsHTML, localeZhCN))
	for _, marker := range []string{
		"[[i18n:roots.healthTitle]]",
		"[[i18n:roots.loadingHealth]]",
		"/api/v1/roots/health",
		"loadRootHealth()",
		`<table class="table">`,
		"tdT('roots.unavailablePause')",
		"tdT('roots.colLastHealthy')",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("library roots page missing root-health marker %q", marker)
		}
	}
	// The health copy is real translated text in the fragment, not a bare
	// key: the reconciliation-pause verdict and the last-healthy label must
	// read as Chinese in the zh-CN fragment.
	frag := rootsFragment(t)
	for _, key := range []string{"roots.healthTitle", "roots.healthIntro", "roots.unavailablePause", "roots.lastHealthy", "roots.colLastHealthy", "roots.colTips"} {
		if !rootsHasCJK(frag[string(localeZhCN)][key]) {
			t.Fatalf("roots fragment zh-CN %q does not look like translated copy: %q", key, frag[string(localeZhCN)][key])
		}
	}
}

// The root health table carries a 提示 column fed by the same warnings the
// CLI doctor prints — network-mount notice, staging-copy recommendation,
// writable-mount note — so an operator sees the advice for an already-added
// root without running the CLI. The cell must escape every warning and render
// one line each. warning_details (code/params/message) is rendered through
// the roots.warning.* catalog keys; warnings remains the fallback for older
// payloads, and an unknown code falls back to the English message.
func TestLibraryRootsPageRootHealthRendersWarnings(t *testing.T) {
	body := libraryRootsHTML
	for _, marker := range []string{
		`tdT('roots.colTips')`,
		`warning_details`,
		`rootWarningKeyPrefix`,
		`d.message`,
		`h.warnings`,
		`callout--attention`,
		`health-tips`,
		`esc(w)`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("library roots page missing root-health warnings marker %q", marker)
		}
	}
}

// CLAUDE.md requires product/UI copy to be Chinese while internal/mount
// stays English -- it is also the CLI's doctor output. This wizard bridges
// that by translating mount.Step/mount.Note by their stable Key through the
// roots.guide.* catalog keys. A step or note whose Key was added to mount but
// never given an entry in that catalog would silently render the raw English
// text inside an otherwise fully localized page, which is exactly the defect
// this whole feature exists to fix. Rather than hardcoding the key list
// here -- which would stop catching anything the day a new step or note is
// added -- this walks every host/share combination mount.Guidance can
// currently produce and derives the key set from it, so a newly introduced
// step or note without a translation fails this test immediately.
func TestLibraryRootsPageTranslatesEveryGuidanceKey(t *testing.T) {
	smb, ok := mount.ParseShare("//192.0.2.10/Video")
	if !ok {
		t.Fatal("smb fixture did not parse as a share")
	}
	nfs, ok := mount.ParseShare("192.0.2.10:/volume1/Video")
	if !ok {
		t.Fatal("nfs fixture did not parse as a share")
	}
	hosts := []mount.Host{
		{OS: "linux", UID: 1000, GID: 1000},
		{OS: "linux", UID: 1000, GID: 1000, WSL: true},
		{OS: "darwin"},
		{OS: "windows"},
	}
	keys := map[string]bool{}
	for _, share := range []mount.Share{smb, nfs} {
		for _, host := range hosts {
			guide := mount.Guidance(share, "", host)
			for _, step := range guide.Steps {
				keys[step.Key] = true
			}
			for _, note := range guide.Notes {
				keys[note.Key] = true
			}
		}
	}
	if len(keys) == 0 {
		t.Fatal("derived no keys from mount.Guidance -- this test is not exercising the page at all")
	}
	frag := rootsFragment(t)
	for key := range keys {
		if key == "" {
			t.Fatal("mount.Guidance produced an empty key; internal/mount's own test should have caught this first")
		}
		catKey := "roots.guide." + key
		for _, loc := range supportedLocales {
			if _, ok := frag[string(loc)][catKey]; !ok {
				t.Errorf("roots fragment locale %s is missing %q for guidance key %q", loc, catKey, key)
			}
		}
		if !rootsHasCJK(frag[string(localeZhCN)][catKey]) {
			t.Errorf("roots fragment zh-CN %q = %q does not look like a Chinese translation", catKey, frag[string(localeZhCN)][catKey])
		}
	}
	// The page must render these through the catalog, not a page-local table:
	// the old MOUNT_TR table is gone and guideText maps key → roots.guide.*.
	if !strings.Contains(libraryRootsHTML, "guideKeyPrefix='roots.guide.'") {
		t.Fatal("the page must render mount keys through the roots.guide.* catalog keys")
	}
}

// mount.ComposeVolume's WarningKey follows the exact same translation
// contract as mount.Step.Key/mount.Note.Key (see ComposeVolumeSuggestion in
// internal/app/service.go), but it is not reachable from mount.Guidance, so
// TestLibraryRootsPageTranslatesEveryGuidanceKey above -- which only walks
// Guidance's steps and notes -- cannot see it. A key introduced on the
// compose side without an entry here would silently render English inside
// the danger box a password warning belongs in, which is exactly the
// scenario the whole feature exists to avoid.
func TestLibraryRootsPageTranslatesComposeWarningKey(t *testing.T) {
	smb, ok := mount.ParseShare("//192.0.2.10/Video")
	if !ok {
		t.Fatal("smb fixture did not parse as a share")
	}
	volume, ok := mount.ComposeVolume(smb, "nas-video")
	if !ok {
		t.Fatal("an SMB share must produce a compose volume definition")
	}
	if volume.WarningKey == "" {
		t.Fatal("the SMB compose form must carry a WarningKey; internal/mount's own test should have caught this first")
	}
	frag := rootsFragment(t)
	catKey := "roots.guide." + volume.WarningKey
	for _, loc := range supportedLocales {
		if _, ok := frag[string(loc)][catKey]; !ok {
			t.Fatalf("roots fragment locale %s is missing %q for compose warning key %q", loc, catKey, volume.WarningKey)
		}
	}
	if !rootsHasCJK(frag[string(localeZhCN)][catKey]) {
		t.Errorf("roots fragment zh-CN %q = %q does not look like a Chinese translation", catKey, frag[string(localeZhCN)][catKey])
	}
}

// Every static [[i18n:key]] marker and literal tdT/tdPlural key on the page
// must resolve: to a key the roots fragment carries in all five locales, or
// to one of the shared catalog keys (common.* / status.* / progress.title)
// that every locale already ships. Plural bases are resolved through their
// .one/.other siblings.
func TestLibraryRootsPageI18nKeysResolveFromFragment(t *testing.T) {
	frag := rootsFragment(t)
	fragKeys := frag[string(localeZhCN)]
	shared := map[string]bool{}
	for _, loc := range supportedLocales {
		for k := range catalogs[loc] {
			shared[k] = true
		}
	}
	unresolved := map[string]bool{}
	collect := func(key string) {
		if fragKeys[key] != "" || shared[key] {
			return
		}
		if fragKeys[key+".one"] != "" || fragKeys[key+".other"] != "" || shared[key+".one"] || shared[key+".other"] {
			return
		}
		unresolved[key] = true
	}
	for _, m := range i18nMarkerRE.FindAllStringSubmatch(libraryRootsHTML, -1) {
		collect(m[1])
	}
	for _, m := range tdKeyCallRE.FindAllStringSubmatch(libraryRootsHTML, -1) {
		collect(m[1])
	}
	if len(unresolved) > 0 {
		names := make([]string, 0, len(unresolved))
		for k := range unresolved {
			names = append(names, k)
		}
		sort.Strings(names)
		t.Fatalf("roots keys that resolve to nothing (not in fragment, its plural siblings, or a shared catalog): %v", names)
	}
}

// The fragment must be a valid UTF-8 JSON catalog fragment: the page name
// matches, all five locales carry the identical key set, and every key's
// {placeholder} set matches zh-CN. It must define only page-prefixed keys.
func TestLibraryRootsPageI18nFragmentParityAcrossLocales(t *testing.T) {
	frag := rootsFragment(t)
	base := frag[string(localeZhCN)]
	if len(base) == 0 {
		t.Fatal("zh-CN fragment is empty")
	}
	for _, loc := range supportedLocales {
		km := frag[string(loc)]
		if len(km) != len(base) {
			t.Fatalf("locale %s has %d keys, zh-CN has %d", loc, len(km), len(base))
		}
		for k := range base {
			if _, ok := km[k]; !ok {
				t.Fatalf("locale %s is missing key %q", loc, k)
			}
		}
	}
	phRE := regexp.MustCompile(`\{[a-z]+\}`)
	phSet := func(s string) string {
		parts := phRE.FindAllString(s, -1)
		sort.Strings(parts)
		return strings.Join(parts, ",")
	}
	for k, zh := range base {
		want := phSet(zh)
		for _, loc := range supportedLocales[1:] {
			if got := phSet(frag[string(loc)][k]); got != want {
				t.Fatalf("placeholder set differs for %s in %s: %q vs zh-CN %q", k, loc, frag[string(loc)][k], zh)
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

// An operator who has already copied the YAML block has already leaked the
// password it would have warned about; the warning is only useful read
// first. This checks the client-side rendering code itself puts the danger
// box before the copyable YAML block in renderComposeVolume, not after.
func TestLibraryRootsPageRendersComposeWarningAboveYAML(t *testing.T) {
	start := strings.Index(libraryRootsHTML, "function renderComposeVolume")
	if start < 0 {
		t.Fatal("renderComposeVolume is not defined on the page")
	}
	end := strings.Index(libraryRootsHTML[start:], "\n}")
	if end < 0 {
		t.Fatal("could not find the end of renderComposeVolume")
	}
	body := libraryRootsHTML[start : start+end]
	warningIdx := strings.Index(body, "callout--contradicted")
	yamlIdx := strings.Index(body, "compose-yaml-code")
	if warningIdx < 0 {
		t.Fatal("renderComposeVolume never renders the warning box")
	}
	if yamlIdx < 0 {
		t.Fatal("renderComposeVolume never renders the YAML block")
	}
	if warningIdx > yamlIdx {
		t.Fatal("the warning must be emitted before the YAML block, not after — see the danger box's placement rationale")
	}
}

// The decision doc's actual recommendation is to avoid the cleartext-password
// problem entirely by using NFS instead of SMB when the NAS supports it, not
// merely to disclose the caveat and move on.
func TestLibraryRootsPageRecommendsNFSOverSMBForCompose(t *testing.T) {
	start := strings.Index(libraryRootsHTML, "function renderComposeVolume")
	if start < 0 {
		t.Fatal("renderComposeVolume is not defined on the page")
	}
	end := strings.Index(libraryRootsHTML[start:], "\n}")
	body := libraryRootsHTML[start : start+end]
	if !strings.Contains(body, "tdT('roots.composeNfsRecommended')") {
		t.Fatal("the SMB branch must recommend NFS through the roots.composeNfsRecommended catalog key as the way to avoid the cleartext-credentials problem entirely")
	}
}

// This is the important one: /api/v1/roots/inspect answers os.Stat and mount
// table questions about an arbitrary path on the Hub's own filesystem. If it
// were reachable without the admin token, any device on the LAN could probe
// which paths exist on the Hub.
func TestInspectRootRejectsUnauthenticatedRequest(t *testing.T) {
	service := newLibraryRootsTestService(t, "inspect-auth.db", "required")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodPost, "/api/v1/roots/inspect", strings.NewReader(`{"path":"/tmp"}`)))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401 — an unauthenticated LAN caller must not be able to probe Hub filesystem paths", response.Code, response.Body.String())
	}
}

// A share address like //nas.local/Video should come back with the exact
// mount commands (mount.Guidance), not just "not found" — that is the whole
// point of the endpoint over a bare os.Stat.
func TestInspectRootShareInputReturnsGuidance(t *testing.T) {
	service := newLibraryRootsTestService(t, "inspect-share.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/inspect", strings.NewReader(`{"path":"//nas.local/Video"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var inspection app.RootInspection
	if err := json.Unmarshal(response.Body.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	if !inspection.IsShare {
		t.Fatalf("//nas.local/Video should parse as a share: %+v", inspection)
	}
	if inspection.Share == nil || inspection.Share.Host != "nas.local" || inspection.Share.Name != "Video" {
		t.Fatalf("share=%+v", inspection.Share)
	}
	if inspection.Guidance == nil || inspection.Guidance.Summary == "" || len(inspection.Guidance.Steps) == 0 {
		t.Fatalf("guidance missing or empty: %+v", inspection.Guidance)
	}
	if inspection.DefaultMountpoint == "" {
		t.Fatal("default_mountpoint must be prefilled so the wizard's step 2 has something to show before the operator edits it")
	}
}

// Dropping the password out of Share.User is not enough on its own: this
// response is rendered straight into the page that asked for it, so anything
// in it that still echoes what was typed puts the password back in the DOM.
// A browser address bar accepts smb://user:password@host/share, which is
// exactly the string an operator is liable to paste. The whole response body
// is checked rather than one field, because the leak this covers was in
// `path` — a field nobody thought of as carrying a credential.
func TestInspectRootNeverEchoesAPastedPassword(t *testing.T) {
	service := newLibraryRootsTestService(t, "inspect-password.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/inspect", strings.NewReader(`{"path":"smb://ev:hunter2@nas.local/Video"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "hunter2") {
		t.Fatalf("the pasted password reached the response body: %s", response.Body.String())
	}
	var inspection app.RootInspection
	if err := json.Unmarshal(response.Body.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	// The username is still useful — it prefills the credentials file — so
	// this must not have been achieved by discarding the userinfo wholesale.
	if inspection.Share == nil || inspection.Share.User != "ev" {
		t.Fatalf("the username must survive so the generated commands can use it: %+v", inspection.Share)
	}
}

// This test machine is never itself the containerised Hub the compose_volume
// suggestion is for (see app.Service.hostOverride's doc), and this package
// cannot reach that unexported field to fake it — only internal/app can, in
// TestInspectRootPathComposeVolumeNeverEchoesAPastedPassword, which is the
// version of this check that actually exercises a populated compose_volume.
// This one only guards the ordinary path/share fields over a real HTTP
// response, kept here as a regression guard alongside the other
// password-leak tests in this file.
func TestInspectRootShareResponseNeverEchoesAPastedPasswordOverHTTP(t *testing.T) {
	service := newLibraryRootsTestService(t, "inspect-password-http.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/inspect", strings.NewReader(`{"path":"smb://ev:hunter2@192.0.2.10/Video"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "hunter2") {
		t.Fatalf("the pasted password reached the response body: %s", response.Body.String())
	}
}

// The same leak reaches an API caller that never opens the wizard, through
// the 422 body createRoot answers an unmounted share with.
func TestCreateRootNeverEchoesAPastedPassword(t *testing.T) {
	service := newLibraryRootsTestService(t, "create-password.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots", strings.NewReader(`{"path":"smb://ev:hunter2@nas.local/Video"}`)))
	if strings.Contains(response.Body.String(), "hunter2") {
		t.Fatalf("the pasted password reached the response body: %s", response.Body.String())
	}
}

// A plain, already-accessible directory is the common case and must not be
// treated as a share (no guidance) or flagged with warnings that only apply
// to network mounts.
func TestInspectRootPlainDirectoryReturnsNoGuidanceOrWarnings(t *testing.T) {
	service := newLibraryRootsTestService(t, "inspect-plain-dir.db")
	handler := NewServer("", service).Handler()
	dir := t.TempDir()
	payload, err := json.Marshal(map[string]string{"path": dir})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/inspect", strings.NewReader(string(payload))))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var inspection app.RootInspection
	if err := json.Unmarshal(response.Body.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.IsShare || inspection.Share != nil || inspection.Guidance != nil {
		t.Fatalf("a plain local directory must not be treated as a share: %+v", inspection)
	}
	if !inspection.Exists || !inspection.IsDir {
		t.Fatalf("an existing temp directory should report exists=true, is_dir=true: %+v", inspection)
	}
	if len(inspection.Warnings) != 0 {
		t.Fatalf("a plain local temp directory should carry no warnings, got %v", inspection.Warnings)
	}
}

// A typo'd or not-yet-created path must be reported, not turned into a 500 —
// the wizard's step 3 relies on this to tell the operator "not there yet,
// try again" rather than showing a crash.
func TestInspectRootNonExistentPathReportsInsteadOfFailing(t *testing.T) {
	service := newLibraryRootsTestService(t, "inspect-missing.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/inspect", strings.NewReader(`{"path":"/definitely/does/not/exist/on/this/machine-xyz"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 — a missing path is reported, not a server error", response.Code, response.Body.String())
	}
	var inspection app.RootInspection
	if err := json.Unmarshal(response.Body.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.Exists {
		t.Fatalf("a path that does not exist must report exists=false: %+v", inspection)
	}
}

// Before this change, createRoot's ErrShareNotMounted handling fell through to
// writeError, which always answers 500 with just err.Error() as plain text.
// An API caller (or the wizard, on an unlucky race where the share unmounts
// between verify and add) needs to tell "share not mounted" apart from any
// other failure and get the guidance back, not a generic error string.
func TestCreateRootWithUnmountedShareReturnsShareSpecificResponse(t *testing.T) {
	service := newLibraryRootsTestService(t, "create-root-unmounted-share.db")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots", strings.NewReader(`{"path":"//nas.local/Video"}`)))
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422 — an unmounted share must not look like the same generic failure as a bad local path", response.Code, response.Body.String())
	}
	var payload struct {
		Error      APIError           `json:"error"`
		Inspection app.RootInspection `json:"inspection"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Message == "" {
		t.Fatal("response must carry an error message")
	}
	if !payload.Inspection.IsShare || payload.Inspection.Guidance == nil {
		t.Fatalf("response must carry the mount guidance, so an API caller that never opens /library-roots is still told what to do: %+v", payload.Inspection)
	}
}

// The discover endpoint is an active LAN-wide network scan, so it must be
// Hub-admin gated like every other root-mutating route: an unauthenticated
// caller must not be able to trigger it.
func TestDiscoverRootsRejectsUnauthenticatedRequest(t *testing.T) {
	service := newLibraryRootsTestService(t, "discover-auth.db", "required")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, lanRequest(http.MethodPost, "/api/v1/roots/discover", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401 — an unauthenticated caller must not trigger a LAN scan", response.Code, response.Body.String())
	}
}

// With the Hub admin token the discover endpoint answers 200 with a hosts
// array. A fake discover function is injected so the test never triggers a
// real 15-second LAN scan; it returns one host with a share to prove the
// handler passes the discovery result through.
func TestDiscoverRootsAuthorizedReturnsHostsArray(t *testing.T) {
	service := newLibraryRootsTestService(t, "discover-ok.db", "required")
	// Inject a fast fake via the exported setter so the test never triggers a
	// real LAN scan.
	service.SetSMBDiscoverer(func(context.Context) ([]smbdiscover.Host, error) {
		return []smbdiscover.Host{
			{Name: "nas.local", IP: "192.168.1.50", Shares: []string{"video", "photos"}, Source: "mdns"},
		}, nil
	})
	handler := NewServer("", service).Handler()
	request := lanRequest(http.MethodPost, "/api/v1/roots/discover", nil)
	request.Header.Set("Authorization", "Bearer "+service.AdminToken())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	var payload struct {
		Hosts []smbdiscover.Host `json:"hosts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Hosts == nil {
		t.Fatal("hosts must be an empty array, not null")
	}
	if len(payload.Hosts) != 1 || payload.Hosts[0].Name != "nas.local" || len(payload.Hosts[0].Shares) != 2 {
		t.Fatalf("hosts = %+v, want the injected nas.local with 2 shares", payload.Hosts)
	}
}

// The scan result panel must tell an operator why a root produced no footage:
// the skipped-file census (bounded extension names plus a folded count) and
// the supported extension list render through catalog keys, and a zero-
// discovery scan with skipped files renders as a warning callout rather than a
// green success.
func TestLibraryRootsPageScanResultShowsSkippedAndSupported(t *testing.T) {
	body := libraryRootsHTML
	for _, marker := range []string{
		`result.skipped_files`,
		`result.skipped_extensions`,
		`result.skipped_other`,
		`result.supported_extensions`,
		`var noFootage=result.discovered===0&&skipped>0`,
		`tdPlural('roots.scanSkipped'`,
		`tdT('roots.scanSkippedMore'`,
		`tdT('roots.scanSupported'`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("library-roots page missing scan marker %q", marker)
		}
	}
	if !strings.Contains(body, `noFootage?'callout callout--attention':'callout callout--confirmed'`) {
		t.Fatalf("zero-discovery scan with skipped files must render a warning callout")
	}
}

// POST /roots/{id}/scan reports the skipped-file census and the supported
// extension list alongside discovered/linked/missing, so the page and the CLI
// can explain a root that produced no footage.
func TestScanRootReportsSkippedFilesAndSupportedExtensions(t *testing.T) {
	service := newLibraryRootsTestService(t, "scan-skipped.db", "required")
	ctx := context.Background()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "clip.mp4"), []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "clip.mkv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "subs.srt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := service.AddLibraryRoot(ctx, rootPath)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/"+root.ID+"/scan", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("scan status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Discovered          int      `json:"discovered"`
		SkippedFiles        int      `json:"skipped_files"`
		SkippedExtensions   []string `json:"skipped_extensions"`
		SkippedOther        int      `json:"skipped_other"`
		SupportedExtensions []string `json:"supported_extensions"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Discovered != 1 {
		t.Fatalf("Discovered = %d, want 1", body.Discovered)
	}
	if body.SkippedFiles != 2 {
		t.Fatalf("SkippedFiles = %d, want 2", body.SkippedFiles)
	}
	named := strings.Join(body.SkippedExtensions, ",")
	if !strings.Contains(named, ".mkv") || !strings.Contains(named, ".srt") {
		t.Fatalf("SkippedExtensions = %v, want .mkv and .srt named", body.SkippedExtensions)
	}
	if !strings.Contains(strings.Join(body.SupportedExtensions, ","), ".mp4") {
		t.Fatalf("SupportedExtensions = %v, want .mp4 listed", body.SupportedExtensions)
	}
	// Drain the background pipeline the scan triggered so it cannot outlive
	// the test.
	deadline := time.Now().Add(5 * time.Second)
	for service.PipelineRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}
