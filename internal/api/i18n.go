package api

import (
	"bytes"
	"embed"
	"encoding/json"
	"html"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/text/language"
)

// Browser-interface localization. The Hub serves 11 inline HTML pages; every
// piece of product copy they carry is drawn from one embedded catalog so a
// browser request can be rendered in Simplified Chinese (the default), or in
// Japanese, US English, French or Spanish, without touching the CLI, MCP,
// Agent, Worker, API fields or API error semantics. Only the selected catalog
// (plus the zh-CN fallback the robustness note in the plan requires) is
// marshalled into the page, immediately before </head>, so the initial markup,
// <title>, dynamic states, confirmations and errors all agree without a
// Chinese first-paint flash.
//
// The catalog lives as JSON files under internal/api/locales/. Every locale
// must carry the identical key set and placeholder set; i18n_test.go pins
// that contract. Static HTML text and attribute copy is referenced with
// [[i18n:key]] markers that serveLocalizedPage resolves server-side with HTML
// escaping; dynamic JavaScript copy calls the injected tdT/tdPlural helpers
// (and never builds a key by concatenating an untrusted wire value).

// locale is a canonical BCP-47 tag the Hub UI can render. The set is closed:
// a request for any other language falls back to zh-CN rather than being
// invented.
type locale string

const (
	localeZhCN locale = "zh-CN"
	localeJaJP locale = "ja-JP"
	localeEnUS locale = "en-US"
	localeFrFR locale = "fr-FR"
	localeEsES locale = "es-ES"
)

// supportedLocales is the closed, declaration-ordered set of UI locales;
// supportedTags[i] is the x/text tag for supportedLocales[i], kept in the
// same order so langMatcher.Match's returned index addresses both.
var supportedLocales = []locale{localeZhCN, localeJaJP, localeEnUS, localeFrFR, localeEsES}

var supportedTags = func() []language.Tag {
	tags := make([]language.Tag, 0, len(supportedLocales))
	for _, loc := range supportedLocales {
		tags = append(tags, language.MustParse(string(loc)))
	}
	return tags
}()

// langMatcher resolves any BCP-47 tag (Accept-Language entry or cookie value)
// to the closest supported locale, e.g. fr-CA→fr-FR, en-GB→en-US, ja→ja-JP.
var langMatcher = language.NewMatcher(supportedTags)

// localeCookie is the preference cookie the shell selector writes. It is a
// preference, not a credential, so it deliberately stays usable on local HTTP
// deployments: no Secure flag, SameSite=Lax, one-year lifetime, and never
// mirrored into localStorage/sessionStorage (which the credential-leak checks
// require to remain empty).
const localeCookie = "nexusgate_locale"

//go:embed locales/*.json
var localesFS embed.FS

// catalog is one locale's key→value table. Values are plain text with named
// {placeholder} slots; they never contain HTML (the resolver and the runtime
// tdT both treat values as text to be escaped by the caller).
type catalog map[string]string

// catalogs is loaded once at package init from the embedded JSON files. A
// missing or malformed file is a build-time defect, not a runtime decision,
// so it panics immediately rather than serving half a UI.
var catalogs = func() map[locale]catalog {
	m := make(map[locale]catalog, len(supportedLocales))
	for _, loc := range supportedLocales {
		data, err := localesFS.ReadFile("locales/" + string(loc) + ".json")
		if err != nil {
			panic("i18n: cannot load locale catalog " + string(loc) + ": " + err.Error())
		}
		var c catalog
		if err := json.Unmarshal(data, &c); err != nil {
			panic("i18n: invalid catalog " + string(loc) + ": " + err.Error())
		}
		m[loc] = c
	}
	return m
}()

// has reports whether key exists in this locale's catalog.
func (c catalog) has(key string) bool {
	_, ok := c[key]
	return ok
}

// get returns key's value in this locale, falling back to the zh-CN value and
// finally to the key itself, so a missing key is visible (diagnosable) rather
// than a blank control. The plan's robustness note requires exactly this
// chain; i18n_test.go's key-parity check is what keeps the fallback cold.
func (c catalog) get(key string) string {
	if v, ok := c[key]; ok {
		return v
	}
	if v, ok := catalogs[localeZhCN][key]; ok {
		return v
	}
	return key
}

var i18nMarkerRE = regexp.MustCompile(`\[\[i18n:([a-zA-Z0-9._-]+)\]\]`)

// resolveMarkers substitutes every [[i18n:key]] marker with the locale value,
// HTML-escaped so a marker in a text node or an attribute cannot inject
// markup. Static markers are reserved for copy without placeholders;
// placeholder-bearing copy goes through tdT at runtime.
func (c catalog) resolveMarkers(s string) string {
	return i18nMarkerRE.ReplaceAllStringFunc(s, func(m string) string {
		key := m[len("[[i18n:") : len(m)-len("]]")]
		return html.EscapeString(c.get(key))
	})
}

// canonicalLocale maps a raw cookie value to a canonical locale. Malformed or
// unsupported values report false and the caller falls through to
// Accept-Language, never reflecting the raw value.
func canonicalLocale(raw string) (locale, bool) {
	tag, err := language.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	_, idx, conf := langMatcher.Match(tag)
	if conf == language.No {
		return "", false
	}
	return supportedLocales[idx], true
}

// localeFromAcceptLanguage resolves an Accept-Language header to the best
// supported locale. Unsupported browser languages fall back to zh-CN (the
// matcher's default). The first accepted language that matches wins, so a
// request preferring de-DE but accepting fr second renders French rather than
// the default.
func localeFromAcceptLanguage(header string) (locale, bool) {
	tags, _, err := language.ParseAcceptLanguage(header)
	if err != nil || len(tags) == 0 {
		return "", false
	}
	for _, tag := range tags {
		_, idx, conf := langMatcher.Match(tag)
		if conf != language.No {
			return supportedLocales[idx], true
		}
	}
	return "", false
}

// resolveLocale is the precedence chain: nexusgate_locale cookie → best
// Accept-Language match → zh-CN.
func resolveLocale(r *http.Request) locale {
	if c, err := r.Cookie(localeCookie); err == nil {
		if loc, ok := canonicalLocale(c.Value); ok {
			return loc
		}
	}
	if loc, ok := localeFromAcceptLanguage(r.Header.Get("Accept-Language")); ok {
		return loc
	}
	return localeZhCN
}

// pageID is a page's identity for the rendered-page cache. Each page's exact
// route path is its ID; query strings never affect page markup, so they are
// deliberately not part of the key.
type pageID string

type pageCacheKey struct {
	id  pageID
	loc locale
}

// pageCache lazily holds the final branded bytes for each (pageID, locale)
// pair. Pages are static after branding and marker resolution, so the cache
// is a pure speed-up; a lost entry just rebuilds. A mutex keeps concurrent
// requests from racing on the map (covered by the race-detector cache test).
type pageCache struct {
	mu    sync.Mutex
	items map[pageCacheKey][]byte
}

var cachedPages = pageCache{items: map[pageCacheKey][]byte{}}

// i18nRuntimeJS defines the collision-free bootstrap helpers every page can
// call. It is injected with the selected catalog in the <head>, before any
// page script, so page scripts can use it at parse time. Values are always
// plain text: a caller that inserts a tdT result into innerHTML must pass it
// through the page's own esc helper.
const i18nRuntimeJS = `
function _tdLookup(key){var c=window.TD_CATALOG;if(c&&Object.prototype.hasOwnProperty.call(c,key))return c[key];var z=window.TD_ZH;if(z&&Object.prototype.hasOwnProperty.call(z,key))return z[key];return null}
function tdT(key,vars){var s=_tdLookup(key);if(s==null)s=key;if(vars){for(var k in vars){s=s.split('{'+k+'}').join(String(vars[k]))}}return s}
function tdPlural(key,count,vars){var rule='other';try{rule=new Intl.PluralRules(window.TD_LOCALE).select(Number(count)||0)}catch(_){}var full=key+'.'+rule;var s=_tdLookup(full);if(s==null)s=_tdLookup(key+'.other');if(s==null)s=_tdLookup(key);if(s==null)s=key;var all={count:count};if(vars){for(var k in vars){all[k]=vars[k]}}for(var k in all){s=s.split('{'+k+'}').join(String(all[k]))}return s}
function tdFormatNumber(v){var n=Number(v);if(!Number.isFinite(n))return String(v);try{return new Intl.NumberFormat(window.TD_LOCALE).format(n)}catch(_){return String(v)}}
function tdFormatDate(v){var d=v instanceof Date?v:new Date(v);if(isNaN(d.getTime()))return String(v);try{return new Intl.DateTimeFormat(window.TD_LOCALE,{year:'numeric',month:'short',day:'numeric'}).format(d)}catch(_){return String(v)}}
function tdFormatTime(v){var d=v instanceof Date?v:new Date(v);if(isNaN(d.getTime()))return String(v);try{return new Intl.DateTimeFormat(window.TD_LOCALE,{hour:'2-digit',minute:'2-digit'}).format(d)}catch(_){return String(v)}}
function tdFormatDateTime(v){var d=v instanceof Date?v:new Date(v);if(isNaN(d.getTime()))return String(v);try{return new Intl.DateTimeFormat(window.TD_LOCALE,{dateStyle:'medium',timeStyle:'short'}).format(d)}catch(_){return String(v)}}
async function tdApiErrorMessage(response){var text='';try{text=await response.clone().text()}catch(_){}var e=null;try{var d=JSON.parse(text);e=d&&d.error?d.error:null}catch(_){}if(e&&e.code){var msg=_tdLookup('api.error.'+e.code);if(msg!=null)return msg}if(e&&e.action){var act=_tdLookup('api.action.'+e.action);if(act!=null)return act}var status=(response&&response.status)||0;var g=_tdLookup('api.http.'+status);if(g!=null)return g;return text||('HTTP '+(status||'?'))}
`

func headRuntimeScript(loc locale) string {
	cat, err := json.Marshal(catalogs[loc])
	if err != nil {
		panic("i18n: catalog " + string(loc) + " is not JSON-marshalable: " + err.Error())
	}
	zh, err := json.Marshal(catalogs[localeZhCN])
	if err != nil {
		panic("i18n: zh-CN catalog is not JSON-marshalable: " + err.Error())
	}
	// Escape "</" inside the JSON so a catalog value cannot close the <script>
	// element it is emitted inside.
	catJSON := string(bytes.ReplaceAll(cat, []byte("</"), []byte("<\\/")))
	zhJSON := string(bytes.ReplaceAll(zh, []byte("</"), []byte("<\\/")))
	locJSON := string(bytes.ReplaceAll(mustJSON(string(loc)), []byte("</"), []byte("<\\/")))

	var b strings.Builder
	b.WriteString(`<script id="nexusgate-i18n">window.TD_LOCALE=`)
	b.WriteString(locJSON)
	b.WriteString(`;window.TD_CATALOG=`)
	b.WriteString(catJSON)
	b.WriteString(`;window.TD_ZH=`)
	b.WriteString(zhJSON)
	b.WriteString(`;`)
	b.WriteString(i18nRuntimeJS)
	b.WriteString(`</script>`)
	return b.String()
}

// mustJSON marshals v and panics on error; used only for values that cannot
// fail to marshal (strings and the already-unmarshalled catalogs).
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("i18n: marshal: " + err.Error())
	}
	return b
}

// serveLocalizedPage renders one inline page in the request's locale: it
// resolves the locale, injects the head runtime, injects the shell, swaps the
// <html lang>, resolves the static [[i18n:key]] markers, applies branding, and
// writes the (cached) result with Content-Type/Content-Language/Vary headers.
func (s *Server) serveLocalizedPage(w http.ResponseWriter, r *http.Request, id pageID, source string) {
	loc := resolveLocale(r)
	key := pageCacheKey{id: id, loc: loc}

	cachedPages.mu.Lock()
	body, ok := cachedPages.items[key]
	cachedPages.mu.Unlock()
	if ok {
		writeLocalizedHeaders(w, loc)
		_, _ = w.Write(body)
		return
	}

	cat := catalogs[loc]
	page := source
	page = strings.Replace(page, "</head>", headRuntimeScript(loc)+"</head>", 1)
	page = shelledPage(page, loc)
	page = strings.Replace(page, `lang="zh-CN"`, `lang="`+string(loc)+`"`, 1)
	page = cat.resolveMarkers(page)
	page = brandedPage(page)

	body = []byte(page)
	cachedPages.mu.Lock()
	cachedPages.items[key] = body
	cachedPages.mu.Unlock()

	writeLocalizedHeaders(w, loc)
	_, _ = w.Write(body)
}

// writeLocalizedHeaders stamps the locale-sensitive response headers. Vary is
// set once with both request inputs the cache key depends on, so a shared
// cache cannot serve one locale's page to another.
func writeLocalizedHeaders(w http.ResponseWriter, loc locale) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Language", string(loc))
	w.Header().Set("Vary", "Accept-Language, Cookie")
}
