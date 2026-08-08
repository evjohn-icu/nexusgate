package api

import (
	"fmt"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/normalize"
)

var libraryIndexHTML = enhanceLibraryPage(legacyLibraryIndexHTML)

// facetLabels maps a controlled-vocabulary value to the Chinese label shown
// in the facet selects. The vocabularies and this map are separate sources of
// truth; TestFacetLabelsCoverEveryVocabularyValue pins the seam so a value
// added to a vocabulary fails loudly instead of rendering as its English slug
// in a Chinese UI. Shared values (unknown, mixed, not_recommended) carry one
// label across the fields that use them.
var facetLabels = map[string]string{
	// asset_type
	"b_roll":            "空镜",
	"talking_to_camera": "对镜头讲述",
	"conversation":      "对话",
	"activity":          "活动",
	"performance":       "表演",
	"food":              "美食",
	"transport":         "交通",
	"architecture":      "建筑",
	"landscape":         "风光",
	"animal":            "动物",
	"document":          "文档",
	"screen_recording":  "录屏",
	"accidental":        "误拍",
	"other":             "其他",
	// camera_motion
	"static":    "固定",
	"pan_left":  "左摇",
	"pan_right": "右摇",
	"tilt_up":   "上摇",
	"tilt_down": "下摇",
	"forward":   "前进",
	"backward":  "后退",
	"tracking":  "跟拍",
	"orbit":     "环绕",
	"handheld":  "手持",
	// shot_size
	"extreme_wide":     "大远景",
	"wide":             "远景",
	"medium":           "中景",
	"close_up":         "近景",
	"extreme_close_up": "特写",
	// audio_type
	"silence":          "静音",
	"ambient":          "环境声",
	"speech":           "说话",
	"music":            "音乐",
	"singing":          "唱歌",
	"speech_and_music": "人声与音乐",
	"noise":            "噪声",
	// quality
	"excellent": "优秀",
	"usable":    "可用",
	"limited":   "受限",
	// usable_as
	"hook":              "开场钩子",
	"opening":           "开场",
	"establishing":      "交代镜头",
	"transition":        "转场",
	"montage":           "蒙太奇",
	"narration_support": "旁白配图",
	"character_intro":   "人物登场",
	"activity_detail":   "活动细节",
	"emotional_pause":   "情绪留白",
	"behind_the_scenes": "花絮",
	"ending":            "结尾",
	// shared by several fields
	"mixed":           "混合",
	"unknown":         "未知",
	"not_recommended": "不推荐",
}

// facetFields drives the semantic filter section markup. The values list is
// read straight from the normalize vocabularies, so a vocabulary addition
// grows the control for free; nothing here is hardcoded in the JS.
var facetFields = []struct {
	id, name, empty string
	values          []string
	multiple        bool
}{
	{id: "asset-type-select", name: "素材类型", empty: "全部类型", values: normalize.AssetTypeValues},
	// The other five filters resolve through the shot's asset (they exist only
	// in asset_analysis), so their labels say so: a close-up shot inside an
	// asset that is mostly wide is legitimately matched by 景别(素材级)=wide,
	// and nobody should read that as the shot itself being wide.
	{id: "shot-size-select", name: "景别(素材级)", empty: "全部景别", values: normalize.ShotSizeValues, multiple: true},
	{id: "camera-motion-select", name: "运镜(素材级)", empty: "全部运镜", values: normalize.MotionValues, multiple: true},
	{id: "audio-type-select", name: "音频(素材级)", empty: "全部音频", values: normalize.AudioTypeValues, multiple: true},
	{id: "quality-select", name: "质量(素材级)", empty: "全部质量", values: normalize.QualityValues, multiple: true},
	{id: "usable-as-select", name: "用途(素材级)", empty: "全部用途", values: normalize.UsableAsValues, multiple: true},
}

// pagePatch is one exact-match string replacement over the legacy constant.
// A patch whose anchor no longer occurs is a silent no-op — the page still
// builds and serves and the feature is just gone — so the slice is replayed
// by TestLibraryPagePatchesApplyInOrderAndBite, which requires every anchor
// to match exactly once at the moment it is applied.
type pagePatch struct {
	anchor      string
	replacement string
}

// libraryPagePatches assemble the v0.15 library page on top of
// legacyLibraryIndexHTML. Order is load-bearing: every later anchor sits
// inside text an earlier patch inserted. The first three are short copy
// strings that used to run through strings.NewReplacer; each occurs exactly
// once, so replace-all and replace-first coincide and the ordered slice covers
// them with the same exactly-once rule.
var libraryPagePatches = []pagePatch{
	{anchor: "FOOTAGE LIBRARY · SHOT LEVEL", replacement: "EXPIRED FOOTAGE · RECLAIMED"},
	{anchor: "素材库 · 镜头浏览", replacement: "素材回收站 · 镜头浏览"},
	{anchor: "从缩略图、素材语义到每个时间段的镜头内容，一眼看清你的素材里有什么可以用。", replacement: "把被遗忘的镜头重新放回时间轴：从缩略图、语义到准确时间段，找回仍然值得使用的素材。"},
	{anchor: `.library{display:grid;gap:13px}`,
		replacement: `html,body{overflow-x:clip}.filters,.semantic-filters{display:grid;grid-template-columns:repeat(6,minmax(0,1fr)) auto;gap:9px;margin:0 0 12px;padding:13px;background:#121f34;border:1px solid #2b3e5b;border-radius:14px}.semantic-filters{grid-template-columns:repeat(8,minmax(110px,1fr))}.filters label,.semantic-filters label{display:block;color:#93a6c6;font-size:10px;font-weight:800;letter-spacing:.06em;margin-bottom:5px}.filters input,.filters select,.semantic-filters input,.semantic-filters select{width:100%;padding:8px 9px}.filters button{align-self:end}.processing-summary{display:flex;gap:7px;flex-wrap:wrap;margin:0 0 18px}.status-pill{border:1px solid #385072;border-radius:999px;padding:5px 9px;color:#bed0ed;font-size:12px}.status-pill b{color:#fff}.group-title{margin:20px 2px 8px;color:#cbd8f0;font-size:13px;font-weight:850;letter-spacing:.02em}.search-bar{margin:0 0 12px}.search-bar input{width:100%;padding:11px 13px;border:1px solid #354965;border-radius:12px;background:#111d30;color:#fff;font:inherit}.advanced-filters{margin:0 0 12px;border:1px solid #2b3e5b;border-radius:14px;background:#121f34}.advanced-filters summary{cursor:pointer;padding:11px 13px;color:#b7c8eb;font-weight:800;font-size:13px;user-select:none}.advanced-filters .advanced-grid{grid-template-columns:repeat(8,minmax(0,1fr));gap:9px;padding:0 13px 13px}.advanced-filters:not([open]) .advanced-grid{display:none}.advanced-filters[open] .advanced-grid{display:grid}.advanced-filters label{display:block;color:#93a6c6;font-size:10px;font-weight:800;letter-spacing:.06em;margin-bottom:5px}.advanced-filters input,.advanced-filters select{width:100%;padding:8px 9px}.shot-results-head{display:flex;align-items:center;justify-content:space-between;gap:14px;margin:0 0 14px}.link-button{background:none;border:0;color:#93aaff;font-weight:800;cursor:pointer;padding:0}.shot-result{display:grid;grid-template-columns:200px 1fr;gap:14px;align-items:start;padding:13px;background:#121f34;border:1px solid #2b3e5b;border-radius:16px;cursor:pointer;margin:0 0 11px;transition:border-color .15s}.shot-result:hover{border-color:#506e9d}.shot-result .thumb{width:100%;aspect-ratio:16/9;object-fit:cover;border-radius:10px;background:#070d17;min-height:0}.shot-result-head{display:flex;align-items:baseline;gap:10px;flex-wrap:wrap}.shot-result-file{color:#fff;font-size:14px}.shot-result-time{color:#b7c8eb;font-size:13px;font-weight:700}.shot-result-desc{margin:8px 0;color:#d6e1f7;line-height:1.55}.shot-drawer{position:fixed;inset:0;z-index:40;display:none;pointer-events:none}.shot-drawer.open{display:block;pointer-events:auto}.shot-drawer-backdrop{position:absolute;inset:0;background:#04070dee}.shot-drawer-panel{position:absolute;top:0;right:0;bottom:0;width:min(520px,94vw);background:#101b2e;border-left:1px solid #2b3e5b;display:flex;flex-direction:column}.shot-drawer.open .shot-drawer-panel{animation:shotDrawerIn .18s ease}@keyframes shotDrawerIn{from{transform:translateX(100%)}to{transform:translateX(0)}}.shot-drawer-head{display:flex;justify-content:flex-end;padding:12px 14px;border-bottom:1px solid #273750}.shot-drawer-close{background:#2b3e5b;color:#fff;border:0;border-radius:9px;width:34px;height:34px;font-size:17px;cursor:pointer}.shot-drawer-body{overflow:auto;padding:16px}.shot-drawer-body video{width:100%;border-radius:12px;background:#070d17}.shot-drawer-title{font-size:16px;font-weight:850;color:#fff;margin:14px 0 4px}.shot-drawer-time{color:#93aaff;font-weight:800;font-size:13px}.shot-drawer-desc{color:#d6e1f7;line-height:1.6;margin:10px 0}.shot-drawer-fields{margin-top:10px;display:grid;gap:4px;color:#9fb0ce;font-size:12px}.library{display:grid;gap:13px}`},
	{anchor: `<section id="library" class="library"`,
		replacement: filterPanelsHTML() + searchBarHTML() + shotDrawerHTML() + `<section id="library" class="library"`},
	{anchor: `async function load(ids)`,
		replacement: `let activeCollection='';function filterQuery(){const values={date_from:document.getElementById('date-from').value,date_to:document.getElementById('date-to').value,region:document.getElementById('region-filter').value.trim(),camera:document.getElementById('camera-filter').value,session:document.getElementById('session-filter').value,status:document.getElementById('status-filter').value};const query=new URLSearchParams();Object.entries(values).forEach(([key,value])=>{if(value)query.set(key,value)});[['asset-type-select','asset_type']].forEach(([id,param])=>{const value=document.getElementById(id).value;if(value)query.set(param,value)});[['shot-size-select','asset_shot_size'],['camera-motion-select','asset_camera_motion'],['audio-type-select','asset_audio_type'],['quality-select','asset_quality'],['usable-as-select','asset_usable_as']].forEach(([id,param])=>{const sel=document.getElementById(id);const vals=Array.from(sel.selectedOptions).map(o=>o.value).filter(Boolean);if(vals.length)query.set(param,vals.join(','))});[['min-duration','min_duration_ms'],['max-duration','max_duration_ms']].forEach(([id,param])=>{const raw=document.getElementById(id).value.trim();if(raw==='')return;const sec=Number(raw);if(!Number.isFinite(sec)||sec<0)return;query.set(param,String(Math.round(sec*1000)))});return query.toString()?'&'+query.toString():''}function clearFilters(){activeCollection='';['date-from','date-to','region-filter','camera-filter'].forEach(id=>document.getElementById(id).value='');document.getElementById('session-filter').value='';document.getElementById('status-filter').value='';document.getElementById('collection-filter').value='';document.getElementById('asset-type-select').value='';['shot-size-select','camera-motion-select','audio-type-select','quality-select','usable-as-select'].forEach(id=>document.getElementById(id).selectedIndex=-1);document.getElementById('min-duration').value='';document.getElementById('max-duration').value='';load()}function groupKey(x){const date=x.captured_at?String(x.captured_at).slice(0,10):'日期未知';return [date,x.region_label||'地区未知',x.camera_model||'相机未知',x.session_id||'未归类场次'].join(' · ')}async function loadSessions(){const select=document.getElementById('session-filter');try{const response=await fetch('/api/v1/shoot-sessions?limit=500');if(!response.ok)throw Error('sessions unavailable');const sessions=await response.json();(Array.isArray(sessions)?sessions:[]).forEach(s=>{const option=document.createElement('option');option.value=s.id;const date=s.starts_at?String(s.starts_at).slice(0,10):'日期未知';const details=[date,s.camera_label,s.region_label].filter(Boolean).join(' · ');option.textContent=(s.title||s.id)+(details?' · '+details:'');select.appendChild(option)})}catch(_){select.innerHTML='<option value="">场次列表暂不可用</option>'}}async function loadCollections(){const select=document.getElementById('collection-filter');try{const items=await fetch('/api/v1/collections').then(r=>r.ok?r.json():[]);items.forEach(item=>{const option=document.createElement('option');option.value=item.id;option.textContent=item.name;select.appendChild(option)})}catch(_){}}async function loadSummary(){try{const data=await fetch('/api/v1/library/processing-summary?limit=300'+filterQuery()).then(r=>r.ok?r.json():null);if(!data)return;const labels={ready:'可用',processing:'处理中',queued:'等待中',failed:'失败',discovered:'未处理',missing:'原片缺失'};document.getElementById('processing-summary').innerHTML=Object.entries(labels).map(([key,label])=>'<span class="status-pill">'+label+' <b>'+esc((data.by_status||{})[key]||0)+'</b></span>').join('')}catch(_){}}async function loadCollection(id){activeCollection=id||'';load()}function loadLibrary(){loadSessions();loadCollections();load()}async function load(ids)`},
	// The pre-facet page fetched up to 300 cards and, when ids came from a
	// search, intersected them client-side (see the anchor removed just
	// below) — two independently capped windows whose overlap silently
	// shrank on a large library. Search hits are now server-facet-matched
	// (see the search() patch below), so this fetches those exact ids
	// instead of re-deriving the intersection from a second capped listing.
	// ids&&!ids.length (a search that matched nothing) skips the request
	// rather than sending ids= empty, which /api/v1/assets reads as "unset"
	// (see parseAssetIDs) and would silently show the whole library instead
	// of the true empty result. parseFacetFilter/parseAssetIDs answer a bad
	// facet or an over-cap id list with a 400 naming the problem; the old
	// chain mapped any non-ok response to [] and the empty state read as
	// "you have no footage" — the exact failure the 400 was written to
	// prevent — so this throws with the server's message instead, same as
	// the existing catch already renders.
	{anchor: `fetch('/api/v1/assets?limit=300').then(r=>r.ok?r.json():[])`,
		replacement: `(ids&&!ids.length?Promise.resolve([]):fetch(ids?'/api/v1/assets?ids='+ids.map(encodeURIComponent).join(','):(activeCollection?'/api/v1/collections/'+encodeURIComponent(activeCollection)+'/assets?limit=300':'/api/v1/assets?limit=300'+filterQuery())).then(async r=>{if(!r.ok)throw Error(await r.text());return r.json()}))`},
	// The fetch above already returns exactly the requested ids (or [] before
	// it is even called), so re-filtering the result by ids here would only
	// be the client-side intersection this task removes, now redundant
	// instead of load-bearing. Deleting it keeps that anti-pattern from
	// reappearing if the fetch above is ever loosened.
	{anchor: `if(ids)data=data.filter(x=>ids.includes(x.id));`, replacement: ``},
	{anchor: `const rows=await Promise.all(data.map(async x=>row(x,await loadShots(x.id))));library.innerHTML=rows.join('')`,
		replacement: `await loadSummary();const groups=new Map();data.forEach(x=>{const key=groupKey(x);if(!groups.has(key))groups.set(key,[]);groups.get(key).push(x)});const sections=[];for(const [key,items] of groups){const rows=await Promise.all(items.map(async x=>row(x,await loadShots(x.id))));sections.push('<div class="group-title">'+esc(key)+'</div>'+rows.join(''))}library.innerHTML=sections.join('')`},
	{anchor: `@media(max-width:720px){`,
		replacement: `@media(max-width:980px){.filters{grid-template-columns:repeat(3,minmax(0,1fr))}.semantic-filters{grid-template-columns:repeat(4,minmax(0,1fr))}.advanced-filters .advanced-grid{grid-template-columns:repeat(4,minmax(0,1fr))}}@media(max-width:720px){.filters{grid-template-columns:repeat(2,minmax(0,1fr))}.semantic-filters{grid-template-columns:repeat(2,minmax(0,1fr))}.advanced-filters .advanced-grid{grid-template-columns:repeat(2,minmax(0,1fr))}`},
	// search() sent a bare q= with no facets, so /api/v1/search — the only
	// facet-blind search endpoint before this task — narrowed nothing;
	// load(ids) then intersected its ids against an unrelated, independently
	// capped card listing (see above). Appending filterQuery() sends the same
	// facets the card listing already uses, and the 400 handling matches the
	// Shot-first search. The pre-facet search() fetched asset ids from the
	// facet-blind /api/v1/search endpoint and re-listed whole assets; the
	// product search must return SHOTS, each with its own evidence, time
	// range and score, from the same hybrid retrieval the golden set
	// measures. Browse mode (empty query) still loads asset cards.
	{anchor: `async function search(){const q=document.getElementById('q').value.trim();if(!q)return load();try{const ids=await fetch('/api/v1/search?q='+encodeURIComponent(q)).then(r=>r.ok?r.json():[]);load(Array.isArray(ids)?ids:[])}catch(e){library.innerHTML='<div class="empty error">搜索失败：'+esc(e.message)+'</div>'}}`,
		replacement: `async function search(){const q=document.getElementById('q').value.trim();if(!q)return load();try{const data=await fetch('/api/v1/search/shots/hybrid?q='+encodeURIComponent(q)+'&limit=40'+filterQuery()).then(async r=>{if(!r.ok)throw Error(await r.text());return r.json()});renderShotResults(Array.isArray(data)?data:[])}catch(e){library.innerHTML='<div class="empty error">搜索失败：'+esc(e.message)+'</div>'}}`},
	// The legacy script binds Enter on #q itself; the search bar now carries
	// the onkeydown attribute, so the duplicate listener would fire search()
	// twice per Enter.
	{anchor: `document.getElementById('q').addEventListener('keydown',e=>{if(e.key==='Enter')search()});`, replacement: ``},
	// Timeline blocks become clickable shot evidence: each carries its own
	// row (the same payload the drawer opens with) so the drawer never needs
	// a second round trip.
	{anchor: `title="'+esc(title)+'"><span class="shot-label">'+esc(description)+'</span></div>'`,
		replacement: `data-shot="'+esc(JSON.stringify(s))+'" data-asset="'+esc(x.id)+'" data-filename="'+esc(x.filename||'')+'" title="'+esc(title)+'"><span class="shot-label">'+esc(description)+'</span></div>'`},
	// The drawer script lives inside the page's one <script> block (function
	// declarations hoist, so search() can call renderShotResults and the
	// blocks' delegated click handler is wired after load()).
	{anchor: `load();</script>`, replacement: `loadLibrary();` + shotDrawerScript + `</script>`},
}

// filtersRowHTML is the high-frequency filter row: date range, asset type,
// camera, session and status, plus the saved-view selector and the reset
// button. Region and the vocabulary facets live behind 高级筛选 (see
// semanticFiltersSection) so the default view stays light; the labels of the
// six vocabulary controls keep the 素材级 suffix because those fields exist
// only in asset_analysis, and nobody should read them as shot truth.
func filtersRowHTML() string {
	return `<section class="filters" aria-label="素材筛选"><div><label for="date-from">开始日期</label><input id="date-from" type="date" onchange="load()"></div><div><label for="date-to">结束日期</label><input id="date-to" type="date" onchange="load()"></div>` +
		facetSelectHTML("asset-type-select", "素材类型", "全部类型", normalize.AssetTypeValues, false) +
		`<div><label for="camera-filter">相机</label><input id="camera-filter" placeholder="例如 Sony FX3" onchange="load()"></div><div><label for="session-filter">拍摄场次</label><select id="session-filter" onchange="load()"><option value="">全部场次</option></select></div><div><label for="status-filter">处理状态</label><select id="status-filter" onchange="load()"><option value="">全部状态</option><option value="ready">可用</option><option value="processing">处理中</option><option value="queued">等待中</option><option value="failed">失败</option><option value="discovered">未处理</option><option value="missing">原片缺失</option></select></div><div><label for="collection-filter">保存的视图</label><select id="collection-filter" onchange="loadCollection(this.value)"><option value="">不使用</option></select></div><button onclick="clearFilters()">清除筛选</button></section>`
}

// searchBarHTML is the shot-first search entry: it lives above the filters
// so a query is the first thing an editor meets, and it renders shot-level
// results (see search() and renderShotResults in the page script).
func searchBarHTML() string {
	return `<div class="search-bar"><input id="q" placeholder="搜索镜头内容、口述或标签，回车查看镜头级结果" onkeydown="if(event.key==='Enter')search()" aria-label="镜头搜索"></div>`
}

// filterPanelsHTML is the replacement for the <section id="library"> anchor:
// the search bar, both filter rows and the processing summary. The facet
// options are generated from the normalize vocabularies, never typed into
// the JS.
func filterPanelsHTML() string {
	return searchBarHTML() + filtersRowHTML() + semanticFiltersSection() + `<div id="processing-summary" class="processing-summary" aria-label="处理状态汇总"></div>`
}

// semanticFiltersSection is the 高级筛选 details block: region plus the
// five vocabulary facets (asset type lives in the top row) and the duration
// bounds. Collapsed by default so the frequent conditions stay prominent.
func semanticFiltersSection() string {
	var b strings.Builder
	b.WriteString(`<details class="advanced-filters" aria-label="高级筛选"><summary>高级筛选</summary><div class="advanced-grid">`)
	b.WriteString(`<div><label for="region-filter">地区</label><input id="region-filter" placeholder="例如 中国 · 深圳 · 南山" onchange="load()"></div>`)
	for _, f := range facetFields[1:] {
		b.WriteString(facetSelectHTML(f.id, f.name, f.empty, f.values, f.multiple))
	}
	// Duration is entered in seconds, the unit an editor thinks in, and the JS
	// converts to milliseconds before sending; empty means unset.
	b.WriteString(`<div><label for="min-duration">最短时长（秒）</label><input id="min-duration" type="number" min="0" step="any" inputmode="numeric" placeholder="不限" onchange="load()"></div>`)
	b.WriteString(`<div><label for="max-duration">最长时长（秒）</label><input id="max-duration" type="number" min="0" step="any" inputmode="numeric" placeholder="不限" onchange="load()"></div>`)
	b.WriteString(`</div></details>`)
	return b.String()
}

func facetSelectHTML(id, name, empty string, values []string, multiple bool) string {
	var b strings.Builder
	b.WriteString(`<div><label for="` + id + `">` + name + `</label><select id="` + id + `" onchange="load()"`)
	if multiple {
		b.WriteString(` multiple size="4"`)
	}
	b.WriteString(`>`)
	if !multiple {
		b.WriteString(`<option value="">` + empty + `</option>`)
	}
	for _, v := range values {
		fmt.Fprintf(&b, `<option value="%s">%s</option>`, v, facetLabels[v])
	}
	b.WriteString(`</select></div>`)
	return b.String()
}

// shotDrawerHTML is the overlay that turns a timeline shot into something an
// editor can actually watch: the proxy seeks to the shot's start and plays
// its range, while the shot's own evidence (description, objects, actions,
// tags, mood) is shown beside it. It is an overlay, never a navigation, so
// the query, filters and scroll position survive.
func shotDrawerHTML() string {
	return `<div class="shot-drawer" id="shot-drawer" data-shot-drawer aria-hidden="true"><div class="shot-drawer-backdrop" data-drawer-close></div><aside class="shot-drawer-panel" aria-label="镜头预览"><div class="shot-drawer-head"><button class="shot-drawer-close" data-drawer-close aria-label="关闭预览">×</button></div><div class="shot-drawer-body"><video id="shot-video" data-shot-video controls autoplay playsinline></video><div class="shot-drawer-title" id="shot-drawer-title">—</div><div class="shot-drawer-time" id="shot-drawer-time">—</div><p class="shot-drawer-desc" id="shot-drawer-desc">—</p><div class="chips" id="shot-drawer-chips"></div><div class="shot-drawer-fields" id="shot-drawer-fields"></div></div></aside></div>`
}

// shotDrawerScript is appended inside the page's single script block. It
// wires the timeline blocks and the shot-result cards to the drawer and
// renders the shot-first result list (search() calls renderShotResults).
const shotDrawerScript = `
function shotChips(shot){const chips=[];['tags','objects','actions','mood'].forEach(k=>{(shot[k]||[]).forEach(v=>chips.push('<span class="chip">'+esc(v)+'</span>'))});return chips.join('')}
function shotResultCard(s){const score=Math.round((s.score||0)*100);return '<article class="shot-result" data-shot-result data-shot="'+esc(JSON.stringify(s))+'" data-asset="'+esc(s.asset_id)+'" data-filename="'+esc(s.filename||'')+'" onclick="openShotDrawer(JSON.parse(this.dataset.shot),this.dataset.asset,this.dataset.filename)"><img class="thumb" loading="lazy" src="/api/v1/assets/'+encodeURIComponent(s.asset_id)+'/thumbnail" alt="镜头缩略图" onerror="this.style.visibility=\'hidden\'"><div class="shot-result-main"><div class="shot-result-head"><b class="shot-result-file">'+esc(s.filename||s.asset_id)+'</b><span class="shot-result-time">'+fmt(s.start_ms)+' — '+fmt(s.end_ms)+'</span><span class="status-pill">匹配 '+score+'%</span></div><p class="shot-result-desc">'+esc(s.description||'')+'</p><div class="chips">'+shotChips(s)+'</div></div></article>'}
function renderShotResults(shots){if(!shots.length){library.innerHTML='<div class="empty">没有找到匹配的镜头。换个说法，或减少筛选条件后再试。</div>';return}library.innerHTML='<div class="shot-results-head"><span class="eyebrow">镜头级搜索结果</span><button class="link-button" onclick="load()">返回素材浏览</button></div>'+shots.map(shotResultCard).join('')}
function openShotDrawer(shot,assetId,filename){const d=document.getElementById('shot-drawer');if(!d)return;document.getElementById('shot-drawer-title').textContent=filename||assetId||'—';const s=Number(shot.start_ms)||0,e=Number(shot.end_ms)||s;document.getElementById('shot-drawer-time').textContent=fmt(s)+' — '+fmt(e)+' · 时长 '+fmt(e-s);document.getElementById('shot-drawer-desc').textContent=shot.description||'（该镜头没有描述）';document.getElementById('shot-drawer-chips').innerHTML=shotChips(shot);const fields=[];if(shot.confidence!=null)fields.push('<span><b>置信度</b> '+esc(String(shot.confidence))+'</span>');document.getElementById('shot-drawer-fields').innerHTML=fields.join('');const v=document.getElementById('shot-video');v.src='/api/v1/assets/'+encodeURIComponent(assetId)+'/proxy#t='+Math.floor(s/1000)+','+Math.ceil(e/1000);d.classList.add('open');d.setAttribute('aria-hidden','false');v.play().catch(function(){})}
function closeShotDrawer(){const d=document.getElementById('shot-drawer');if(!d)return;d.classList.remove('open');d.setAttribute('aria-hidden','true');const v=document.getElementById('shot-video');if(v){v.pause();v.removeAttribute('src')}}
function openShotDrawerInit(){document.addEventListener('click',function(e){const block=e.target.closest('.shot');if(!block)return;const shot=JSON.parse(block.dataset.shot||'{}');openShotDrawer(shot,block.dataset.asset||'',block.dataset.filename||'')});document.querySelectorAll('[data-drawer-close]').forEach(function(el){el.addEventListener('click',closeShotDrawer)});document.addEventListener('keydown',function(e){if(e.key==='Escape')closeShotDrawer()})}
openShotDrawerInit();`

func enhanceLibraryPage(page string) string {
	for _, p := range libraryPagePatches {
		page = strings.Replace(page, p.anchor, p.replacement, 1)
	}
	return page
}
