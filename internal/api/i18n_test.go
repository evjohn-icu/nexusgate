package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// All page constants, so the catalog-coverage scans below cannot miss a page.
var allPageSources = []struct {
	id     pageID
	source string
}{
	{"/", libraryIndexHTML},
	{"/progress", progressHTML},
	{"/repurpose", repurposeWorkspaceHTML},
	{"/tags", tagsHTML},
	{"/providers", providersHTML},
	{"/setup", setupHTML},
	{"/collections", collectionsHTML},
	{"/settings", settingsHTML},
	{"/workers", workersPageHTML},
	{"/worker-setup", workerSetupPageHTML},
	{"/library-roots", libraryRootsHTML},
}

// TestCatalogsShareIdenticalKeySetsAndPlaceholders pins the catalog contract:
// every locale must define the exact same non-empty key set and the exact same
// {placeholder} set per key. A key added to one locale and not another renders
// the wrong language or the raw key — the defect this whole feature exists to
// prevent — so it must fail here first.
func TestCatalogsShareIdenticalKeySetsAndPlaceholders(t *testing.T) {
	if len(catalogs) != len(supportedLocales) {
		t.Fatalf("loaded %d catalogs, want %d", len(catalogs), len(supportedLocales))
	}
	base := catalogs[localeZhCN]
	if len(base) == 0 {
		t.Fatal("zh-CN catalog is empty")
	}
	phRE := regexp.MustCompile(`\{[a-z]+\}`)
	placeholders := func(s string) string {
		parts := phRE.FindAllString(s, -1)
		sort.Strings(parts)
		return strings.Join(parts, ",")
	}
	for _, loc := range supportedLocales {
		cat := catalogs[loc]
		if len(cat) != len(base) {
			t.Fatalf("locale %s has %d keys, zh-CN has %d", loc, len(cat), len(base))
		}
		for key, zh := range base {
			val, ok := cat[key]
			if !ok {
				t.Fatalf("locale %s is missing key %q", loc, key)
			}
			if strings.TrimSpace(val) == "" {
				t.Fatalf("locale %s has empty value for key %q", loc, key)
			}
			if placeholders(val) != placeholders(zh) {
				t.Fatalf("placeholder set differs for %s in %s: %q vs zh-CN %q", key, loc, val, zh)
			}
		}
	}
}

// tdKeyCallRE isolates literal keys passed to tdT/tdPlural. i18nMarkerRE is
// declared in i18n.go and reused here.
var tdKeyCallRE = regexp.MustCompile(`td(?:T|Plural)\('([a-zA-Z0-9._-]+)'`)

// TestEveryPageMarkerAndRuntimeKeyResolves scans every page constant for static
// [[i18n:key]] markers and literal tdT/tdPlural keys and requires each to exist
// in the zh-CN catalog (which, by the parity test above, means every locale).
// A plural base key is allowed to resolve through its .one/.other siblings.
// Static markers are additionally required to reference placeholder-free
// values, because markers are resolved server-side without substitution vars.
func TestEveryPageMarkerAndRuntimeKeyResolves(t *testing.T) {
	cat := catalogs[localeZhCN]
	seen := map[string]bool{}
	for _, p := range allPageSources {
		for _, m := range i18nMarkerRE.FindAllStringSubmatch(p.source, -1) {
			seen[m[1]] = true
		}
		for _, m := range tdKeyCallRE.FindAllStringSubmatch(p.source, -1) {
			seen[m[1]] = true
		}
	}
	if len(seen) == 0 {
		t.Fatal("scan found no keys — the marker regexes are broken")
	}
	for key := range seen {
		if cat.has(key) {
			continue
		}
		// Plural base: resolve via category siblings.
		if cat.has(key+".one") || cat.has(key+".other") {
			continue
		}
		t.Errorf("key %q used by a page is missing from the catalogs", key)
	}
	// Static markers must be placeholder-free.
	for _, p := range allPageSources {
		for _, m := range i18nMarkerRE.FindAllStringSubmatch(p.source, -1) {
			key := m[1]
			if strings.Contains(cat.get(key), "{") {
				t.Errorf("static marker [[i18n:%s]] references a value with a {placeholder}, which markers cannot substitute", key)
			}
		}
	}
}

// TestResolveLocale pins the precedence chain: timingdex_locale cookie wins,
// then the best Accept-Language match, then zh-CN; malformed or unsupported
// cookie values fall through to Accept-Language; regional variants (fr-CA,
// en-GB, es-MX, ja) resolve to the canonical family locale.
func TestResolveLocale(t *testing.T) {
	cases := []struct {
		name       string
		cookie     string
		acceptLang string
		want       locale
	}{
		{"cookie wins", "fr-FR", "ja-JP", localeFrFR},
		{"cookie regional variant", "es-MX", "", localeEsES},
		{"cookie ja family", "ja", "", localeJaJP},
		{"cookie malformed ignored", "not a locale", "ja-JP", localeJaJP},
		{"cookie unsupported ignored", "de-DE", "fr-FR", localeFrFR},
		{"cookie empty ignored", "", "en-US", localeEnUS},
		{"accept exact", "", "en-US", localeEnUS},
		{"accept regional", "", "en-GB", localeEnUS},
		{"accept fr-CA", "", "fr-CA", localeFrFR},
		{"accept ja", "", "ja", localeJaJP},
		{"accept unsupported falls back", "", "de-DE", localeZhCN},
		{"no headers default", "", "", localeZhCN},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: localeCookie, Value: tc.cookie})
			}
			if tc.acceptLang != "" {
				r.Header.Set("Accept-Language", tc.acceptLang)
			}
			if got := resolveLocale(r); got != tc.want {
				t.Fatalf("resolveLocale(cookie=%q, accept=%q) = %s, want %s", tc.cookie, tc.acceptLang, got, tc.want)
			}
		})
	}
}

// TestServeLocalizedPageHeadersAndLang renders one page in every supported
// locale and asserts the <html lang>, Content-Language and Vary contract.
func TestServeLocalizedPageHeadersAndLang(t *testing.T) {
	service := providerChannelTestService(t, "i18n-serve.db")
	handler := NewServer("", service).Handler()
	for _, loc := range supportedLocales {
		t.Run(string(loc), func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/providers", nil)
			r.Header.Set("Accept-Language", string(loc))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, `<html lang="`+string(loc)+`"`) {
				t.Fatalf("page does not carry lang=%s", loc)
			}
			if got := w.Header().Get("Content-Language"); got != string(loc) {
				t.Fatalf("Content-Language=%q, want %q", got, loc)
			}
			vary := w.Header().Get("Vary")
			if !strings.Contains(vary, "Accept-Language") || !strings.Contains(vary, "Cookie") {
				t.Fatalf("Vary=%q must carry both Accept-Language and Cookie", vary)
			}
		})
	}
}

// TestServeLocalizedPageCookiePreference renders the same page via cookie vs
// Accept-Language and asserts the cookie wins (different <html lang>).
func TestServeLocalizedPageCookiePreference(t *testing.T) {
	service := providerChannelTestService(t, "i18n-cookie.db")
	handler := NewServer("", service).Handler()
	r := httptest.NewRequest(http.MethodGet, "/settings", nil)
	r.AddCookie(&http.Cookie{Name: localeCookie, Value: "ja-JP"})
	r.Header.Set("Accept-Language", "en-US")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `<html lang="ja-JP"`) {
		t.Fatalf("cookie preference did not win: %s", w.Body.String()[:200])
	}
}

// TestPageCacheConcurrent requests the same and different (pageID, locale)
// pairs in parallel and compares complete bytes, so a race on the lazy cache
// fails under the race detector rather than corrupting a page.
func TestPageCacheConcurrent(t *testing.T) {
	service := providerChannelTestService(t, "i18n-cache.db")
	handler := NewServer("", service).Handler()

	var wg sync.WaitGroup
	results := make([][]byte, 0, 32)
	var mu sync.Mutex
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			loc := supportedLocales[i%len(supportedLocales)]
			id := pageID(allPageSources[i%len(allPageSources)].id)
			r := httptest.NewRequest(http.MethodGet, string(id), nil)
			r.Header.Set("Accept-Language", string(loc))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			mu.Lock()
			results = append(results, w.Body.Bytes())
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	// Same (pageID, locale) responses must be byte-identical.
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			locI := supportedLocales[i%len(supportedLocales)]
			locJ := supportedLocales[j%len(supportedLocales)]
			idI := allPageSources[i%len(allPageSources)].id
			idJ := allPageSources[j%len(allPageSources)].id
			if idI == idJ && locI == locJ {
				if !bytes.Equal(results[i], results[j]) {
					t.Fatalf("cached page bytes differ for identical (%s, %s) requests", idI, locI)
				}
			}
		}
	}
}
