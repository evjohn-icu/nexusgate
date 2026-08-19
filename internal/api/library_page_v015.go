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
		replacement: `html,body{overflow-x:clip}.filters,.semantic-filters{display:grid;grid-template-columns:repeat(6,minmax(0,1fr)) auto;gap:9px;margin:0 0 12px;padding:13px;background:#121f34;border:1px solid #2b3e5b;border-radius:14px}.semantic-filters{grid-template-columns:repeat(8,minmax(110px,1fr))}.filters label,.semantic-filters label{display:block;color:#93a6c6;font-size:10px;font-weight:800;letter-spacing:.06em;margin-bottom:5px}.filters input,.filters select,.semantic-filters input,.semantic-filters select{width:100%;padding:8px 9px}.filters button{align-self:end}.filters .collections-hint{margin:6px 0 0;font-size:11px;line-height:1.5}.filters .collections-hint.warn{color:#ffc0ca}.processing-summary{display:flex;gap:7px;flex-wrap:wrap;margin:0 0 18px}.status-pill{border:1px solid #385072;border-radius:999px;padding:5px 9px;color:#bed0ed;font-size:12px}.status-pill b{color:#fff}.group-title{margin:20px 2px 8px;color:#cbd8f0;font-size:13px;font-weight:850;letter-spacing:.02em}.search-bar{margin:0 0 12px}.search-bar input{width:100%;padding:11px 13px;border:1px solid #354965;border-radius:12px;background:#111d30;color:#fff;font:inherit}.advanced-filters{margin:0 0 12px;border:1px solid #2b3e5b;border-radius:14px;background:#121f34}.advanced-filters summary{cursor:pointer;padding:11px 13px;color:#b7c8eb;font-weight:800;font-size:13px;user-select:none}.advanced-filters .advanced-grid{grid-template-columns:repeat(8,minmax(0,1fr));gap:9px;padding:0 13px 13px}.advanced-filters:not([open]) .advanced-grid{display:none}.advanced-filters[open] .advanced-grid{display:grid}.advanced-filters label{display:block;color:#93a6c6;font-size:10px;font-weight:800;letter-spacing:.06em;margin-bottom:5px}.advanced-filters input,.advanced-filters select{width:100%;padding:8px 9px}.shot-results-head{display:flex;align-items:center;justify-content:space-between;gap:14px;margin:0 0 14px}.link-button{background:none;border:0;color:#93aaff;font-weight:800;cursor:pointer;padding:0}.shot-result{display:grid;grid-template-columns:200px 1fr;gap:14px;align-items:start;padding:13px;background:#121f34;border:1px solid #2b3e5b;border-radius:16px;cursor:pointer;margin:0 0 11px;transition:border-color .15s}.shot-result:hover{border-color:#506e9d}.shot-result .thumb{width:100%;aspect-ratio:16/9;object-fit:cover;border-radius:10px;background:#070d17;min-height:0}.shot-result-head{display:flex;align-items:baseline;gap:10px;flex-wrap:wrap}.shot-result-file{color:#fff;font-size:14px}.shot-result-time{color:#b7c8eb;font-size:13px;font-weight:700}.shot-result-desc{margin:8px 0;color:#d6e1f7;line-height:1.55}.shot-evidence{margin:6px 0 8px;padding:8px 10px;background:#0c1526;border:1px solid #26395a;border-radius:10px;color:#b7c8eb;font-size:12px;line-height:1.7}.shot-evidence .ev-confirmed{color:#6fe3a1}.shot-evidence .ev-possible{color:#f0c674}.shot-evidence .ev-contradicted{color:#ff7b72}.shot-evidence .ev-unknown{color:#8fa3c4}.shot-drawer{position:fixed;inset:0;z-index:40;display:none;pointer-events:none}.shot-drawer.open{display:block;pointer-events:auto}.shot-drawer-backdrop{position:absolute;inset:0;background:#04070dee}.shot-drawer-panel{position:absolute;top:0;right:0;bottom:0;width:min(520px,94vw);background:#101b2e;border-left:1px solid #2b3e5b;display:flex;flex-direction:column}.shot-drawer.open .shot-drawer-panel{animation:shotDrawerIn .18s ease}@keyframes shotDrawerIn{from{transform:translateX(100%)}to{transform:translateX(0)}}.shot-drawer-head{display:flex;justify-content:flex-end;padding:12px 14px;border-bottom:1px solid #273750}.shot-drawer-close{background:#2b3e5b;color:#fff;border:0;border-radius:9px;width:34px;height:34px;font-size:17px;cursor:pointer}.shot-drawer-body{overflow:auto;padding:16px}.shot-drawer-body video{width:100%;border-radius:12px;background:#070d17}.shot-drawer-title{font-size:16px;font-weight:850;color:#fff;margin:14px 0 4px}.shot-drawer-time{color:#93aaff;font-weight:800;font-size:13px}.shot-drawer-desc{color:#d6e1f7;line-height:1.6;margin:10px 0}.shot-drawer-fields{margin-top:10px;display:grid;gap:4px;color:#9fb0ce;font-size:12px}.root-health{display:grid;gap:8px;margin:0 0 18px}.root-health-warn{display:flex;align-items:center;gap:9px;padding:10px 13px;background:#3a2230;border:1px solid #6e3a4a;border-radius:12px;color:#ffc0c8;font-size:13px;font-weight:700}.library{display:grid;gap:13px}`},
	// Selection affordances (加入收藏, drawer actions, test drive, saved-view
	// button) ride on the same sheet as the filter panels; keeping them here
	// keeps the whole page's look in one place instead of scattering inline
	// styles through the script. .shot-add is also reused inside the result
	// cards and the timeline-empty state, so its size must stay compact.
	{anchor: `.root-health-warn{display:flex;align-items:center;gap:9px;padding:10px 13px;background:#3a2230;border:1px solid #6e3a4a;border-radius:12px;color:#ffc0c8;font-size:13px;font-weight:700}.library{display:grid;gap:13px}`,
		replacement: `.root-health-warn{display:flex;align-items:center;gap:9px;padding:10px 13px;background:#3a2230;border:1px solid #6e3a4a;border-radius:12px;color:#ffc0c8;font-size:13px;font-weight:700}.shot-result-foot{display:flex;justify-content:flex-end;padding:10px 13px 4px}.shot-add{background:#233451;color:#b8c9ff;border:1px solid #385072;border-radius:999px;padding:4px 11px;font-size:12px;font-weight:800;cursor:pointer}.shot-add:hover{background:#2c4060}.shot-add.ok{color:#9cf0c2;border-color:#2e6b4f}.shot-add:disabled{opacity:.5;cursor:default}.test-drive-box{margin:14px 0 0;padding:14px;border:1px dashed #3a4e70;border-radius:12px;background:#0f1a2e}.test-drive-box .group-title{margin-top:0}.test-drive-status{color:#9cf0c2;font-size:12px;line-height:1.6;margin-top:8px}.test-drive-status a{color:#b8c8ff}.try-chip{background:#233451;color:#b8c9ff;border:1px solid #385072;border-radius:999px;padding:5px 11px;font-size:12px;font-weight:800;cursor:pointer}.try-chip:hover{background:#2c4060}.shot-drawer-actions{display:flex;gap:8px;flex-wrap:wrap;margin:0 0 12px}.shot-drawer-similar{display:grid;gap:8px;margin-top:12px}.similar-item{border:1px solid #2b3e5b;border-radius:10px;padding:9px 11px;background:#0f1a2e;cursor:pointer;display:grid;gap:3px;font-size:12px;color:#aab8d0;line-height:1.5}.similar-item:hover{border-color:#506e9d}.similar-item b{color:#93aaff}.shot-modal{position:fixed;inset:0;z-index:60;display:none;place-items:center;background:#04070dcc}.shot-modal.open{display:grid}.shot-modal-card{width:min(440px,92vw);background:#121f34;border:1px solid #2b3e5b;border-radius:14px;padding:18px}.shot-modal-card h3{margin:0 0 4px;font-size:16px;color:#fff}.shot-modal-error{color:#ffc0ca;font-size:12px;min-height:16px;margin:8px 0}.shot-modal-list{display:grid;gap:8px;margin:10px 0}.shot-modal-row{display:flex;align-items:center;justify-content:space-between;gap:10px;padding:9px 11px;background:#0f1a2e;border:1px solid #2b3e5b;border-radius:10px;font-size:13px}.shot-modal-row .muted{font-size:11px}.shot-modal-new{display:flex;gap:8px;margin-top:10px}.shot-modal-new input{flex:1;padding:8px 9px}.library{display:grid;gap:13px}`},
	{anchor: `<section id="library" class="library"`,
		replacement: filterPanelsHTML() + shotDrawerHTML() + `<section id="library" class="library"`},
	{anchor: `async function load(ids)`,
		replacement: `async function apiErrMsg(r){try{const d=await r.json();if(d&&d.error&&d.error.message)return d.error.action?(d.error.message+'（'+d.error.action+'）'):d.error.message}catch(_){}return (await r.text()).trim()}
let activeCollection='';function filterQuery(){const values={date_from:document.getElementById('date-from').value,date_to:document.getElementById('date-to').value,region:document.getElementById('region-filter').value.trim(),camera:document.getElementById('camera-filter').value,session:document.getElementById('session-filter').value,status:document.getElementById('status-filter').value};const query=new URLSearchParams();Object.entries(values).forEach(([key,value])=>{if(value)query.set(key,value)});[['asset-type-select','asset_type']].forEach(([id,param])=>{const value=document.getElementById(id).value;if(value)query.set(param,value)});[['shot-size-select','asset_shot_size'],['camera-motion-select','asset_camera_motion'],['audio-type-select','asset_audio_type'],['quality-select','asset_quality'],['usable-as-select','asset_usable_as']].forEach(([id,param])=>{const sel=document.getElementById(id);const vals=Array.from(sel.selectedOptions).map(o=>o.value).filter(Boolean);if(vals.length)query.set(param,vals.join(','))});[['min-duration','min_duration_ms'],['max-duration','max_duration_ms']].forEach(([id,param])=>{const raw=document.getElementById(id).value.trim();if(raw==='')return;const sec=Number(raw);if(!Number.isFinite(sec)||sec<0)return;query.set(param,String(Math.round(sec*1000)))});return query.toString()?'&'+query.toString():''}function filterFacets(){const facets={};const pick=(id,key)=>{const sel=document.getElementById(id);const vals=Array.from(sel.selectedOptions).map(o=>o.value).filter(Boolean);if(vals.length)facets[key]=vals};pick('asset-type-select','asset_types');pick('shot-size-select','shot_sizes');pick('camera-motion-select','camera_motions');pick('audio-type-select','audio_types');pick('quality-select','qualities');pick('usable-as-select','usable_as');[['min-duration','min_duration_ms'],['max-duration','max_duration_ms']].forEach(([id,key])=>{const raw=document.getElementById(id).value.trim();if(raw==='')return;const sec=Number(raw);if(!Number.isFinite(sec)||sec<0)return;facets[key]=Math.round(sec*1000)});return facets}function clearFilters(){activeCollection='';['date-from','date-to','region-filter','camera-filter'].forEach(id=>document.getElementById(id).value='');document.getElementById('session-filter').value='';document.getElementById('status-filter').value='';document.getElementById('collection-filter').value='';document.getElementById('asset-type-select').value='';['shot-size-select','camera-motion-select','audio-type-select','quality-select','usable-as-select'].forEach(id=>document.getElementById(id).selectedIndex=-1);document.getElementById('min-duration').value='';document.getElementById('max-duration').value='';load()}function groupKey(x){const date=x.captured_at?String(x.captured_at).slice(0,10):'日期未知';return [date,x.region_label||'地区未知',x.camera_model||'相机未知',x.session_id||'未归类场次'].join(' · ')}async function loadSessions(){const select=document.getElementById('session-filter');try{const response=await fetch('/api/v1/shoot-sessions?limit=500');if(!response.ok)throw Error('sessions unavailable');const sessions=await response.json();(Array.isArray(sessions)?sessions:[]).forEach(s=>{const option=document.createElement('option');option.value=s.id;const date=s.starts_at?String(s.starts_at).slice(0,10):'日期未知';const details=[date,s.camera_label,s.region_label].filter(Boolean).join(' · ');option.textContent=(s.title||s.id)+(details?' · '+details:'');select.appendChild(option)})}catch(_){select.innerHTML='<option value="">场次列表暂不可用</option>'}}function collectionsHint(text,warn){const el=document.getElementById('collections-hint');if(!el)return;el.hidden=!text;el.textContent=text||'';el.classList.toggle('warn',!!warn)}async function loadCollections(){const select=document.getElementById('collection-filter');try{const r=await fetch('/api/v1/collections');if(!r.ok)throw Error('HTTP '+r.status);const items=await r.json();items.forEach(item=>{const option=document.createElement('option');option.value=item.id;option.textContent=item.name;select.appendChild(option)});collectionsHint(items.length?'':'还没有保存的视图。在搜索结果页可以把筛选保存为视图。',false)}catch(_){collectionsHint('无法加载保存的视图，请稍后再试。',true)}}async function loadSummary(){try{const data=await fetch('/api/v1/library/processing-summary?limit=300'+filterQuery()).then(r=>r.ok?r.json():null);if(!data)return;const labels={ready:'可用',processing:'处理中',queued:'等待中',failed:'失败',discovered:'未处理',missing:'原片缺失'};document.getElementById('processing-summary').innerHTML=Object.entries(labels).map(([key,label])=>'<span class="status-pill">'+label+' <b>'+esc((data.by_status||{})[key]||0)+'</b></span>').join('')}catch(_){}}function adminToken(){const el=document.getElementById('admin-token');return el?el.value.trim():''}function authHeaders(base){const headers=new Headers(base||{});const token=adminToken();if(token)headers.set('Authorization','Bearer '+token);return headers}function fmtHealthTime(v){if(!v)return'—';const t=new Date(v);return isNaN(t.getTime())?esc(v):t.toLocaleString()}// Root health rides the same load() flow as the processing summary: /api/v1/roots/health is admin-only, so without a token this fetch 401s and the strip stays empty rather than nagging. When a root is unavailable the reconciliation gate is paused server-side; this line is the library page's answer to "why is nothing marked missing".
async function loadRootHealth(){const el=document.getElementById('root-health');if(!el)return;try{const list=await fetch('/api/v1/roots/health',{headers:authHeaders()}).then(r=>r.ok?r.json():null);if(!list)return;const down=list.filter(h=>h.state==='unavailable');el.innerHTML=down.length?down.map(h=>'<div class="root-health-warn">⚠ '+esc(h.path)+' 离线 · 上次正常 '+esc(fmtHealthTime(h.last_healthy_at))+' · 暂停对账</div>').join(''):''}catch(_){}}async function loadCollection(id){activeCollection=id||'';load()}function loadLibrary(){loadSessions();loadCollections();load()}async function load(ids)`},
	// The test-drive section and the shot drawer need module-level state: the
	// ids of the last-loaded asset cards (so 开始测试分析 can pick three
	// samples without a round trip), the shot currently open in the drawer,
	// and the button that opened the collections modal (so a successful add
	// can flip that exact button to ✓ 已加入).
	{anchor: `let activeCollection='';`,
		replacement: `let activeCollection='',lastLoadedAssets=[],currentShot=null,addShotSourceBtn=null;`},
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
		replacement: `(ids&&!ids.length?Promise.resolve([]):fetch(ids?'/api/v1/assets?ids='+ids.map(encodeURIComponent).join(','):(activeCollection?'/api/v1/collections/'+encodeURIComponent(activeCollection)+'/assets?limit=300':'/api/v1/assets?limit=300'+filterQuery())).then(async r=>{if(!r.ok)throw Error(await apiErrMsg(r));return r.json()}))`},
	// The fetch above already returns exactly the requested ids (or [] before
	// it is even called), so re-filtering the result by ids here would only
	// be the client-side intersection this task removes, now redundant
	// instead of load-bearing. Deleting it keeps that anti-pattern from
	// reappearing if the fetch above is ever loosened.
	{anchor: `if(ids)data=data.filter(x=>ids.includes(x.id));`, replacement: ``},
	{anchor: `const rows=await Promise.all(data.map(async x=>row(x,await loadShots(x.id))));library.innerHTML=rows.join('')`,
		replacement: `await loadSummary();loadRootHealth();const groups=new Map();data.forEach(x=>{const key=groupKey(x);if(!groups.has(key))groups.set(key,[]);groups.get(key).push(x)});const sections=[];for(const [key,items] of groups){const rows=await Promise.all(items.map(async x=>row(x,await loadShots(x.id))));sections.push('<div class="group-title">'+esc(key)+'</div>'+rows.join(''))}library.innerHTML=sections.join('')`},
	// load() knows the ids of the asset cards it just rendered; remembering
	// them lets the search-empty and timeline-empty states offer 开始测试分析
	// without a second round trip for the sample set.
	{anchor: `await loadSummary();loadRootHealth();const groups=new Map();`,
		replacement: `await loadSummary();loadRootHealth();lastLoadedAssets=data.map(x=>x.id);const groups=new Map();`},
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
	//
	// The v2 structured endpoint carries the same facets in the body and
	// returns per-constraint evidence, which renderShotResults shows as the
	// "为什么命中" line. The GET hybrid endpoint stays for MCP and agents.
	{anchor: `async function search(){const q=document.getElementById('q').value.trim();if(!q)return load();try{const ids=await fetch('/api/v1/search?q='+encodeURIComponent(q)).then(r=>r.ok?r.json():[]);load(Array.isArray(ids)?ids:[])}catch(e){library.innerHTML='<div class="empty error">搜索失败：'+esc(e.message)+'</div>'}}`,
		replacement: `async function search(ev){if(ev&&ev.preventDefault)ev.preventDefault();const q=document.getElementById('q').value.trim();if(!q)return load();try{const data=await fetch('/api/v1/search/shots',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({query:q,mode:'auto',limit:40,diversity:0.2,include_evidence:true,include_context:false,facets:filterFacets()})}).then(async r=>{if(!r.ok)throw Error(await apiErrMsg(r));return r.json()});renderShotResults(data&&data.results?data.results:[])}catch(e){library.innerHTML='<div class="empty error">搜索失败：'+esc(e.message)+'</div>'}}
function clearSearch(){const q=document.getElementById('q');if(q)q.value='';load()}`},
	// The legacy script binds Enter on #q itself; the search bar now carries
	// the onkeydown attribute, so the duplicate listener would fire search()
	// twice per Enter.
	{anchor: `document.getElementById('q').addEventListener('keydown',e=>{if(e.key==='Enter')search()});`, replacement: ``},
	// Timeline blocks become clickable shot evidence: each carries its own
	// row (the same payload the drawer opens with) so the drawer never needs
	// a second round trip.
	{anchor: `title="'+esc(title)+'"><span class="shot-label">'+esc(description)+'</span></div>'`,
		replacement: `data-shot="'+encodeURIComponent(JSON.stringify(s))+'" data-asset="'+esc(x.id)+'" data-filename="'+esc(x.filename||'')+'" title="'+esc(title)+'"><span class="shot-label">'+esc(description)+'</span></div>'`},
	// An asset card with no shots yet is a browse-mode dead end; the same
	// test-drive coaching the search-empty state offers (below) goes here so
	// the first-run library coaches from both modes. The status and chips
	// boxes are class-based because every such card renders one.
	{anchor: `if(!shots.length)return '<div class="timeline-empty">尚未生成镜头理解；完成分析后会显示可用时间段。</div>';`,
		replacement: `if(!shots.length)return '<div class="timeline-empty"><span>尚未生成镜头理解；完成分析后会显示可用时间段。</span><button class="shot-add" onclick="testDriveStart(this)">开始测试分析</button><div class="test-drive-status"></div><div class="test-drive-chips"></div></div>';`},
	// The drawer script lives inside the page's one <script> block (function
	// declarations hoist, so search() can call renderShotResults and the
	// blocks' delegated click handler is wired after load()).
	{anchor: `load();</script>`, replacement: `loadLibrary();` + shotDrawerScript + shotSelectionScript + `</script>`},
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
		`<div><label for="camera-filter">相机</label><input id="camera-filter" placeholder="例如 Sony FX3" onchange="load()"></div><div><label for="session-filter">拍摄场次</label><select id="session-filter" onchange="load()"><option value="">全部场次</option></select></div><div><label for="status-filter">处理状态</label><select id="status-filter" onchange="load()"><option value="">全部状态</option><option value="ready">可用</option><option value="processing">处理中</option><option value="queued">等待中</option><option value="failed">失败</option><option value="discovered">未处理</option><option value="missing">原片缺失</option></select></div><div><label for="collection-filter">保存的视图</label><select id="collection-filter" onchange="loadCollection(this.value)"><option value="">不使用</option></select><p class="muted collections-hint" id="collections-hint" hidden>还没有保存的视图。在搜索结果页可以把筛选保存为视图。</p><button class="shot-add" onclick="saveCurrentView()" title="把当前筛选条件保存为视图，之后可从下拉框随时恢复">保存当前筛选</button></div><button onclick="clearFilters()">清除筛选</button></section>`
}

// searchBarHTML is the shot-first search entry: it lives above the filters
// so a query is the first thing an editor meets, and it renders shot-level
// results (see search() and renderShotResults in the page script). It is a
// real form so Enter submits and the button gives keyboard/screen-reader
// users an explicit activation point — the single #q on the page.
func searchBarHTML() string {
	return `<form class="search-bar" role="search" onsubmit="search(event)"><input id="q" name="q" placeholder="搜索镜头内容、口述或标签" aria-label="镜头搜索"><button type="submit" class="search-submit">搜索</button><button type="button" class="search-clear" onclick="clearSearch()">清除</button></form>`
}

// filterPanelsHTML is the replacement for the <section id="library"> anchor:
// the search bar, both filter rows and the processing summary. The facet
// options are generated from the normalize vocabularies, never typed into
// the JS.
func filterPanelsHTML() string {
	return searchBarHTML() + filtersRowHTML() + semanticFiltersSection() + `<div id="processing-summary" class="processing-summary" aria-label="处理状态汇总"></div>` + `<div id="root-health" class="root-health" aria-label="素材目录状态"></div>`
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
	return `<div class="shot-drawer" id="shot-drawer" data-shot-drawer aria-hidden="true"><div class="shot-drawer-backdrop" data-drawer-close></div><aside class="shot-drawer-panel" aria-label="镜头预览"><div class="shot-drawer-head"><button class="shot-drawer-close" data-drawer-close aria-label="关闭预览">×</button></div><div class="shot-drawer-body"><video id="shot-video" data-shot-video controls autoplay playsinline></video><div class="shot-drawer-actions"><button id="copy-timecode-btn" onclick="copyTimecode(this)">复制时间码</button><button onclick="loadSimilarShots()">相似镜头</button><button class="shot-add" onclick="addShotToCollection(this)">加入收藏</button></div><div class="shot-drawer-title" id="shot-drawer-title">—</div><div class="shot-drawer-time" id="shot-drawer-time">—</div><p class="shot-drawer-desc" id="shot-drawer-desc">—</p><div class="chips" id="shot-drawer-chips"></div><div class="shot-drawer-fields" id="shot-drawer-fields"></div><div class="shot-drawer-similar" id="shot-drawer-similar"></div></div></aside></div>`
}

// shotDrawerScript is appended inside the page's single script block. It
// wires the timeline blocks and the shot-result cards to the drawer and
// renders the shot-first result list (search() calls renderShotResults).
const shotDrawerScript = `
function shotChips(shot){const chips=[];['tags','objects','actions','mood'].forEach(k=>{(shot[k]||[]).forEach(v=>chips.push('<span class="chip">'+esc(v)+'</span>'))});return chips.join('')}
function shotEvidence(shot){const ev=(shot.evidence||[]).filter(e=>!e.negated);if(!ev.length)return'';const marks=ev.map(e=>{const label=esc(e.constraint);if(e.state==='confirmed')return '<span class="ev-confirmed">'+label+' ✓</span>';if(e.state==='possible')return '<span class="ev-possible">'+label+' 可能</span>';if(e.state==='contradicted')return '<span class="ev-contradicted">'+label+' ✗</span>';return '<span class="ev-unknown">'+label+' 未确认</span>'}).join(' ');return '<div class="shot-evidence">为什么命中：'+marks+'</div>'}
function shotResultCard(s){const score=Math.round((s.score||0)*100);return '<article class="shot-result" data-shot-result data-shot="'+encodeURIComponent(JSON.stringify(s))+'" data-asset="'+esc(s.asset_id)+'" data-filename="'+esc(s.filename||'')+'" onclick="openShotDrawer(JSON.parse(decodeURIComponent(this.dataset.shot)),this.dataset.asset,this.dataset.filename)"><img class="thumb" loading="lazy" src="/api/v1/assets/'+encodeURIComponent(s.asset_id)+'/thumbnail" alt="镜头缩略图" onerror="this.style.visibility=\'hidden\'"><div class="shot-result-main"><div class="shot-result-head"><b class="shot-result-file">'+esc(s.filename||s.asset_id)+'</b><span class="shot-result-time">'+fmt(s.start_ms)+' — '+fmt(s.end_ms)+'</span><span class="status-pill">匹配 '+score+'%</span></div><p class="shot-result-desc">'+esc(s.description||'')+'</p>'+shotEvidence(s)+'<div class="chips">'+shotChips(s)+'</div></div><div class="shot-result-foot"><button class="shot-add" data-add-shot onclick="event.stopPropagation();addShotToCollection(this)">加入收藏</button></div></article>'}
function renderShotResults(shots){if(!shots.length){library.innerHTML='<div class="empty">没有找到匹配的镜头。换个说法，或减少筛选条件后再试。</div>'+(lastLoadedAssets&&lastLoadedAssets.length?'<div class="test-drive-box" id="test-drive-box"><div class="group-title">先分析几个片段</div><p class="muted">让系统先分析当前加载的素材，几分钟后就能用自然语言搜索这些镜头。</p><button class="shot-add" onclick="testDriveStart()">开始测试分析</button><div class="test-drive-status"></div><div class="test-drive-chips"></div></div>':'');return}library.innerHTML='<div class="shot-results-head"><span class="eyebrow">镜头级搜索结果</span><button class="link-button" onclick="load()">返回素材浏览</button></div>'+shots.map(shotResultCard).join('')}
function openShotDrawer(shot,assetId,filename){currentShot=shot;const d=document.getElementById('shot-drawer');if(!d)return;document.getElementById('shot-drawer-title').textContent=filename||assetId||'—';const s=Number(shot.start_ms)||0,e=Number(shot.end_ms)||s;document.getElementById('shot-drawer-time').textContent=fmt(s)+' — '+fmt(e)+' · 时长 '+fmt(e-s);document.getElementById('shot-drawer-desc').textContent=shot.description||'（该镜头没有描述）';document.getElementById('shot-drawer-chips').innerHTML=shotChips(shot);const fields=[];if(shot.confidence!=null)fields.push('<span><b>置信度</b> '+esc(String(shot.confidence))+'</span>');document.getElementById('shot-drawer-fields').innerHTML=fields.join('');const similar=document.getElementById('shot-drawer-similar');if(similar)similar.innerHTML='';// The proxy fragment is clamped to one minute past the shot start and rounded to whole seconds: some browsers treat a huge or sub-second-precise fragment range as a request to load the whole asset and hang the drawer's playback.
const clampEnd=Math.min(e,s+60000);const v=document.getElementById('shot-video');v.src='/api/v1/assets/'+encodeURIComponent(assetId)+'/proxy#t='+Math.floor(s/1000)+','+Math.ceil(clampEnd/1000);d.classList.add('open');d.setAttribute('aria-hidden','false');v.play().catch(function(){})}
function closeShotDrawer(){const d=document.getElementById('shot-drawer');if(!d)return;d.classList.remove('open');d.setAttribute('aria-hidden','true');const v=document.getElementById('shot-video');if(v){v.pause();v.removeAttribute('src')}}
function openShotDrawerInit(){document.addEventListener('click',function(e){const block=e.target.closest('.shot');if(!block)return;const raw=block.dataset.shot;let shot={};if(raw){try{shot=JSON.parse(decodeURIComponent(raw))}catch(_){shot={}}}openShotDrawer(shot,block.dataset.asset||'',block.dataset.filename||'')});document.querySelectorAll('[data-drawer-close]').forEach(function(el){el.addEventListener('click',closeShotDrawer)});document.addEventListener('keydown',function(e){if(e.key==='Escape')closeShotDrawer()})}
openShotDrawerInit();`

// shotSelectionScript is the selection loop: 加入收藏 (card and drawer),
// the drawer's 复制时间码 and 相似镜头 actions, the 保存当前筛选 view
// builder, and the test-drive coaching on both empty states. Every fetch
// goes through apiErrMsg; the modal, drawer and empty-state elements use ids
// that collide with nothing else in the page.
const shotSelectionScript = `
function fmtTimecode(ms){ms=Math.max(0,Math.floor(Number(ms)||0));const h=Math.floor(ms/3600000),m=Math.floor(ms%3600000/60000),s=Math.floor(ms%60000/1000),mm=ms%1000;return h+':'+String(m).padStart(2,'0')+':'+String(s).padStart(2,'0')+'.'+String(mm).padStart(3,'0')}
async function copyTimecode(btn){if(!currentShot)return;const start=Number(currentShot.start_ms)||0,end=Math.max(start,Number(currentShot.end_ms)||start);const text=start===end?fmtTimecode(start):fmtTimecode(start)+'–'+fmtTimecode(end);try{if(!navigator.clipboard||!navigator.clipboard.writeText)throw Error('no clipboard api');await navigator.clipboard.writeText(text)}catch(_){const ta=document.createElement('textarea');ta.value=text;ta.style.position='fixed';ta.style.opacity='0';document.body.appendChild(ta);ta.select();try{document.execCommand('copy')}catch(_){}ta.remove()}if(btn)btn.textContent='✓ 已复制'}
async function loadSimilarShots(){const box=document.getElementById('shot-drawer-similar');if(!box)return;const shotId=currentShot&&(currentShot.shot_id||currentShot.id);if(!shotId)return;box.innerHTML='<div class="muted">正在查找相似镜头…</div>';try{const r=await fetch('/api/v1/shots/'+encodeURIComponent(shotId)+'/similar');if(!r.ok)throw Error(await apiErrMsg(r));const hits=await r.json();const items=Array.isArray(hits)?hits:[];box.innerHTML=items.length?'<div class="group-title">相似镜头</div>'+items.map(function(h){return '<div class="similar-item" data-s="'+encodeURIComponent(JSON.stringify({shot_id:h.id,asset_id:h.asset_id,filename:h.filename||'',start_ms:h.start_ms,end_ms:h.end_ms,description:h.description||'',confidence:h.confidence}))+'" onclick="openSimilarShot(JSON.parse(decodeURIComponent(this.dataset.s)))"><b>'+fmt(h.start_ms)+' — '+fmt(h.end_ms)+'</b>'+esc(h.description||'')+'</div>'}).join(''):'<div class="muted">没有找到相似镜头。</div>'}catch(_){box.innerHTML=''}}
function openSimilarShot(s){openShotDrawer(s,s.asset_id,s.filename||'')}
function addShotToCollection(btn){let shot=null;const host=btn?btn.closest('[data-shot]'):null;if(host&&host.dataset.shot){try{shot=JSON.parse(decodeURIComponent(host.dataset.shot))}catch(_){shot=null}}if(!shot)shot=currentShot;if(!shot)return;addShotSourceBtn=btn||null;addShotModal(shot)}
function addShotModal(shot){let overlay=document.getElementById('add-shot-modal');if(!overlay){overlay=document.createElement('div');overlay.id='add-shot-modal';overlay.className='shot-modal';overlay.innerHTML='<div class="shot-modal-card"><h3>加入收藏</h3><div class="shot-modal-error"></div><div class="shot-modal-list"></div><div class="shot-modal-new"><input class="shot-modal-name" placeholder="新建收藏名称"><button class="shot-add" onclick="createAndAddCollection()">创建并加入</button></div><div style="text-align:right;margin-top:12px"><button class="shot-add" onclick="closeAddShotModal()">取消</button></div></div>';document.body.appendChild(overlay)}overlay.dataset.shot=encodeURIComponent(JSON.stringify(shot));overlay.classList.add('open');(async function(){const listEl=overlay.querySelector('.shot-modal-list'),errEl=overlay.querySelector('.shot-modal-error');errEl.textContent='';listEl.innerHTML='<div class="muted">正在读取收藏…</div>';try{const r=await fetch('/api/v1/collections',{headers:authHeaders()});if(!r.ok){if(r.status===401){errEl.textContent='需要管理 Token：请先在左侧栏填入';return}throw Error(await apiErrMsg(r))}const list=await r.json();const items=Array.isArray(list)?list:[];listEl.innerHTML=items.length?items.map(function(c){return '<div class="shot-modal-row"><span>'+esc(c.name)+'</span><span class="muted">'+c.shot_count+' 个镜头</span><button class="shot-add" data-cid="'+esc(c.id)+'" onclick="addToCollection(this.dataset.cid)">加入</button></div>'}).join(''):'<div class="muted">还没有收藏。可以先新建一个，或在筛选栏保存当前视图。</div>'}catch(e){errEl.textContent='无法读取收藏：'+e.message}}())}
function closeAddShotModal(){const overlay=document.getElementById('add-shot-modal');if(overlay)overlay.classList.remove('open')}
async function addToCollection(cid){const overlay=document.getElementById('add-shot-modal');if(!overlay)return;let shot={};try{shot=JSON.parse(decodeURIComponent(overlay.dataset.shot))}catch(_){return}const shotId=shot.shot_id||shot.id;if(!shotId)return;try{const r=await fetch('/api/v1/collections/'+encodeURIComponent(cid)+'/shots',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({shot_id:shotId})});if(!r.ok){if(r.status===401){overlay.querySelector('.shot-modal-error').textContent='需要管理 Token：请先在左侧栏填入';return}throw Error(await apiErrMsg(r))}closeAddShotModal();if(addShotSourceBtn){addShotSourceBtn.textContent='✓ 已加入';addShotSourceBtn.classList.add('ok')}}catch(e){overlay.querySelector('.shot-modal-error').textContent='加入收藏失败：'+e.message}}
async function createAndAddCollection(){const overlay=document.getElementById('add-shot-modal');if(!overlay)return;const input=overlay.querySelector('.shot-modal-name'),errEl=overlay.querySelector('.shot-modal-error');const name=input.value.trim();if(!name){errEl.textContent='请输入收藏名称';return}let shot={};try{shot=JSON.parse(decodeURIComponent(overlay.dataset.shot))}catch(_){}const shotId=shot.shot_id||shot.id;try{const r=await fetch('/api/v1/collections',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({name:name})});if(!r.ok){if(r.status===401){errEl.textContent='需要管理 Token：请先在左侧栏填入';return}throw Error(await apiErrMsg(r))}const created=await r.json();if(shotId){const r2=await fetch('/api/v1/collections/'+encodeURIComponent(created.id)+'/shots',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({shot_id:shotId})});if(!r2.ok&&r2.status!==409){if(r2.status===401){errEl.textContent='需要管理 Token：请先在左侧栏填入';return}throw Error(await apiErrMsg(r2))}}closeAddShotModal();resetCollectionSelect();await loadCollections();if(addShotSourceBtn){addShotSourceBtn.textContent='✓ 已加入';addShotSourceBtn.classList.add('ok')}}catch(e){errEl.textContent='创建收藏失败：'+e.message}}
function resetCollectionSelect(){const select=document.getElementById('collection-filter');if(!select)return;select.innerHTML='<option value="">不使用</option>'}
function isoDate(id){const v=document.getElementById(id).value;return v?new Date(v+'T00:00:00Z').toISOString():undefined}
function isoDatePlus1(id){const v=document.getElementById(id).value;if(!v)return undefined;const d=new Date(v+'T00:00:00Z');d.setUTCDate(d.getUTCDate()+1);return d.toISOString()}
async function saveCurrentView(){const name=prompt('保存当前筛选为视图：');if(name===null)return;const filter={captured_from:isoDate('date-from'),captured_to:isoDatePlus1('date-to'),region_label:document.getElementById('region-filter').value.trim(),camera_model:document.getElementById('camera-filter').value,session_id:document.getElementById('session-filter').value,status:document.getElementById('status-filter').value};Object.assign(filter,filterFacets());try{const r=await fetch('/api/v1/collections',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({name:name,filter:filter})});if(!r.ok){if(r.status===401)throw Error('需要 Hub 管理 Token：请先在左侧填入');throw Error(await apiErrMsg(r))}resetCollectionSelect();await loadCollections();alert('已保存视图：'+name)}catch(e){alert('保存失败：'+e.message)}}
async function testDriveStart(btn){let ids=(lastLoadedAssets||[]).slice(0,3);const box=btn?btn.parentElement:document.getElementById('test-drive-box');if(!box||!ids.length)return;const status=box.querySelector('.test-drive-status'),chipsBox=box.querySelector('.test-drive-chips');const b=btn||box.querySelector('button');if(b){b.disabled=true;b.textContent='正在启动…'}try{const r=await fetch('/api/v1/test-drive',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({asset_ids:ids})});if(!r.ok){if(r.status===401)throw Error('需要 Hub 管理 Token：请先在左侧填入');throw Error(await apiErrMsg(r))}if(status)status.innerHTML='已开始处理，可在 <a href="/progress">处理任务</a> 查看进度。';testDriveChips(chipsBox,ids)}catch(e){if(status)status.textContent='测试分析启动失败：'+e.message}finally{if(b){b.disabled=false;b.textContent='开始测试分析'}}}
async function testDriveChips(box,ids){if(!box)return;try{const r=await fetch('/api/v1/test-drive/suggestions?assets='+ids.map(encodeURIComponent).join(','));if(!r.ok)return;const d=await r.json();const items=Array.isArray(d.suggestions)?d.suggestions:[];box.innerHTML=items.length?'<div class="group-title">试试搜索</div><div class="chips">'+items.map(function(s){return '<button class="try-chip" data-q="'+esc(s)+'" onclick="document.getElementById(\'q\').value=this.dataset.q;search()">'+esc(s)+'</button>'}).join('')+'</div>':''}catch(_){box.innerHTML=''}}`

func enhanceLibraryPage(page string) string {
	for _, p := range libraryPagePatches {
		page = strings.Replace(page, p.anchor, p.replacement, 1)
	}
	return page
}
