package api

import (
	"strings"
	"testing"
)

// TestLibraryPageRestoresSearchFromURL pins U3-01. A shared or refreshed URL
// carries q plus the filters that produced the result; the page used to put
// the query back in the box and then run the browse listing, so the link
// showed a different thing than the sender saw. The two ordering facts below
// are the fix's whole point: the session and collection selects must be
// populated before search() runs, because search() calls syncURL() and would
// otherwise rewrite the address bar without the very params it is restoring.
func TestLibraryPageRestoresSearchFromURL(t *testing.T) {
	page := libraryIndexHTML
	for _, marker := range []string{
		`activeCollection=params.get('collection')||''`, // the collection was never restored at all
		`const restored=document.getElementById('q').value.trim()`,
		`const ready=Promise.all([loadSessions(),loadCollections()])`,
		`await ready;refreshFilterUI();search()`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("URL restore missing %q", marker)
		}
	}
	if strings.Contains(page, `function loadLibrary(){restoreFromURL();loadSessions();loadCollections();load()}`) {
		t.Fatal("loadLibrary still degrades a restored query to a browse listing")
	}
	// A restored query must not race the selects: the await has to sit before
	// search(), never after.
	body := page[strings.Index(page, "async function loadLibrary()"):]
	body = body[:strings.Index(body, "</script>")]
	awaitAt, searchAt := strings.Index(body, "await ready"), strings.Index(body, "search()")
	if awaitAt < 0 || searchAt < 0 || awaitAt > searchAt {
		t.Fatalf("loadLibrary runs search() before the selects are populated (await at %d, search at %d)", awaitAt, searchAt)
	}
}

// TestLibraryPagePaginatesShotResults pins U3-02. The backend has carried
// offset/has_more/next_offset/window_exhausted since v0.31; the page asked for
// 40 rows, rendered them, and said nothing about the rest.
func TestLibraryPagePaginatesShotResults(t *testing.T) {
	page := libraryIndexHTML
	for _, marker := range []string{
		`offset:shotPage.offset`,                   // the request carries the page
		`shotPage.hasMore=!!(data&&data.has_more)`, // and reads the answer back
		`shotPage.nextOffset=data&&typeof data.next_offset==='number'?data.next_offset:null`,
		`shotPage.windowExhausted=!!(data&&data.window_exhausted)`,
		`shotPage.items=append?shotPage.items.concat(page):page`, // load-more appends
		`function loadMoreShots()`,
		`id="shot-more-btn"`,
		`tdT('library.loadMore')`,
		`shotPageFooter()`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("shot pagination missing %q", marker)
		}
	}
	// window_exhausted must not be reported as "that is everything": the page
	// ended on the hard search window, and more may exist past it.
	if !strings.Contains(page, `tdT('library.windowExhausted')`) {
		t.Fatal("window_exhausted has no distinct message")
	}
	footer := page[strings.Index(page, "function shotPageFooter()"):]
	footer = footer[:strings.Index(footer, "function clearSearch")]
	exhausted, allShown := strings.Index(footer, "library.windowExhausted"), strings.Index(footer, "library.allShown")
	if exhausted < 0 || allShown < 0 || exhausted > allShown {
		t.Fatal("the footer must test window_exhausted before claiming all results are shown")
	}
}

// TestLibraryRootsPageOffersLoginOnAuthFailure pins U2-01. Every admin call on
// the wizard used to collapse a 401 into prose with no way to act, and the
// health panel labelled every failure — including an unreachable Hub — as an
// auth problem.
func TestLibraryRootsPageOffersLoginOnAuthFailure(t *testing.T) {
	page := libraryRootsHTML
	for _, marker := range []string{
		`err.status=r.status`, // the status survives the throw
		`addErr.status=response.status`,
		`tdAuthDenied(e)`,       // and every catch branches on it
		`tdAdminLoginRequired(`, // the login control is reachable from the page
		`tdT('roots.authRequired')`,
		`nexusslate:admin-auth-changed`, // logging in retries the step
		`tdT('roots.healthFailed'`,      // a non-auth failure says what actually happened
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("library-roots auth fallback missing %q", marker)
		}
	}
	// Every catch that renders a failure must consult tdAuthDenied; a bare
	// contradicted callout with no auth branch is the old behaviour.
	if n := strings.Count(page, "tdAuthDenied(e)"); n < 6 {
		t.Fatalf("only %d catch sites branch on auth denial, want every admin call site", n)
	}
	if strings.Contains(page, `tdT('roots.healthAuthRequired')`) {
		t.Fatal("the health panel still labels every failure as an auth problem")
	}
}

// TestLibraryRootsPageStatusSelectorIsValid pins U2-03. The list once began
// with a stray "+" combinator, and a CSS selector list is not forgiving: one
// invalid selector voids the whole comma-separated list, so neither status
// span was ever hidden while empty. The page still built and served.
func TestLibraryRootsPageStatusSelectorIsValid(t *testing.T) {
	if strings.Contains(libraryRootsHTML, "+#step1-status:empty") {
		t.Fatal("the status-hiding selector list still starts with a stray combinator")
	}
	if !strings.Contains(libraryRootsHTML, "#step1-status:empty,#discover-status:empty{display:none}") {
		t.Fatal("the status-hiding rule is gone")
	}
	if n := strings.Count(libraryRootsHTML, ".panel{padding:20px;margin-bottom:16px}"); n != 1 {
		t.Fatalf("the .panel rule appears %d times, want 1", n)
	}
}

// TestSetupPageNamesEveryFailedCheck pins U2-02. "✗ 需要处理" told the reader a
// check had failed and nothing else — not which remedy applies, and not that
// `nexusslate doctor` prints the same probes with the paths this page
// deliberately withholds (paths are admin-only everywhere else in the UI).
func TestSetupPageNamesEveryFailedCheck(t *testing.T) {
	page := setupHTML
	for _, marker := range []string{
		`id="env-fixes"`,
		`function needsFix(ok,label,how)`,
		`tdT('setup.env.fixFfmpeg')`,
		`tdT('setup.env.fixFfprobe')`,
		`tdT('setup.env.fixDataDir')`,
		`tdT('setup.env.fixCache')`,
		`tdT('setup.env.fixDisk')`,
		`tdT('setup.env.fixDb')`,
		`tdT('setup.env.fixTitle')`,
		`tdPlural('setup.status.envIssues',envFixes.length)`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("setup remediation missing %q", marker)
		}
	}
	// The status line used to say 状态已更新 even when six checks had just
	// failed.
	if strings.Contains(page, `if(box){box.className='status ok';box.textContent=tdT('setup.status.updated')}`) {
		t.Fatal("setup still reports success after a failed environment check")
	}
	// The remediation copy must not name a filesystem path: /setup is a
	// trusted-read page and every other surface treats root paths as
	// admin-only.
	catalog := catalogs[localeZhCN]
	for _, key := range []string{"setup.env.fixDataDir", "setup.env.fixCache", "setup.env.fixDisk"} {
		if v := catalog[key]; strings.Contains(v, "/") && !strings.Contains(v, "nexusslate cache gc") {
			t.Fatalf("%s leaks a path-like string: %q", key, v)
		}
	}
}

// TestWorkerSetupFailureTextUsesTokens pins U1-01/U1-02. The generate-failure
// line was the only self-rescue text on the wizard and it was painted a
// leftover dark-theme salmon on a light panel: 1.66:1, the least readable
// string on the page at the moment it mattered most.
func TestWorkerSetupFailureTextUsesTokens(t *testing.T) {
	for _, hex := range []string{"#ffb7ac", "#bfcae0"} {
		if strings.Contains(workerSetupPageHTML, hex) {
			t.Fatalf("worker setup page still hardcodes %s", hex)
		}
	}
	if !strings.Contains(workerSetupPageHTML, `<div class="callout callout--contradicted" role="alert"><span>'+esc(tdT('workerSetup.generateFailed'`) {
		t.Fatal("the generate-failure message no longer uses the shared contradicted callout")
	}
}

// TestLibraryEmptyStateUsesTokens pins the same class of defect on the library
// page's first-run guidance (audit U1-03/U3-03): three inline hexes that
// TestPageCSSUsesDesignTokens cannot see, because it only scans <style> blocks.
func TestLibraryEmptyStateUsesTokens(t *testing.T) {
	for _, hex := range []string{"#b7c8eb", "#324767", "#eaf1ff"} {
		if strings.Contains(libraryIndexHTML, hex) {
			t.Fatalf("library empty state still hardcodes %s", hex)
		}
	}
	if !strings.Contains(libraryIndexHTML, `<a class="btn btn--primary" href="/library-roots"`) {
		t.Fatal("the empty-state action is no longer the shared primary button")
	}
}

// TestShellPanelRuleIsSpecificityZero pins U1-04. shellCSS is injected after
// each page's own <style>, so a bare .panel here beats every page rule of the
// same name — which is exactly what the v0.32 punchlist's P0 was about. That
// fix landed on the compatibility layer but not on this copy, leaving it
// inert: /setup asks for --raised panels and /collections for margin-bottom:0,
// and both silently lost.
func TestShellPanelRuleIsSpecificityZero(t *testing.T) {
	if strings.Contains(shellCSS, "\n.panel{background:var(--surface)") {
		t.Fatal("the shell still declares a bare .panel rule that outranks every page")
	}
	if !strings.Contains(shellCSS, ":where(.panel){background:var(--surface)") {
		t.Fatal("the shell .panel rule is not wrapped in :where()")
	}
	// The page rules this unblocks, so a future revert fails here too.
	if !strings.Contains(setupHTML, ".panel{background:var(--raised)") {
		t.Fatal("/setup no longer asks for a raised panel")
	}
	if !strings.Contains(collectionsHTML, ".panel{margin-bottom:0}") {
		t.Fatal("/collections no longer asks for a flush panel")
	}
}

// TestLibraryRootsWizardShowsOneStep pins a defect the audit did not find and
// this accessibility pass surfaced: `goStep()` toggles `.active` on `.step`,
// and nothing in shellCSS or the page ever declared `.step{display:none}`, so
// all four panels rendered at once. The numbered nav above them described a
// sequence that was not happening, and a first-time visitor met an empty
// mount-point field, an empty verify result and a scan panel before typing
// anything. `worker_setup_page.go` got this right with `.step-panel`.
func TestLibraryRootsWizardShowsOneStep(t *testing.T) {
	page := libraryRootsHTML
	if !strings.Contains(page, ".step{display:none}") {
		t.Fatal("inactive wizard steps are not hidden; all four panels render at once")
	}
	if !strings.Contains(page, ".step.active{display:block}") {
		t.Fatal("the active wizard step has no rule to show it")
	}
	// The rule must not exist only in the shell, where a page-level .step
	// could not override it, and must not be shadowed by one.
	if strings.Contains(shellCSS, ".step{display:") {
		t.Fatal("shellCSS now declares .step visibility; the page rule would race it")
	}
}

// TestLibraryRootsWizardIsAccessible pins U2-05. The page was 445 lines with
// zero `aria-*` and zero `role=` — the largest and most complex surface in the
// product, and the one a new user meets first.
func TestLibraryRootsWizardIsAccessible(t *testing.T) {
	page := libraryRootsHTML
	// Every container the wizard writes an outcome into announces it. Without
	// this the whole feedback loop is silent to a screen reader: a button is
	// pressed, text appears somewhere, and nothing is said.
	for _, id := range []string{"root-health-wrap", "discover-status", "discover-results", "step1-status", "verify-result", "added-summary", "scan-result"} {
		i := strings.Index(page, `id="`+id+`"`)
		if i < 0 {
			t.Fatalf("container %q is gone", id)
		}
		tag := page[i:]
		tag = tag[:strings.Index(tag, ">")]
		if !strings.Contains(tag, `role="status"`) || !strings.Contains(tag, `aria-live="polite"`) {
			t.Fatalf("container %q is not a live region: %s", id, tag)
		}
	}
	// The wizard nav is a landmark with exactly one current step, and goStep
	// moves it — leaving aria-current on step 1 forever tells a screen reader
	// the wizard never advanced (the defect audit U4-03 filed against the
	// other wizard).
	if !strings.Contains(page, `<nav class="steps" id="step-nav" aria-label=`) {
		t.Fatal("the wizard nav is not a labelled landmark")
	}
	if !strings.Contains(page, `marker.setAttribute('aria-current','step')`) ||
		!strings.Contains(page, `marker.removeAttribute('aria-current')`) {
		t.Fatal("goStep does not move aria-current")
	}
	if n := strings.Count(page, `aria-current="step"`); n != 1 {
		t.Fatalf("the served page marks %d current steps, want exactly 1", n)
	}
	// Each step panel is a labelled group so its purpose is announced when
	// focus lands inside it.
	for i := 1; i <= 4; i++ {
		if !strings.Contains(page, `id="step-`+string(rune('0'+i))+`" role="group" aria-label=`) {
			t.Fatalf("step %d is not a labelled group", i)
		}
	}
}

// U4-03: the Worker setup wizard had the same defect the roots wizard had —
// aria-current was written into the markup on step 1 and goStep only ever
// rewrote className, so a screen reader was told "step 1 of 4" for the whole
// four-step flow, including on the page that hands out a one-time pairing
// token.
func TestWorkerSetupWizardIsAccessible(t *testing.T) {
	page := workerSetupPageHTML
	for _, id := range []string{"hub-info", "binary-grid", "mounts-panel", "script-area"} {
		i := strings.Index(page, `id="`+id+`"`)
		if i < 0 {
			t.Fatalf("container %q is gone", id)
		}
		tag := page[i:]
		tag = tag[:strings.Index(tag, ">")]
		if !strings.Contains(tag, `role="status"`) || !strings.Contains(tag, `aria-live="polite"`) {
			t.Fatalf("container %q is not a live region: %s", id, tag)
		}
	}
	if !strings.Contains(page, `<nav class="steps" id="step-nav" aria-label=`) {
		t.Fatal("the wizard nav is not a labelled landmark")
	}
	if !strings.Contains(page, `marker.setAttribute('aria-current','step')`) ||
		!strings.Contains(page, `marker.removeAttribute('aria-current')`) {
		t.Fatal("goStep does not move aria-current")
	}
	if n := strings.Count(page, `aria-current="step"`); n != 1 {
		t.Fatalf("the served page marks %d current steps, want exactly 1", n)
	}
	for i := 1; i <= 4; i++ {
		if !strings.Contains(page, `id="step-`+string(rune('0'+i))+`" role="group" aria-label=`) {
			t.Fatalf("step %d is not a labelled group", i)
		}
	}
	// is-done is styled in the shell (.steps span.is-done) but this wizard
	// never set it, so a completed step looked identical to an unvisited one.
	if !strings.Contains(page, `(i<n?'is-done':'')`) {
		t.Fatal("goStep does not mark completed steps as done")
	}
}

// U3-05: found while writing the U3-04 browser spec, not by the audit. Every
// filter control on the library page carried onchange="load()", and load() is
// the asset browse listing. Since the search became shot-first (U3-02), that
// meant narrowing a filter after a search silently threw the search away and
// replaced the shot cards with the unfiltered asset list — the one gesture a
// user makes to refine a result set was the one that discarded it. The
// handlers simply predated the shot-first refactor.
func TestLibraryFilterChangeKeepsTheSearch(t *testing.T) {
	page := libraryIndexHTML
	if strings.Contains(page, `onchange="load()"`) {
		t.Fatal("a filter control still reloads the browse listing, discarding an active search")
	}
	if n := strings.Count(page, `onchange="refreshResults()"`); n < 9 {
		t.Fatalf("only %d filter controls route through refreshResults, want at least 9", n)
	}
	// refreshResults is the whole fix: with a query it re-runs the search,
	// without one it is exactly today's browse listing.
	if !strings.Contains(page, `function refreshResults(){const q=document.getElementById('q').value.trim();return q?search():load()}`) {
		t.Fatal("refreshResults does not dispatch on the current query")
	}
	// The other filter-driven entry points go through it too; a chip removal
	// or a Clear that dropped back to browse would have the same effect.
	for _, site := range []string{
		`function applyFilters(){closeFilterDrawer();refreshResults();syncURL()}`,
		`el.value=''}refreshResults();syncURL()}`,
		`document.getElementById('max-duration').value='';refreshResults();syncURL()}`,
	} {
		if !strings.Contains(page, site) {
			t.Fatalf("filter entry point still calls load() directly: %s", site)
		}
	}
	// Collection is the one filter that deliberately stays on load(): it is a
	// saved *asset* filter and runShotSearch's body carries no collection, so
	// routing it through refreshResults would have made picking a collection
	// during a search a silent no-op — the chip appears, the URL updates, the
	// results do not move. Dropping to that collection's asset listing is at
	// least an answer.
	for _, site := range []string{
		`async function loadCollection(id){activeCollection=id||'';load()}`,
		`activeCollection='';load();syncURL();return`,
	} {
		if !strings.Contains(page, site) {
			t.Fatalf("collection selection no longer falls back to the browse listing: %s", site)
		}
	}
}
