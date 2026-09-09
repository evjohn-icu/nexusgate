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

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/mount"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
	"github.com/evjohn-icu/nexusgate/internal/smbdiscover"
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

func TestLibraryRootsPageUsesShareNameGuidanceAction(t *testing.T) {
	body := libraryRootsHTML
	for _, marker := range []string{
		"function guideActionKey(inspection)",
		"code==='root.share_name_unsupported'",
		"return {key:'roots.shareNameUnsupportedAction'}",
		"{key:'roots.shareNameUnsupportedCharacterAction',vars:detail.params||{}}",
		"tdT(action.key,action.vars)",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("library roots page missing share-name guidance marker %q", marker)
		}
	}
	if got := strings.Count(body, "renderGuideBody(inspection.guidance, guideActionKey(inspection))"); got != 2 {
		t.Fatalf("renderGuideBody must receive the inspection action key at both call sites, got %d", got)
	}
}

// The root health table carries a 提示 column fed by the same warnings the
// CLI doctor prints — network-mount notice, staging-copy recommendation,
// writable-mount note — so an operator sees the advice for an already-added
// root without running the CLI. The cell must escape every warning and render
// one line each. warning_details (code/params/message) is rendered through
// the roots.warning.* catalog keys; warnings remains the fallback for older
// payloads, and an unknown code falls back to the English message.
//
// rootWarningsHTML now takes the object rather than closing over a health
// row, because the mount wizard renders the same two shapes and had drifted
// into printing the API's raw English. So the marker below is source.warnings
// rather than h.warnings: what needs pinning is that the flat-array fallback
// branch still exists, and the parameter name is the only thing that moved.
func TestLibraryRootsPageRootHealthRendersWarnings(t *testing.T) {
	body := libraryRootsHTML
	for _, marker := range []string{
		`tdT('roots.colTips')`,
		`warning_details`,
		`rootWarningKeyPrefix`,
		`d.message`,
		`(source.warnings||[])`,
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
	// The matrix has to cover every switch Guidance branches on, not just OS.
	// It listed four OS shapes and nothing behind Container, Service or
	// Platform, so an entire branch could ship with no translation at all and
	// this guard would stay green — which is exactly what happened to the
	// Unraid steps.
	hosts := []mount.Host{
		{OS: "linux", UID: 1000, GID: 1000},
		{OS: "linux", UID: 1000, GID: 1000, WSL: true},
		{OS: "linux", UID: 1000, GID: 1000, Container: true, MediaBind: mount.Bind{Target: "/media/library", Source: "/mnt/remotes"}},
		{OS: "darwin"},
		{OS: "darwin", Service: true},
		{OS: "windows"},
		{OS: "windows", Service: true},
		{OS: "linux", UID: 1000, GID: 1000, Container: true, Platform: "unraid", MediaBind: mount.Bind{Target: "/media/library", Source: "/mnt/remotes"}},
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
	service.SetSMBDiscoverer(func(context.Context) (smbdiscover.Result, error) {
		return smbdiscover.Result{
			Hosts:           []smbdiscover.Host{{Name: "nas.local", IP: "192.168.1.50", Shares: []string{"video", "photos"}, Source: "mdns"}},
			ScannedNetworks: []string{"192.168.1.0/24"},
			SkippedNetworks: []string{"10.0.0.0/8"},
			MDNSAvailable:   true,
			Truncated:       true,
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
		Hosts           []smbdiscover.Host `json:"hosts"`
		ScannedNetworks []string           `json:"scanned_networks"`
		SkippedNetworks []string           `json:"skipped_networks"`
		MDNSAvailable   bool               `json:"mdns_available"`
		Truncated       bool               `json:"truncated"`
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
	if len(payload.ScannedNetworks) != 1 || len(payload.SkippedNetworks) != 1 || !payload.MDNSAvailable || !payload.Truncated {
		t.Fatalf("diagnostics = %+v, want flattened discovery diagnostics", payload)
	}
}

// hub_wsl must be wired to app.Service.HubWSL(), which reports
// mount.LocalHost().WSL rather than a hardcoded value — a Hub under WSL's
// default NAT networking sweeps its own private vEthernet subnet and finds
// nothing there no matter how the scan itself behaves, so the browser needs
// this fact independently of the scan result. Compared against
// mount.LocalHost().WSL directly (not a hardcoded literal) so the assertion
// holds whatever machine runs the test, but still catches the handler
// dropping the field, wiring the wrong Service method, or wiring
// HubContainerised's value in its place.
func TestDiscoverRootsReportsHubWSL(t *testing.T) {
	service := newLibraryRootsTestService(t, "discover-wsl.db", "required")
	service.SetSMBDiscoverer(func(context.Context) (smbdiscover.Result, error) {
		return smbdiscover.Result{}, nil
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
		HubWSL bool `json:"hub_wsl"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if want := mount.LocalHost().WSL; payload.HubWSL != want {
		t.Fatalf("hub_wsl=%v, want %v (mount.LocalHost().WSL)", payload.HubWSL, want)
	}
}

func TestDiscoverRootsReturnsConflictWhileScanning(t *testing.T) {
	service := newLibraryRootsTestService(t, "discover-conflict.db", "required")
	started := make(chan struct{})
	release := make(chan struct{})
	service.SetSMBDiscoverer(func(context.Context) (smbdiscover.Result, error) {
		close(started)
		<-release
		return smbdiscover.Result{}, nil
	})
	handler := NewServer("", service).Handler()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/discover", nil))
		if response.Code != http.StatusOK {
			t.Errorf("first request status=%d body=%s, want 200", response.Code, response.Body.String())
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first discovery did not start")
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, hubAdminRequest(service, http.MethodPost, "/api/v1/roots/discover", nil))
	if second.Code != http.StatusConflict {
		t.Fatalf("second request status=%d body=%s, want 409", second.Code, second.Body.String())
	}

	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first discovery did not finish after release")
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

// Share names come from whatever SMB server answered on the LAN, so they must
// never reach the page as inline handler source. An onclick built by string
// concatenation is parsed twice — the HTML parser decodes character references
// in the attribute value before the JS parser sees it — so escaping a quote as
// &#39; there is not protection: it arrives at the JS parser as a quote and
// ends the string literal. This guard exists because that is exactly what the
// discover panel used to do.
func TestLibraryRootsPageDiscoverCarriesNoInlineShareHandlers(t *testing.T) {
	body := libraryRootsHTML
	// Positive control: the region this test is about still exists, so an
	// absent-substring assertion below cannot pass by searching a page that no
	// longer renders share buttons at all.
	if !strings.Contains(body, "discover-share") {
		t.Fatal("discover-share is gone from the page — this guard is searching the wrong region")
	}
	for _, forbidden := range []string{`onclick="useShare(`, `onclick="useManualShare(`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("page still builds an inline handler from discovery data: %s", forbidden)
		}
	}
	for _, required := range []string{"data-share-host", "data-share-name", "addEventListener('click'"} {
		if !strings.Contains(body, required) {
			t.Errorf("page is missing %q — discovery data must travel as data-* read back by a delegated listener", required)
		}
	}
}

// The discover panel is a child of step 1, not a sibling of the step
// container. As a sibling it stayed visible through steps 2-4, where a click
// on a share silently reset the wizard to step 1 and discarded the mount point
// the operator had just entered. .step{display:none} only retires it while it
// is nested, so a future edit that lifts it back out is a silent regression.
func TestLibraryRootsPageDiscoverSectionLivesInsideStepOne(t *testing.T) {
	body := libraryRootsHTML
	stepOne := strings.Index(body, `id="step-1"`)
	discover := strings.Index(body, `id="discover-section"`)
	stepTwo := strings.Index(body, `id="step-2"`)
	if stepOne < 0 || discover < 0 || stepTwo < 0 {
		t.Fatalf("anchors missing: step-1=%d discover-section=%d step-2=%d", stepOne, discover, stepTwo)
	}
	if !(stepOne < discover && discover < stepTwo) {
		t.Fatalf("discover-section at %d must sit between step-1 (%d) and step-2 (%d)", discover, stepOne, stepTwo)
	}
}

// Consumer NAS boxes ship with guest access disabled, so "auth" is the answer
// the anonymous probe usually gets, not the edge case. Every probe state
// therefore has to leave the operator something to act on: a card with no
// share list and no input is a dead end, because the next wizard step needs a
// share name and enumeration is what failed to produce one. "unusable" must
// also stay distinct from "auth" — promising that a password helps against an
// SMB1-only box blames the operator for something a password cannot fix.
func TestLibraryRootsPageRendersEveryProbeState(t *testing.T) {
	body := libraryRootsHTML
	for _, key := range []string{
		"roots.probe.ok.hint",
		"roots.probe.auth.title", "roots.probe.auth.hint",
		"roots.probe.unusable.title", "roots.probe.unusable.hint",
		"roots.probe.timeout.title", "roots.probe.timeout.hint",
		"roots.manualShareLabel", "roots.manualShareUse", "roots.shareNameRequired",
	} {
		if !strings.Contains(body, key) {
			t.Errorf("page never renders %s", key)
		}
	}
	// The manual share entry is appended for every state rather than inside
	// one of the probe branches; if it moves into a branch this stops holding.
	discoverFn := body[strings.Index(body, "async function runDiscover()"):]
	discoverFn = discoverFn[:strings.Index(discoverFn, "\nfunction escAttr")]
	branch := strings.Index(discoverFn, "probe==='auth'")
	manual := strings.Index(discoverFn, "discover-manual-go")
	if branch < 0 || manual < 0 {
		t.Fatalf("runDiscover no longer contains the probe branch (%d) or the manual entry (%d)", branch, manual)
	}
	if manual < branch {
		t.Fatal("the manual share entry must be appended after the probe branches, so every state carries it")
	}
}

// A file-line step's commands are a line to append to a file, not a line to
// run. Rendered identically to every other command block, an /etc/fstab entry
// reads as a command, and pasting it into a terminal is what an operator
// following the wizard actually does.
func TestLibraryRootsPageSeparatesFileLinesFromCommands(t *testing.T) {
	body := libraryRootsHTML
	if !strings.Contains(body, "renderGuideBody") {
		t.Fatal("renderGuideBody is gone — this guard is searching the wrong region")
	}
	if !strings.Contains(body, "step.kind==='file-line'") {
		t.Error("renderGuideBody does not branch on step.kind, so an fstab line still renders as a runnable command")
	}
	if !strings.Contains(body, "roots.fileLineNote") {
		t.Error("the file-line note is never rendered")
	}
}

// An empty host list has at least five different causes and only one of them
// is "your LAN has no SMB server". Reporting that one sentence for all five
// sends operators looking for a fault that is not there — most importantly
// when the Hub is a bridge-networked container that swept Docker's own
// subnet, or a Hub under WSL's default NAT networking that swept its own
// private vEthernet subnet.
func TestLibraryRootsPageDiagnosesEmptyDiscoveryResults(t *testing.T) {
	body := libraryRootsHTML
	if !strings.Contains(body, "function discoverDiagnostics") {
		t.Fatal("discoverDiagnostics is missing")
	}
	for _, signal := range []string{
		"hub_containerised", "hub_wsl", "mdns_available", "scanned_networks", "skipped_networks", "truncated",
	} {
		if !strings.Contains(body, signal) {
			t.Errorf("discovery diagnostics ignore the %s signal the API now returns", signal)
		}
	}
	for _, key := range []string{
		"roots.diag.containerBridge", "roots.diag.wslNat", "roots.diag.noNetworks", "roots.diag.noHosts",
		"roots.diag.skipped", "roots.diag.truncated", "roots.diag.allUnusable", "roots.diag.noMulticast",
	} {
		if !strings.Contains(body, key) {
			t.Errorf("page never renders %s", key)
		}
	}
}

// hub_wsl must be checked before the truncated gate, not after it. If the
// subnet is wider than /24 — WSL's default NAT vEthernet often is; a /20 has
// 4094 addresses — the port scan's budget only reaches roughly 512
// addresses, so it always truncates before finishing. If the wsl branch were
// gated behind "not truncated" like the plain noHosts branch, a WSL Hub
// would see roots.diag.truncated ("a device may have been missed") instead
// of the NAT explanation — which implies the NAS is on this subnet and the
// scan simply did not reach it, when in fact this subnet is not the
// operator's LAN at all and no amount of sweeping it will find the NAS.
func TestLibraryRootsPageWSLDiagnosisPrecedesTruncatedGate(t *testing.T) {
	body := libraryRootsHTML
	start := strings.Index(body, "function discoverDiagnostics")
	if start < 0 {
		t.Fatal("discoverDiagnostics is missing")
	}
	fn := body[start:]
	end := strings.Index(fn, "\nfunction ")
	if end < 0 {
		t.Fatal("could not find the end of discoverDiagnostics")
	}
	fn = fn[:end]
	wslBranch := strings.Index(fn, "data.hub_wsl")
	truncatedGate := strings.Index(fn, "!data.truncated")
	if wslBranch < 0 || truncatedGate < 0 {
		t.Fatalf("hub_wsl branch (%d) or the truncated gate (%d) not found in discoverDiagnostics", wslBranch, truncatedGate)
	}
	if wslBranch > truncatedGate {
		t.Fatal("hub_wsl branch is gated behind the truncated check — a WSL Hub whose NAT subnet scan truncates (which a /20 subnet always does inside the port budget) would see the generic truncated message instead of the WSL explanation")
	}
}

// The copy button used to swallow every failure in a bare catch and say
// nothing on success either, so a dead button and a copied command looked
// identical — and execCommand is deprecated, which makes "nothing happened" a
// real outcome rather than a hypothetical one.
func TestLibraryRootsPageCopyButtonReportsOutcome(t *testing.T) {
	body := libraryRootsHTML
	if !strings.Contains(body, "navigator.clipboard") {
		t.Error("copyCmd never tries the non-deprecated clipboard API")
	}
	for _, key := range []string{"roots.copied", "roots.copyFailed"} {
		if !strings.Contains(body, key) {
			t.Errorf("copyCmd never reports %s", key)
		}
	}
	// Scoped to the copy helpers on purpose: addRoot's bare catch around a
	// non-JSON error body is deliberate and falls back to statusText, so a
	// page-wide search would fail on code that is already correct.
	start := strings.Index(body, "function legacyCopy(")
	if start < 0 {
		t.Fatal("legacyCopy is missing — this guard is searching the wrong region")
	}
	legacy := body[start:]
	if end := strings.Index(legacy, "\nfunction "); end > 0 {
		legacy = legacy[:end]
	}
	if strings.Contains(legacy, "catch(e){}") {
		t.Error("a bare catch is back in the copy path: a swallowed failure is indistinguishable from success")
	}
}

// The Hub cannot see which platform owns mounts on its Docker host, nor
// whether it runs under a service manager, and no path is proof of either.
// The wizard therefore asks — and must actually send the answer, or the whole
// Unraid branch is unreachable no matter what the mount package can generate.
func TestLibraryRootsPageAsksAndSendsTheHostHint(t *testing.T) {
	body := libraryRootsHTML
	for _, marker := range []string{
		`id="host-platform"`, `value="unraid"`, `id="host-service"`,
		"roots.hostWhere", "roots.hostUnraid", "roots.hostServiceLabel", "roots.hostHintNote",
		"function hostHint()",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("page missing host-hint marker %q", marker)
		}
	}
	// Sending it is the part that silently breaks: the selector can sit on the
	// page looking correct while no inspect call carries its value.
	sends := strings.Count(body, "hostHint()")
	if sends < 3 {
		t.Errorf("hostHint() is referenced %d times; it must be defined and sent on both the initial inspect and every regeneration", sends)
	}
	// The selector belongs in step 2, where the guidance it changes is shown.
	stepTwo := strings.Index(body, `id="step-2"`)
	selector := strings.Index(body, `id="host-platform"`)
	stepThree := strings.Index(body, `id="step-3"`)
	if !(stepTwo < selector && selector < stepThree) {
		t.Fatalf("host selector at %d must sit between step-2 (%d) and step-3 (%d)", selector, stepTwo, stepThree)
	}
}

// The endpoint must accept the hint and must drop a platform label outside the
// closed set, because that label chooses which instructions an administrator is
// shown and the request is the least trustworthy place it could come from.
func TestInspectRootHonoursKnownPlatformAndDropsUnknown(t *testing.T) {
	service := newLibraryRootsTestService(t, "inspect-platform.db", "required")
	handler := NewServer("", service).Handler()
	inspect := func(platform string) string {
		request := lanRequest(http.MethodPost, "/api/v1/roots/inspect",
			strings.NewReader(`{"path":"//192.0.2.10/Video","platform":"`+platform+`"}`))
		request.Header.Set("Authorization", "Bearer "+service.AdminToken())
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("platform=%q: status=%d body=%s", platform, response.Code, response.Body.String())
		}
		return response.Body.String()
	}
	// This test binary is not in a container, so the Unraid branch is reachable
	// only through the hint; that is exactly the wiring under test.
	if !strings.Contains(inspect("unraid"), "unraid-") {
		t.Error("platform=unraid did not reach the Unraid guidance")
	}
	if strings.Contains(inspect("unraid-but-not-really"), "unraid-") {
		t.Error("an unrecognised platform label reached the Unraid guidance")
	}
}
