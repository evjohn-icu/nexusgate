package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/normalize"
)

// enhanceLibraryPage is a chain of exact-match string replacements over a
// legacy constant, and every later anchor sits inside text an earlier
// replacement inserted. A stale anchor makes strings.Replace no-op silently:
// the page still builds and serves and the feature is just gone. This test
// replays the slice the way enhanceLibraryPage does and requires each anchor
// to match exactly once at the moment it is applied — zero is the silent
// no-op, two is an ambiguous patch. The replay must reproduce libraryIndexHTML
// exactly, so the test is coupled to the shipped page, not to data it built.
func TestLibraryPagePatchesApplyInOrderAndBite(t *testing.T) {
	page := legacyLibraryIndexHTML
	for i, patch := range libraryPagePatches {
		n := strings.Count(page, patch.anchor)
		if n != 1 {
			name := patch.anchor
			if len(name) > 80 {
				name = name[:80] + "…"
			}
			if n == 0 {
				t.Fatalf("patch %d: anchor not found (silent no-op): %q", i, name)
			}
			t.Fatalf("patch %d: anchor matches %d times (ambiguous patch): %q", i, n, name)
		}
		page = strings.Replace(page, patch.anchor, patch.replacement, 1)
	}
	if page != libraryIndexHTML {
		t.Fatal("replayed patches do not reproduce libraryIndexHTML: enhanceLibraryPage and the patch slice have drifted")
	}
}

// Every vocabulary value needs a Chinese label, or the select renders a raw
// English slug into a Chinese UI. The map and the vocabularies are separate
// sources of truth; this pins the seam so a value added to a vocabulary fails
// loudly instead of rendering as its slug.
func TestFacetLabelsCoverEveryVocabularyValue(t *testing.T) {
	for _, field := range []struct {
		name   string
		values []string
	}{
		{"asset_type", normalize.AssetTypeValues},
		{"shot_size", normalize.ShotSizeValues},
		{"camera_motion", normalize.MotionValues},
		{"audio_type", normalize.AudioTypeValues},
		{"quality", normalize.QualityValues},
		{"usable_as", normalize.UsableAsValues},
	} {
		for _, value := range field.values {
			if strings.TrimSpace(facetLabels[value]) == "" {
				t.Fatalf("facet value %q (%s) has no Chinese label", value, field.name)
			}
		}
	}
}

// The facet controls must render from the normalize vocabularies with Chinese
// labels, filterQuery() must send them under the API's parameter names, and
// clearFilters() must reset every one of them — the single easiest thing to
// forget when a control is added.
func TestLibraryPageRendersSemanticFacetControls(t *testing.T) {
	page := libraryIndexHTML
	if !strings.Contains(page, `aria-label="高级筛选"`) {
		t.Fatalf("library page missing the advanced-filter section")
	}
	selects := []string{
		"asset-type-select", "shot-size-select", "camera-motion-select",
		"audio-type-select", "quality-select", "usable-as-select",
	}
	for _, id := range selects {
		if !strings.Contains(page, `<select id="`+id+`"`) {
			t.Fatalf("library page missing facet select %q", id)
		}
	}
	// asset-type-select stays single-select, cleared via .value=''.
	if !strings.Contains(page, "document.getElementById('asset-type-select').value=''") {
		t.Fatalf("clearFilters() does not reset asset-type-select")
	}
	// shot-size, camera-motion, audio-type, quality, usable-as are multi-select,
	// cleared via selectedIndex=-1.
	if !strings.Contains(page, ".selectedIndex=-1") {
		t.Fatalf("clearFilters() must use selectedIndex=-1 to reset multi-select facets")
	}
	for _, id := range []string{"shot-size-select", "camera-motion-select", "audio-type-select", "quality-select", "usable-as-select"} {
		if !strings.Contains(page, id) {
			t.Fatalf("clearFilters() does not reference multi-select facet %q", id)
		}
	}
	for _, id := range []string{"min-duration", "max-duration"} {
		if !strings.Contains(page, `<input id="`+id+`"`) {
			t.Fatalf("library page missing duration input %q", id)
		}
		if !strings.Contains(page, "document.getElementById('"+id+"').value=''") {
			t.Fatalf("clearFilters() does not reset duration input %q", id)
		}
	}
	if !strings.Contains(page, `<label for="min-duration">最短时长（秒）</label>`) || !strings.Contains(page, `<label for="max-duration">最长时长（秒）</label>`) {
		t.Fatalf("duration inputs must be labelled in seconds")
	}
	// Options come from the vocabularies, never typed into JS: an English slug
	// shown as a menu item in a Chinese UI is the failure this catches.
	for _, values := range [][]string{
		normalize.AssetTypeValues,
		normalize.ShotSizeValues,
		normalize.MotionValues,
		normalize.AudioTypeValues,
		normalize.QualityValues,
		normalize.UsableAsValues,
	} {
		for _, value := range values {
			if strings.Contains(page, `<option value="`+value+`">`+value+`</option>`) {
				t.Fatalf("facet option %q rendered as its raw English slug", value)
			}
		}
	}
	if !strings.Contains(page, `<option value="extreme_close_up">特写</option>`) {
		t.Fatalf("facet options are not rendered from the vocabulary with Chinese labels")
	}
	// The select ids must be wired to the API's asset_* facet parameter names
	// (the un-prefixed names are legacy aliases, kept only for old callers).
	for _, param := range []string{
		"'asset_type'", "'asset_shot_size'", "'asset_camera_motion'", "'asset_audio_type'", "'asset_quality'", "'asset_usable_as'",
		"'min_duration_ms'", "'max_duration_ms'",
	} {
		if !strings.Contains(page, param) {
			t.Fatalf("filterQuery() does not send facet param %s", param)
		}
	}
	// A 400 from parseFacetFilter must surface as an error, not as an empty
	// library: the old fetch chain mapped any non-ok response to [], which is
	// the exact failure the 400 was written to prevent.
	if !strings.Contains(page, `if(!r.ok)throw Error(await apiErrMsg(r));return r.json()`) {
		t.Fatalf("load() must surface the server message on a non-ok response")
	}
	if strings.Contains(page, `'assets?limit=300'+filterQuery()).then(r=>r.ok?r.json():[])`) {
		t.Fatalf("load() still swallows a non-ok response into the empty-library state")
	}
	// Duration inputs must accept fractional seconds (step="any") so an
	// editor can type 0.5 for 500 ms. step="1" (the old default) and the
	// old Number.isInteger guard both silently dropped fractional input.
	if !strings.Contains(page, `step="any"`) {
		t.Fatalf("duration inputs must use step=\"any\" to accept fractional seconds")
	}
	if strings.Contains(page, `step="1"`) {
		t.Fatalf("duration inputs must not use step=\"1\" — rejects decimal seconds in browser UI")
	}
	// filterQuery() must accept finite floats, not just integers:
	// Number.isInteger(0.5) is false, which silently dropped the input.
	if !strings.Contains(page, `!Number.isFinite(sec)`) {
		t.Fatalf("filterQuery() must use !Number.isFinite(sec) to accept float seconds")
	}
	if strings.Contains(page, `Number.isInteger(sec)`) {
		t.Fatalf("filterQuery() must not use Number.isInteger(sec) — it rejects fractional seconds")
	}
	// The ms conversion must round to avoid float-artefact strings like
	// \"333.3333333333333\" from 0.333 * 1000.
	if !strings.Contains(page, `Math.round(sec*1000)`) {
		t.Fatalf("filterQuery() must use Math.round(sec*1000) for correct millisecond conversion")
	}
}

// The empty library must coach rather than dead-end: 启动配置 links to the
// setup wizard and a second line offers the roots wizard. Both anchors live in
// the legacy constant as exact-match text — the same silent-no-op hazard as
// the page patches — so they are pinned on the served page.
func TestLibraryPageEmptyStateLinksToSetupAndRoots(t *testing.T) {
	response := httptest.NewRecorder()
	service := providerChannelTestService(t, "library-empty-state-page.db")
	// Fresh-install routing redirects a rootless hub to /setup, so the empty
	// state below is only reachable with at least one root registered.
	if _, err := service.AddLibraryRoot(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, marker := range []string{
		`暂无素材。先在<a href="/setup"`,
		`打开素材目录向导`,
		`href="/library-roots"`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("library empty state missing %q", marker)
		}
	}
}

// The saved-view select must explain itself when empty: a muted hint sits next
// to the select (hidden until loadCollections shows it), the 不使用 option
// stays in place, and a fetch failure surfaces as a warning hint instead of
// being swallowed into a silent no-op.
func TestLibraryPageCollectionsEmptyStateHasGuidance(t *testing.T) {
	page := libraryIndexHTML
	if !strings.Contains(page, `<p class="muted collections-hint" id="collections-hint" hidden>`) {
		t.Fatalf("library page must ship a hidden collections hint next to the select")
	}
	if !strings.Contains(page, `还没有动态视图。在搜索结果页可以把筛选保存为动态视图。`) {
		t.Fatalf("library page collections hint copy missing")
	}
	if !strings.Contains(page, `<option value="">不使用</option>`) {
		t.Fatalf("the 不使用 option must stay in place when no collections exist")
	}
	if !strings.Contains(page, `collectionsHint(views.length?'':'还没有动态视图。在搜索结果页可以把筛选保存为动态视图。',false)`) {
		t.Fatalf("loadCollections() must show the hint when the fetched list is empty")
	}
	if !strings.Contains(page, `collectionsHint('无法加载动态视图，请稍后再试。',true)`) {
		t.Fatalf("loadCollections() must surface a fetch failure as a warning hint")
	}
}

// TestLibraryPageSearchIsShotFirst pins the shot-first search: search()
// must ask the structured v2 shot endpoint (the same evidence-bearing
// retrieval the v2 golden benchmark measures), forward the facets in the
// body, and render shot-level result cards carrying the shot's own evidence
// — filename, time range, description, score, and the "为什么命中" evidence
// line — so a user lands on a concrete shot, not a whole-asset card. The
// drawer the cards open must seek the proxy to the shot's own range.
func TestLibraryPageSearchIsShotFirst(t *testing.T) {
	page := libraryIndexHTML
	if count := strings.Count(page, `id="q"`); count != 1 {
		t.Fatalf("library page renders %d search inputs with id=q, want exactly one", count)
	}
	if !strings.Contains(page, `fetch('/api/v1/search/shots',{method:'POST'`) {
		t.Fatalf("search() must POST to the structured v2 shot endpoint")
	}
	if !strings.Contains(page, `include_evidence:true`) {
		t.Fatalf("search() must request evidence")
	}
	if strings.Contains(page, `'/api/v1/search?q='+encodeURIComponent(q)`) {
		t.Fatalf("search() must not query the facet-blind asset search endpoint")
	}
	if !strings.Contains(page, `renderShotResults(data&&data.results?data.results:[])`) {
		t.Fatalf("search() must render shot-level results from the v2 response")
	}
	for _, marker := range []string{
		`data-shot-result`, // result cards are shot rows
		`shot-result-time`, // start — end rendered
		`shot-result-file`, // owning asset filename rendered
		`s.filename`,       // filename comes from the joined asset
		`shotEvidence(s)`,  // the evidence line renders per card
		`为什么命中：`,           // the evidence line is human-readable
		`未确认`,              // unknown evidence must render, not vanish
		`/api/v1/assets/'+encodeURIComponent(s.asset_id)+'/thumbnail`, // shot thumb via asset endpoint
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("shot result card missing marker %q", marker)
		}
	}
	// No-match must be honest: a clear message, not a fall-through to the
	// whole-asset listing.
	if !strings.Contains(page, `没有找到匹配的镜头`) {
		t.Fatalf("empty shot results must render a no-match message")
	}
	if strings.Contains(page, `if(ids)data=data.filter(x=>ids.includes(x.id));`) {
		t.Fatalf("load(ids) still intersects a capped card listing against search ids client-side")
	}
	if !strings.Contains(page, `ids&&!ids.length?Promise.resolve([])`) {
		t.Fatalf("load(ids) must not call /api/v1/assets with an empty ids list")
	}
}

// TestLibraryPageShotDrawerPinsProxySeek asserts the clickable-shot contract:
// every timeline block carries its own row (so the drawer needs no second
// round trip), and the drawer video seeks the proxy to the shot's exact
// range via the HTML5 fragment, playing start → end without navigation. The
// end is clamped (see TestLibraryPageDrawerClampsFragment) but still
// absolute, never a duration.
func TestLibraryPageShotDrawerPinsProxySeek(t *testing.T) {
	page := libraryIndexHTML
	for _, marker := range []string{
		`data-shot-drawer`,
		`data-shot-video`,
		`data-shot="'+encodeURIComponent(JSON.stringify(s))+'"`,
		`data-asset="'+esc(x.id)+'"`,
		`data-drawer-close`,
		`/proxy#t='+Math.floor(s/1000)+','+Math.ceil(clampEnd/1000)`, // absolute end, not duration
		`document.addEventListener('click',function(e){const block=e.target.closest('.shot')`,
		`document.addEventListener('keydown',function(e){if(e.key==='Escape')closeShotDrawer()`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("shot drawer missing marker %q", marker)
		}
	}
}

// TestLibraryPageSelectionFeatures pins the E2 selection loop: 加入收藏 on
// both the result card (stopPropagation so it never opens the drawer) and the
// drawer, the collections modal wired to the collections API with a
// token hint on 401, the drawer's 复制时间码 (clipboard with execCommand
// fallback) and 相似镜头 actions against /api/v1/shots/{id}/similar, the
// test-drive coaching on the search-empty state, and the 保存当前筛选 view
// builder next to the saved-view select.
func TestLibraryPageSelectionFeatures(t *testing.T) {
	page := libraryIndexHTML
	for _, marker := range []string{
		`加入收藏`,
		`data-add-shot onclick="event.stopPropagation();addShotToCollection(this)"`,
		`addShotToCollection(this)`,
		`/api/v1/collections',{headers:authHeaders()}`,
		`/api/v1/collections/'+encodeURIComponent(cid)+'/shots'`,
		`createAndAddCollection()`,
		`需要管理 Token：请先在左侧栏填入`,
		`复制时间码`,
		`copyTimecode(this)`,
		`navigator.clipboard.writeText`,
		`document.execCommand('copy')`,
		`fmtTimecode(start)`,
		`相似镜头`,
		`loadSimilarShots()`,
		`/api/v1/shots/'+encodeURIComponent(shotId)+'/similar'`,
		`先分析几个片段`,
		`testDriveStart`,
		`'/api/v1/test-drive'`,
		`test-drive/suggestions?assets='+ids.map(encodeURIComponent).join(',')`,
		`试试搜索`,
		`保存当前筛选`,
		`saveCurrentView()`,
		`captured_from`,
		`Object.assign(filter,filterFacets())`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("library page selection features missing marker %q", marker)
		}
	}
	// The test-drive coaching must also reach the browse-mode timeline-empty
	// state, not just the search-empty state.
	if !strings.Contains(page, `<div class="timeline-empty"><span>尚未生成镜头理解；完成分析后会显示可用时间段。</span><button class="shot-add" onclick="testDriveStart(this)">`) {
		t.Fatalf("timeline-empty state missing the test-drive coaching")
	}
}

// TestLibraryPageDrawerClampsFragment pins the clamp rule on the drawer's
// proxy fragment: the end is capped at start+60000ms and both bounds round to
// whole seconds. A huge range makes some browsers preload the whole asset and
// hang playback; sub-second precision makes seeking behave unpredictably.
func TestLibraryPageDrawerClampsFragment(t *testing.T) {
	page := libraryIndexHTML
	for _, marker := range []string{
		`Math.min(e,s+60000)`,                                  // end clamped to one minute past start
		`Math.floor(s/1000)`,                                   // start rounds down to whole seconds
		`Math.ceil(clampEnd/1000)`,                             // end rounds up to whole seconds
		`#t='+Math.floor(s/1000)+','+Math.ceil(clampEnd/1000)`, // fragment keeps its #t=
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("drawer fragment clamp missing marker %q", marker)
		}
	}
}
