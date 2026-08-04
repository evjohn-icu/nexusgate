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
}{
	{id: "asset-type-select", name: "素材类型", empty: "全部类型", values: normalize.AssetTypeValues},
	{id: "shot-size-select", name: "景别", empty: "全部景别", values: normalize.ShotSizeValues},
	{id: "camera-motion-select", name: "运镜", empty: "全部运镜", values: normalize.MotionValues},
	{id: "audio-type-select", name: "音频", empty: "全部音频", values: normalize.AudioTypeValues},
	{id: "quality-select", name: "质量", empty: "全部质量", values: normalize.QualityValues},
	{id: "usable-as-select", name: "用途", empty: "全部用途", values: normalize.UsableAsValues},
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
		replacement: `.filters,.semantic-filters{display:grid;grid-template-columns:repeat(6,minmax(120px,1fr)) auto;gap:9px;margin:0 0 12px;padding:13px;background:#121f34;border:1px solid #2b3e5b;border-radius:14px}.semantic-filters{grid-template-columns:repeat(8,minmax(110px,1fr))}.filters label,.semantic-filters label{display:block;color:#93a6c6;font-size:10px;font-weight:800;letter-spacing:.06em;margin-bottom:5px}.filters input,.filters select,.semantic-filters input,.semantic-filters select{width:100%;padding:8px 9px}.filters button{align-self:end}.processing-summary{display:flex;gap:7px;flex-wrap:wrap;margin:0 0 18px}.status-pill{border:1px solid #385072;border-radius:999px;padding:5px 9px;color:#bed0ed;font-size:12px}.status-pill b{color:#fff}.group-title{margin:20px 2px 8px;color:#cbd8f0;font-size:13px;font-weight:850;letter-spacing:.02em}.library{display:grid;gap:13px}`},
	{anchor: `<section id="library" class="library"`,
		replacement: filterPanelsHTML() + `<section id="library" class="library"`},
	{anchor: `async function load(ids)`,
		replacement: `let activeCollection='';function filterQuery(){const values={date_from:document.getElementById('date-from').value,date_to:document.getElementById('date-to').value,region:document.getElementById('region-filter').value.trim(),camera:document.getElementById('camera-filter').value,session:document.getElementById('session-filter').value,status:document.getElementById('status-filter').value};const query=new URLSearchParams();Object.entries(values).forEach(([key,value])=>{if(value)query.set(key,value)});[['asset-type-select','asset_type'],['shot-size-select','shot_size'],['camera-motion-select','camera_motion'],['audio-type-select','audio_type'],['quality-select','quality'],['usable-as-select','usable_as']].forEach(([id,param])=>{const value=document.getElementById(id).value;if(value)query.set(param,value)});[['min-duration','min_duration_ms'],['max-duration','max_duration_ms']].forEach(([id,param])=>{const raw=document.getElementById(id).value.trim();if(raw==='')return;const sec=Number(raw);if(!Number.isInteger(sec)||sec<0)return;query.set(param,String(sec*1000))});return query.toString()?'&'+query.toString():''}function clearFilters(){activeCollection='';['date-from','date-to','region-filter','camera-filter'].forEach(id=>document.getElementById(id).value='');document.getElementById('session-filter').value='';document.getElementById('status-filter').value='';document.getElementById('collection-filter').value='';document.getElementById('asset-type-select').value='';document.getElementById('shot-size-select').value='';document.getElementById('camera-motion-select').value='';document.getElementById('audio-type-select').value='';document.getElementById('quality-select').value='';document.getElementById('usable-as-select').value='';document.getElementById('min-duration').value='';document.getElementById('max-duration').value='';load()}function groupKey(x){const date=x.captured_at?String(x.captured_at).slice(0,10):'日期未知';return [date,x.region_label||'地区未知',x.camera_model||'相机未知',x.session_id||'未归类场次'].join(' · ')}async function loadSessions(){const select=document.getElementById('session-filter');try{const response=await fetch('/api/v1/shoot-sessions?limit=500');if(!response.ok)throw Error('sessions unavailable');const sessions=await response.json();(Array.isArray(sessions)?sessions:[]).forEach(s=>{const option=document.createElement('option');option.value=s.id;const date=s.starts_at?String(s.starts_at).slice(0,10):'日期未知';const details=[date,s.camera_label,s.region_label].filter(Boolean).join(' · ');option.textContent=(s.title||s.id)+(details?' · '+details:'');select.appendChild(option)})}catch(_){select.innerHTML='<option value="">场次列表暂不可用</option>'}}async function loadCollections(){const select=document.getElementById('collection-filter');try{const items=await fetch('/api/v1/collections').then(r=>r.ok?r.json():[]);items.forEach(item=>{const option=document.createElement('option');option.value=item.id;option.textContent=item.name;select.appendChild(option)})}catch(_){}}async function loadSummary(){try{const data=await fetch('/api/v1/library/processing-summary?limit=300'+filterQuery()).then(r=>r.ok?r.json():null);if(!data)return;const labels={ready:'可用',processing:'处理中',queued:'等待中',failed:'失败',discovered:'未处理',missing:'原片缺失'};document.getElementById('processing-summary').innerHTML=Object.entries(labels).map(([key,label])=>'<span class="status-pill">'+label+' <b>'+esc((data.by_status||{})[key]||0)+'</b></span>').join('')}catch(_){}}async function loadCollection(id){activeCollection=id||'';load()}function loadLibrary(){loadSessions();loadCollections();load()}async function load(ids)`},
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
		replacement: `@media(max-width:980px){.filters{grid-template-columns:repeat(3,1fr)}.semantic-filters{grid-template-columns:repeat(4,1fr)}}@media(max-width:720px){.filters{grid-template-columns:1fr 1fr}.semantic-filters{grid-template-columns:1fr 1fr}`},
	{anchor: `load();</script>`, replacement: `loadLibrary();</script>`},
	// search() sent a bare q= with no facets, so /api/v1/search — the only
	// facet-blind search endpoint before this task — narrowed nothing;
	// load(ids) then intersected its ids against an unrelated, independently
	// capped card listing (see above). Appending filterQuery() sends the same
	// facets the card listing already uses, and the 400 handling matches the
	// pattern above and the fetch patch's comment.
	{anchor: `'/api/v1/search?q='+encodeURIComponent(q)).then(r=>r.ok?r.json():[])`,
		replacement: `'/api/v1/search?q='+encodeURIComponent(q)+filterQuery()).then(async r=>{if(!r.ok)throw Error(await r.text());return r.json()})`},
}

// filtersRowHTML is the pre-existing date/region/camera/session/status/
// collection row. It is unchanged; the semantic row is appended below it.
const filtersRowHTML = `<section class="filters" aria-label="素材筛选"><div><label for="date-from">开始日期</label><input id="date-from" type="date" onchange="load()"></div><div><label for="date-to">结束日期</label><input id="date-to" type="date" onchange="load()"></div><div><label for="region-filter">地区</label><input id="region-filter" placeholder="例如 中国 · 深圳 · 南山" onchange="load()"></div><div><label for="camera-filter">相机</label><input id="camera-filter" placeholder="例如 Sony FX3" onchange="load()"></div><div><label for="session-filter">拍摄场次</label><select id="session-filter" onchange="load()"><option value="">全部场次</option></select></div><div><label for="status-filter">处理状态</label><select id="status-filter" onchange="load()"><option value="">全部状态</option><option value="ready">可用</option><option value="processing">处理中</option><option value="queued">等待中</option><option value="failed">失败</option><option value="discovered">未处理</option><option value="missing">原片缺失</option></select></div><div><label for="collection-filter">保存的视图</label><select id="collection-filter" onchange="loadCollection(this.value)"><option value="">不使用</option></select></div><button onclick="clearFilters()">清除筛选</button></section>`

// filterPanelsHTML is the replacement for the <section id="library"> anchor:
// both filter rows followed by the processing summary. The facet options are
// generated from the normalize vocabularies, never typed into the JS.
func filterPanelsHTML() string {
	return filtersRowHTML + semanticFiltersSection() + `<div id="processing-summary" class="processing-summary" aria-label="处理状态汇总"></div>`
}

func semanticFiltersSection() string {
	var b strings.Builder
	b.WriteString(`<section class="semantic-filters" aria-label="语义筛选">`)
	for _, f := range facetFields {
		b.WriteString(facetSelectHTML(f.id, f.name, f.empty, f.values))
	}
	// Duration is entered in seconds, the unit an editor thinks in, and the JS
	// converts to milliseconds before sending; empty means unset.
	b.WriteString(`<div><label for="min-duration">最短时长（秒）</label><input id="min-duration" type="number" min="0" step="1" inputmode="numeric" placeholder="不限" onchange="load()"></div>`)
	b.WriteString(`<div><label for="max-duration">最长时长（秒）</label><input id="max-duration" type="number" min="0" step="1" inputmode="numeric" placeholder="不限" onchange="load()"></div>`)
	b.WriteString(`</section>`)
	return b.String()
}

func facetSelectHTML(id, name, empty string, values []string) string {
	var b strings.Builder
	b.WriteString(`<div><label for="` + id + `">` + name + `</label><select id="` + id + `" onchange="load()"><option value="">` + empty + `</option>`)
	for _, v := range values {
		fmt.Fprintf(&b, `<option value="%s">%s</option>`, v, facetLabels[v])
	}
	b.WriteString(`</select></div>`)
	return b.String()
}

func enhanceLibraryPage(page string) string {
	for _, p := range libraryPagePatches {
		page = strings.Replace(page, p.anchor, p.replacement, 1)
	}
	return page
}
