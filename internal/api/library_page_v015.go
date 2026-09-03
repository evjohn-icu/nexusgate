package api

import (
	"fmt"
	"strings"

	"github.com/evjohn-icu/nexusslate/internal/normalize"
)

var libraryIndexHTML = enhanceLibraryPage(legacyLibraryIndexHTML)

// facetLabels maps a controlled-vocabulary value to the catalog key that
// names its label. The vocabularies and this map are separate sources of
// truth; TestFacetLabelsCoverEveryVocabularyValue pins the seam so a value
// added to a vocabulary fails loudly instead of rendering as its English slug
// in a Chinese UI. The label keys (facet.*) exist in all five catalogs, and
// facetSelectHTML renders them as [[i18n:facet.*]] markers so the option text
// follows the selected UI locale. Shared values (unknown, mixed,
// not_recommended) carry one key across the fields that use them.
var facetLabels = map[string]string{
	// asset_type
	"b_roll":            "facet.b_roll",
	"talking_to_camera": "facet.talking_to_camera",
	"conversation":      "facet.conversation",
	"activity":          "facet.activity",
	"performance":       "facet.performance",
	"food":              "facet.food",
	"transport":         "facet.transport",
	"architecture":      "facet.architecture",
	"landscape":         "facet.landscape",
	"animal":            "facet.animal",
	"document":          "facet.document",
	"screen_recording":  "facet.screen_recording",
	"accidental":        "facet.accidental",
	"other":             "facet.other",
	// camera_motion
	"static":    "facet.static",
	"pan_left":  "facet.pan_left",
	"pan_right": "facet.pan_right",
	"tilt_up":   "facet.tilt_up",
	"tilt_down": "facet.tilt_down",
	"forward":   "facet.forward",
	"backward":  "facet.backward",
	"tracking":  "facet.tracking",
	"orbit":     "facet.orbit",
	"handheld":  "facet.handheld",
	// shot_size
	"extreme_wide":     "facet.extreme_wide",
	"wide":             "facet.wide",
	"medium":           "facet.medium",
	"close_up":         "facet.close_up",
	"extreme_close_up": "facet.extreme_close_up",
	// audio_type
	"silence":          "facet.silence",
	"ambient":          "facet.ambient",
	"speech":           "facet.speech",
	"music":            "facet.music",
	"singing":          "facet.singing",
	"speech_and_music": "facet.speech_and_music",
	"noise":            "facet.noise",
	// quality
	"excellent": "facet.excellent",
	"usable":    "facet.usable",
	"limited":   "facet.limited",
	// usable_as
	"hook":              "facet.hook",
	"opening":           "facet.opening",
	"establishing":      "facet.establishing",
	"transition":        "facet.transition",
	"montage":           "facet.montage",
	"narration_support": "facet.narration_support",
	"character_intro":   "facet.character_intro",
	"activity_detail":   "facet.activity_detail",
	"emotional_pause":   "facet.emotional_pause",
	"behind_the_scenes": "facet.behind_the_scenes",
	"ending":            "facet.ending",
	// shared by several fields
	"mixed":           "facet.mixed",
	"unknown":         "facet.unknown",
	"not_recommended": "facet.not_recommended",
}

// facetFields drives the semantic filter controls in the filter drawer. The
// values list is read straight from the normalize vocabularies, so a
// vocabulary addition grows the control for free; nothing here is hardcoded
// in the JS. The name/empty fields are catalog keys resolved as [[i18n:...]]
// markers. facetFields[0] (asset-type) lives in the drawer's ASSET group;
// the remaining five facets resolve through the shot's asset (they exist only
// in asset_analysis), so their labels say so: a close-up shot inside an asset
// that is mostly wide is legitimately matched by 景别(素材级)=wide, and nobody
// should read that as the shot itself being wide. They live in the SHOT group.
var facetFields = []struct {
	id, nameKey, emptyKey string
	values                []string
	multiple              bool
}{
	{id: "asset-type-select", nameKey: "library.facet.assetType", emptyKey: "library.facet.assetTypeEmpty", values: normalize.AssetTypeValues},
	{id: "shot-size-select", nameKey: "library.facet.shotSize", emptyKey: "library.facet.shotSizeEmpty", values: normalize.ShotSizeValues, multiple: true},
	{id: "camera-motion-select", nameKey: "library.facet.cameraMotion", emptyKey: "library.facet.cameraMotionEmpty", values: normalize.MotionValues, multiple: true},
	{id: "audio-type-select", nameKey: "library.facet.audioType", emptyKey: "library.facet.audioTypeEmpty", values: normalize.AudioTypeValues, multiple: true},
	{id: "quality-select", nameKey: "library.facet.quality", emptyKey: "library.facet.qualityEmpty", values: normalize.QualityValues, multiple: true},
	{id: "usable-as-select", nameKey: "library.facet.usableAs", emptyKey: "library.facet.usableAsEmpty", values: normalize.UsableAsValues, multiple: true},
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
// inside text an earlier patch inserted. The leading copy patch and the larger
// style/script patches all use the same exactly-once rule. Product copy is
// referenced as [[i18n:*]] markers (resolved by serveLocalizedPage) or tdT()
// calls, never as raw Chinese.
var libraryPagePatches = []pagePatch{
	{anchor: "[[i18n:library.subtitle]]", replacement: "[[i18n:library.subtitleLead]]"},
	{anchor: `.library{display:grid;gap:13px}`,
		replacement: `html,body{overflow-x:clip}.filters{display:grid;grid-template-columns:repeat(4,minmax(0,1fr)) auto;gap:9px;margin:0 0 12px;padding:13px;background:var(--surface);border:1px solid var(--rule);border-radius:14px}.filters label{display:block;color:var(--text-muted);font-size:10px;font-weight:800;letter-spacing:.06em;margin-bottom:5px}.filters input,.filters select{width:100%;padding:8px 9px}#collection-filter{max-width:200px}.filters button{align-self:end}.filters .collections-hint{margin:6px 0 0;font-size:11px;line-height:1.5}.filters .collections-hint.warn{color:var(--ev-contradicted)}.processing-summary{display:flex;gap:7px;flex-wrap:wrap;margin:0 0 18px}.status-pill{border:1px solid var(--rule);border-radius:999px;padding:5px 9px;color:var(--text-muted);font-size:12px}.status-pill b{color:var(--text)}.group-title{margin:20px 2px 8px;color:var(--text-muted);font-size:13px;font-weight:850;letter-spacing:.02em}.searchbar .search-submit{background:var(--btn-solid-bg);border-color:var(--btn-solid-bg);color:var(--btn-solid-fg)}.searchbar .search-submit:hover{opacity:.86}.searchbar .search-clear{background:transparent;border-color:transparent;color:var(--text-muted)}.searchbar .search-clear:hover{background:var(--inset);color:var(--text)}.active-chips{display:flex;flex-wrap:wrap;gap:6px;align-items:center;margin:-2px 0 12px}.active-chips .chip{max-width:260px}.chip-remove{display:inline;height:auto;min-height:0;padding:0 0 0 4px;border:0;background:transparent;color:inherit;font-size:12px;line-height:1;cursor:pointer}.chip-remove:hover{background:transparent;color:var(--ev-contradicted)}.active-chips .chip-clear-all{background:transparent;border-color:transparent;color:var(--ev-contradicted);font-weight:700;height:auto;min-height:0;padding:0 4px;font-size:12px}.active-chips .chip-clear-all:hover{background:var(--ev-contradicted-wash)}.shot-results-head{display:flex;align-items:center;justify-content:space-between;gap:14px;margin:0 0 14px}.shot-results-foot{display:flex;align-items:center;justify-content:space-between;gap:14px;flex-wrap:wrap;margin:14px 0 4px}.link-button{background:none;border:0;color:var(--brand);font-weight:800;cursor:pointer;padding:0}.shot-result{display:grid;grid-template-columns:200px 1fr;gap:14px;align-items:start;padding:13px;background:var(--surface);border:1px solid var(--rule);border-radius:16px;cursor:pointer;margin:0 0 11px;transition:border-color .15s}.shot-result:hover{border-color:var(--rule-strong)}.thumb-wrap{position:relative;display:block;min-width:0}.shot-result .thumb{width:100%;aspect-ratio:16/9;object-fit:cover;border-radius:10px;background:var(--inset);min-height:0}.shot-result-tc{position:absolute;left:8px;bottom:8px;font-family:var(--font-data);font-variant-numeric:tabular-nums;background:rgba(0,0,0,.55);color:var(--overlay-fg);padding:2px 6px;border-radius:4px;font-size:11px}.shot-result-head{display:flex;align-items:baseline;gap:10px;flex-wrap:wrap}.shot-result-file{color:var(--text);font-size:14px}.shot-result-time{color:var(--text-muted);font-size:13px;font-weight:700}.shot-result-desc{margin:8px 0;color:var(--text-muted);line-height:1.55}.shot-evidence{margin:6px 0 8px;padding:8px 10px;background:var(--inset);border:1px solid var(--rule);border-radius:10px;color:var(--text-muted);font-size:12px;line-height:1.7}.shot-evidence .ev-confirmed{color:var(--ev-confirmed)}.shot-evidence .ev-possible{color:var(--ev-draft)}.shot-evidence .ev-contradicted{color:var(--ev-contradicted)}.shot-evidence .ev-unknown{color:var(--ev-unknown)}.shot-drawer{position:fixed;inset:0;z-index:40;display:none;pointer-events:none}.shot-drawer.open{display:block;pointer-events:auto}.shot-drawer-backdrop{position:absolute;inset:0;background:rgba(6,12,24,.87)}.shot-drawer-panel{position:absolute;top:0;right:0;bottom:0;width:min(720px,48vw);background:var(--surface);border-left:1px solid var(--rule);display:flex;flex-direction:column}.shot-drawer.open .shot-drawer-panel{animation:shotDrawerIn .18s ease}@keyframes shotDrawerIn{from{transform:translateX(100%)}to{transform:translateX(0)}}.shot-drawer-head{display:flex;justify-content:flex-end;padding:12px 14px;border-bottom:1px solid var(--rule)}.shot-drawer-close{background:var(--rule);color:var(--text);border:0;border-radius:9px;width:34px;height:34px;font-size:17px;cursor:pointer}.shot-drawer-body{overflow:auto;padding:16px}.shot-drawer-body video{width:100%;border-radius:12px;background:var(--inset)}.shot-drawer-time{margin-top:12px;color:var(--brand);font-weight:800;font-size:13px}.shot-drawer-desc{color:var(--text-muted);line-height:1.6;margin:10px 0}.shot-drawer-evidence{margin-top:12px}.shot-drawer-fields{margin-top:10px;display:grid;gap:4px;color:var(--text-muted);font-size:12px}.filter-drawer{position:fixed;inset:0;z-index:50;display:none;pointer-events:none}.filter-drawer.open{display:block;pointer-events:auto}.filter-drawer-backdrop{position:absolute;inset:0;background:rgba(6,12,24,.87)}.filter-drawer-panel{position:absolute;top:0;right:0;bottom:0;width:min(520px,92vw);background:var(--surface);border-left:1px solid var(--rule);display:flex;flex-direction:column}.filter-drawer.open .filter-drawer-panel{animation:shotDrawerIn .18s ease}.filter-drawer-head{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:12px 14px;border-bottom:1px solid var(--rule)}.filter-drawer-title{font-family:var(--font-data);font-size:var(--fs-label);font-weight:700;letter-spacing:.16em;text-transform:uppercase;color:var(--text-faint)}.filter-drawer-close{background:var(--rule);color:var(--text);border:0;border-radius:9px;width:34px;height:34px;font-size:17px;cursor:pointer}.filter-drawer-body{overflow:auto;padding:16px;display:grid;gap:18px}.filter-group{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px}.filter-group-title{grid-column:1/-1;font-family:var(--font-data);font-size:var(--fs-label);font-weight:700;letter-spacing:.16em;text-transform:uppercase;color:var(--text-faint);border-bottom:1px solid var(--rule);padding-bottom:6px}.filter-group label{display:block;color:var(--text-muted);font-size:10px;font-weight:800;letter-spacing:.06em;margin-bottom:5px}.filter-group input,.filter-group select{width:100%;padding:8px 9px}.filter-drawer-foot{display:flex;align-items:center;justify-content:space-between;gap:10px;padding:12px 14px;border-top:1px solid var(--rule)}.root-health{display:grid;gap:8px;margin:0 0 18px}.root-health-warn{display:flex;align-items:center;gap:9px;padding:10px 13px;background:var(--ev-contradicted-wash);border:1px solid var(--ev-contradicted);border-radius:12px;color:var(--ev-contradicted);font-size:13px;font-weight:700}.library{display:grid;gap:13px}`},
	// Selection affordances (加入收藏, drawer actions, test drive, saved-view
	// button) ride on the same sheet as the filter panels; keeping them here
	// keeps the whole page's look in one place instead of scattering inline
	// styles through the script. .shot-add is also reused inside the result
	// cards and the timeline-empty state, so its size must stay compact.
	{anchor: `.root-health-warn{display:flex;align-items:center;gap:9px;padding:10px 13px;background:var(--ev-contradicted-wash);border:1px solid var(--ev-contradicted);border-radius:12px;color:var(--ev-contradicted);font-size:13px;font-weight:700}.library{display:grid;gap:13px}`,
		replacement: `.root-health-warn{display:flex;align-items:center;gap:9px;padding:10px 13px;background:var(--ev-contradicted-wash);border:1px solid var(--ev-contradicted);border-radius:12px;color:var(--ev-contradicted);font-size:13px;font-weight:700}.shot-result-foot{display:flex;justify-content:flex-end;padding:10px 13px 4px}.shot-add{background:var(--inset);color:var(--text-muted);border:1px solid var(--rule);border-radius:999px;padding:4px 11px;font-size:12px;font-weight:800;cursor:pointer}.shot-add:hover{background:var(--raised)}.shot-add.ok{color:var(--ev-confirmed);border-color:var(--ev-confirmed)}.shot-add:disabled{opacity:.5;cursor:default}.test-drive-box{margin:14px 0 0;padding:14px;border:1px dashed var(--rule-strong);border-radius:12px;background:var(--inset)}.test-drive-box .group-title{margin-top:0}.test-drive-status{color:var(--ev-confirmed);font-size:12px;line-height:1.6;margin-top:8px}.test-drive-status a{color:var(--brand)}.try-chip{background:var(--inset);color:var(--text-muted);border:1px solid var(--rule);border-radius:999px;padding:5px 11px;font-size:12px;font-weight:800;cursor:pointer}.try-chip:hover{background:var(--raised)}.shot-drawer-actions{display:flex;gap:8px;flex-wrap:wrap;margin:0 0 12px}.shot-drawer-similar{display:grid;gap:8px;margin-top:12px}.similar-item{border:1px solid var(--rule);border-radius:10px;padding:9px 11px;background:var(--inset);cursor:pointer;display:grid;gap:3px;font-size:12px;color:var(--text-muted);line-height:1.5}.similar-item:hover{border-color:var(--rule-strong)}.similar-item b{color:var(--brand)}.shot-modal{position:fixed;inset:0;z-index:60;display:none;place-items:center;background:rgba(6,12,24,.8)}.shot-modal.open{display:grid}.shot-modal-card{width:min(440px,92vw);background:var(--surface);border:1px solid var(--rule);border-radius:14px;padding:18px}.shot-modal-card h3{margin:0 0 4px;font-size:16px;color:var(--text)}.shot-modal-error{color:var(--ev-contradicted);font-size:12px;min-height:16px;margin:8px 0}.shot-modal-list{display:grid;gap:8px;margin:10px 0}.shot-modal-row{display:flex;align-items:center;justify-content:space-between;gap:10px;padding:9px 11px;background:var(--inset);border:1px solid var(--rule);border-radius:10px;font-size:13px}.shot-modal-row .muted{font-size:11px}.shot-modal-new{display:flex;gap:8px;margin-top:10px}.shot-modal-new input{flex:1;padding:8px 9px}.library{display:grid;gap:13px}`},
	{anchor: `<section id="library" class="library"`,
		replacement: filterPanelsHTML() + shotDrawerHTML() + `<section id="library" class="library"`},
	{anchor: `async function load(ids)`,
		replacement: `
let activeCollection='';let shotPage={query:'',offset:0,limit:40,items:[],hasMore:false,nextOffset:null,windowExhausted:false,loading:false};const filterControls=[['date_from','date-from'],['date_to','date-to'],['region','region-filter'],['camera','camera-filter'],['session','session-filter'],['status','status-filter'],['collection','collection-filter'],['asset_type','asset-type-select'],['asset_shot_size','shot-size-select'],['asset_camera_motion','camera-motion-select'],['asset_audio_type','audio-type-select'],['asset_quality','quality-select'],['asset_usable_as','usable-as-select'],['min_duration_ms','min-duration'],['max_duration_ms','max-duration']];let pendingSession='';function collectFilterParams(includeQ){const values={date_from:document.getElementById('date-from').value,date_to:document.getElementById('date-to').value,region:document.getElementById('region-filter').value.trim(),camera:document.getElementById('camera-filter').value,session:document.getElementById('session-filter').value,status:document.getElementById('status-filter').value};const query=new URLSearchParams();if(includeQ){const q=document.getElementById('q').value.trim();if(q)query.set('q',q)}Object.entries(values).forEach(([key,value])=>{if(value)query.set(key,value)});[['asset-type-select','asset_type']].forEach(([id,param])=>{const value=document.getElementById(id).value;if(value)query.set(param,value)});[['shot-size-select','asset_shot_size'],['camera-motion-select','asset_camera_motion'],['audio-type-select','asset_audio_type'],['quality-select','asset_quality'],['usable-as-select','asset_usable_as']].forEach(([id,param])=>{const sel=document.getElementById(id);const vals=Array.from(sel.selectedOptions).map(o=>o.value).filter(Boolean);if(vals.length)query.set(param,vals.join(','))});[['min-duration','min_duration_ms'],['max-duration','max_duration_ms']].forEach(([id,param])=>{const raw=document.getElementById(id).value.trim();if(raw==='')return;const sec=Number(raw);if(!Number.isFinite(sec)||sec<0)return;query.set(param,String(Math.round(sec*1000)))});return query}function filterQuery(){const query=collectFilterParams(false);return query.toString()?'&'+query.toString():''}function refreshResults(){const q=document.getElementById('q').value.trim();return q?search():load()}function syncURL(){const query=collectFilterParams(true);if(activeCollection)query.set('collection',activeCollection);history.replaceState(null,'','?'+query.toString())}function restoreFromURL(){try{const params=new URLSearchParams(location.search);const setVal=function(id,v){const el=document.getElementById(id);if(el&&v!=null)el.value=v};setVal('q',params.get('q'));activeCollection=params.get('collection')||'';setVal('date-from',params.get('date_from')||'');setVal('date-to',params.get('date_to')||'');setVal('region-filter',params.get('region')||'');setVal('camera-filter',params.get('camera')||'');setVal('status-filter',params.get('status')||'');pendingSession=params.get('session')||'';setVal('asset-type-select',params.get('asset_type')||'');[['asset_shot_size','shot-size-select'],['asset_camera_motion','camera-motion-select'],['asset_audio_type','audio-type-select'],['asset_quality','quality-select'],['asset_usable_as','usable-as-select']].forEach(function(pair){const v=params.get(pair[0]);if(!v)return;const sel=document.getElementById(pair[1]);if(!sel)return;const values=v.split(',').map(function(s){return s.trim()}).filter(Boolean);Array.from(sel.options).forEach(function(o){if(o.value)o.selected=values.indexOf(o.value)!==-1})});[['min_duration_ms','min-duration'],['max_duration_ms','max-duration']].forEach(function(pair){const v=params.get(pair[0]);if(!v)return;const sec=Number(v)/1000;if(Number.isFinite(sec)&&sec>=0)setVal(pair[1],String(sec))})}catch(_){}}function filterCount(){let n=0;filterControls.forEach(function(c){const el=document.getElementById(c[1]);if(el&&String(el.value||'').trim()!=='')n++});return n}function updateFiltersToggle(){const btn=document.getElementById('filters-toggle');if(!btn)return;const n=filterCount();btn.textContent=n?tdT('library.filter.filtersCount',{count:n}):tdT('library.filter.filters')}function filterChips(){const chips=[];filterControls.forEach(function(c){const el=document.getElementById(c[1]);if(!el)return;const addChip=function(label){chips.push('<span class="chip" data-filter-chip="'+c[0]+'">'+esc(label)+' <button type="button" class="chip-remove" data-param="'+esc(c[0])+'" aria-label="'+esc(tdT('library.filter.removeChip',{label:label}))+'" onclick="removeFilterChip(this.dataset.param)">×</button></span>')};if(el.multiple&&el.tagName==='SELECT'){Array.from(el.selectedOptions).forEach(function(o){if(o.value)addChip(o.textContent.trim())})}else{const value=el.value||'';if(String(value).trim())addChip(el.tagName==='SELECT'&&el.selectedOptions&&el.selectedOptions.length?el.selectedOptions[0].textContent.trim():value)}});return chips}function refreshFilterUI(){const box=document.getElementById('active-chips');if(box){const chips=filterChips();if(chips.length){box.hidden=false;box.innerHTML=chips.join('')+' <button type="button" class="chip chip-clear-all" onclick="clearFilters()">'+esc(tdT('library.filter.clear'))+'</button>'}else{box.hidden=true;box.innerHTML=''}}updateFiltersToggle()}function removeFilterChip(param){if(param==='collection'){const sel=document.getElementById('collection-filter');if(sel)sel.value='';activeCollection='';load();syncURL();return}const entry=filterControls.filter(function(c){return c[0]===param})[0];if(!entry)return;const el=document.getElementById(entry[1]);if(!el)return;if(el.multiple&&el.tagName==='SELECT'){el.selectedIndex=-1}else{el.value=''}refreshResults();syncURL()}function openFilterDrawer(){tdOpenSurface('filter-drawer',document.getElementById('filters-toggle'),'select')}function closeFilterDrawer(){tdCloseSurface('filter-drawer')}function applyFilters(){closeFilterDrawer();refreshResults();syncURL()}function filterFacets(){const facets={};const pick=(id,key)=>{const sel=document.getElementById(id);const vals=Array.from(sel.selectedOptions).map(o=>o.value).filter(Boolean);if(vals.length)facets[key]=vals};pick('asset-type-select','asset_types');pick('shot-size-select','shot_sizes');pick('camera-motion-select','camera_motions');pick('audio-type-select','audio_types');pick('quality-select','qualities');pick('usable-as-select','usable_as');[['min-duration','min_duration_ms'],['max-duration','max_duration_ms']].forEach(([id,key])=>{const raw=document.getElementById(id).value.trim();if(raw==='')return;const sec=Number(raw);if(!Number.isFinite(sec)||sec<0)return;facets[key]=Math.round(sec*1000)});return facets}function clearFilters(){activeCollection='';['date-from','date-to','region-filter','camera-filter'].forEach(id=>document.getElementById(id).value='');document.getElementById('session-filter').value='';document.getElementById('status-filter').value='';document.getElementById('collection-filter').value='';document.getElementById('asset-type-select').value='';['shot-size-select','camera-motion-select','audio-type-select','quality-select','usable-as-select'].forEach(id=>document.getElementById(id).selectedIndex=-1);document.getElementById('min-duration').value='';document.getElementById('max-duration').value='';refreshResults();syncURL()}function groupKey(x){const date=x.captured_at?String(x.captured_at).slice(0,10):tdT('library.groupDateUnknown');return [date,x.region_label||tdT('library.groupRegionUnknown'),x.camera_model||tdT('library.groupCameraUnknown'),x.session_id||tdT('library.groupSessionUnknown')].join(' · ')}async function loadSessions(){const select=document.getElementById('session-filter');try{const response=await fetch('/api/v1/shoot-sessions?limit=500');if(!response.ok)throw Error('sessions unavailable');const sessions=await response.json();(Array.isArray(sessions)?sessions:[]).forEach(s=>{const option=document.createElement('option');option.value=s.id;const date=s.starts_at?String(s.starts_at).slice(0,10):tdT('library.groupDateUnknown');const details=[date,s.camera_label,s.region_label].filter(Boolean).join(' · ');option.textContent=(s.title||s.id)+(details?' · '+details:'');select.appendChild(option)});if(pendingSession&&select.querySelector('option[value="'+pendingSession+'"]'))select.value=pendingSession}catch(_){select.innerHTML='<option value="">'+tdT('library.sessionsUnavailable')+'</option>'}}function collectionsHint(text,warn){const el=document.getElementById('collections-hint');if(!el)return;el.hidden=!text;el.textContent=text||'';el.classList.toggle('warn',!!warn)}function hasMeaningfulFilter(f){f=f||{};return !!(f.captured_from||f.captured_to||f.region_label||f.camera_model||f.session_id||f.status||(f.asset_types&&f.asset_types.length)||(f.shot_sizes&&f.shot_sizes.length)||(f.camera_motions&&f.camera_motions.length)||(f.audio_types&&f.audio_types.length)||(f.qualities&&f.qualities.length)||(f.usable_as&&f.usable_as.length)||f.min_duration_ms||f.max_duration_ms)}async function loadCollections(){const select=document.getElementById('collection-filter');try{const r=await fetch('/api/v1/collections');if(!r.ok)throw Error('HTTP '+r.status);const items=await r.json();const views=(Array.isArray(items)?items:[]).filter(c=>hasMeaningfulFilter(c&&c.filter));views.forEach(item=>{const option=document.createElement('option');option.value=item.id;option.textContent=item.name;select.appendChild(option)});if(activeCollection&&select.querySelector('option[value="'+activeCollection+'"]'))select.value=activeCollection;collectionsHint(views.length?'':tdT('library.filter.collectionsHint'),false)}catch(_){collectionsHint(tdT('library.filter.collectionsHintWarn'),true)}}async function loadSummary(){try{const data=await fetch('/api/v1/library/processing-summary?limit=300'+filterQuery()).then(r=>r.ok?r.json():null);if(!data)return;const labels={ready:tdT('status.ready'),processing:tdT('status.processing'),queued:tdT('status.queued'),failed:tdT('status.failed'),discovered:tdT('status.discovered'),missing:tdT('status.missing')};document.getElementById('processing-summary').innerHTML=Object.entries(labels).map(([key,label])=>'<span class="status-pill">'+label+' <b>'+esc((data.by_status||{})[key]||0)+'</b></span>').join('')}catch(_){}}function adminToken(){const el=document.getElementById('admin-token');return el?el.value.trim():''}function authHeaders(base){const headers=new Headers(base||{});const token=adminToken();if(token)headers.set('Authorization','Bearer '+token);return headers}function fmtHealthTime(v){if(!v)return'—';const t=new Date(v);return isNaN(t.getTime())?esc(v):tdFormatDateTime(t)}// Root health rides the same load() flow as the processing summary: /api/v1/roots/health is admin-only, so without a token this fetch 401s and the strip stays empty rather than nagging. When a root is unavailable the reconciliation gate is paused server-side; this line is the library page's answer to "why is nothing marked missing".
async function loadRootHealth(){const el=document.getElementById('root-health');if(!el)return;try{const list=await fetch('/api/v1/roots/health',{headers:authHeaders()}).then(r=>r.ok?r.json():null);if(!list)return;const down=list.filter(h=>h.state==='unavailable');el.innerHTML=down.length?down.map(h=>'<div class="root-health-warn">'+esc(tdT('library.rootHealthWarn',{path:h.path,time:fmtHealthTime(h.last_healthy_at)}))+'</div>').join(''):''}catch(_){}}async function loadCollection(id){activeCollection=id||'';load()}async function loadLibrary(){restoreFromURL();const restored=document.getElementById('q').value.trim();const ready=Promise.all([loadSessions(),loadCollections()]);if(!restored){refreshFilterUI();return load()}await ready;refreshFilterUI();search()}document.querySelectorAll('[data-filter-drawer-close]').forEach(function(el){el.addEventListener('click',closeFilterDrawer)});document.addEventListener('keydown',function(e){if(e.key==='Escape')closeFilterDrawer()});async function load(ids)`},
	// The test-drive section and the shot drawer need module-level state: the
	// ids of the last-loaded asset cards (so 开始测试分析 can pick three
	// samples without a round trip), the shot currently open in the drawer,
	// and the button that opened the collections modal (so a successful add
	// can flip that exact button to ✓ 已加入).
	{anchor: `let activeCollection='';`,
		replacement: `let activeCollection='',lastLoadedAssets=[],currentShot=null,addShotSourceBtn=null;`},
	// Every load() refreshes the URL-synced filter state: the active-filter
	// chips row and the filters-toggle count re-derive from the current
	// control values, and syncURL() rewrites the address bar so a reload
	// restores the exact query. The anchor is the legacy load() body opener;
	// later patches change the fetch below it but never this prefix.
	{anchor: `{library.innerHTML='<div class="loading">'+tdT('library.loadingTimeline')+'</div>';`,
		replacement: `{refreshFilterUI();syncURL();library.innerHTML='<div class="loading">'+tdT('library.loadingTimeline')+'</div>';`},
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
		replacement: `(ids&&!ids.length?Promise.resolve([]):fetch(ids?'/api/v1/assets?ids='+ids.map(encodeURIComponent).join(','):(activeCollection?'/api/v1/collections/'+encodeURIComponent(activeCollection)+'/assets?limit=300':'/api/v1/assets?limit=300'+filterQuery())).then(async r=>{if(!r.ok)throw Error(await tdApiErrorMessage(r));return r.json()}))`},
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
		replacement: `@media(max-width:980px){.filters{grid-template-columns:repeat(2,minmax(0,1fr))}}@media(max-width:900px){.shot-drawer-panel{width:100vw}.filter-drawer-panel{width:100vw}}@media(max-width:720px){.filters{grid-template-columns:repeat(2,minmax(0,1fr))}#filters-toggle{grid-column:1/-1}.filter-group{grid-template-columns:1fr}`},
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
	{anchor: `async function search(){const q=document.getElementById('q').value.trim();if(!q)return load();try{const ids=await fetch('/api/v1/search?q='+encodeURIComponent(q)).then(r=>r.ok?r.json():[]);load(Array.isArray(ids)?ids:[])}catch(e){library.innerHTML='<div class="empty error">'+esc(tdT('library.searchFailed'))+esc(e.message)+'</div>'}}`,
		replacement: `async function search(ev){if(ev&&ev.preventDefault)ev.preventDefault();const q=document.getElementById('q').value.trim();if(!q)return load();refreshFilterUI();syncURL();shotPage={query:q,offset:0,limit:40,items:[],hasMore:false,nextOffset:null,windowExhausted:false,loading:false};return runShotSearch(false)}
async function runShotSearch(append){if(shotPage.loading)return;shotPage.loading=true;const assetFilter={captured_from:isoDate('date-from'),captured_to:isoDatePlus1('date-to'),region_label:document.getElementById('region-filter').value.trim(),camera_model:document.getElementById('camera-filter').value,session_id:document.getElementById('session-filter').value,status:document.getElementById('status-filter').value};const hasAssetFilter=assetFilter.captured_from||assetFilter.captured_to||assetFilter.region_label||assetFilter.camera_model||assetFilter.session_id||assetFilter.status;try{const data=await fetch('/api/v1/search/shots',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({query:shotPage.query,mode:'auto',limit:shotPage.limit,offset:shotPage.offset,diversity:0.2,include_evidence:true,include_context:false,facets:filterFacets(),...(hasAssetFilter?{asset_filter:assetFilter}:{})})}).then(async r=>{if(!r.ok)throw Error(await tdApiErrorMessage(r));return r.json()});const page=data&&Array.isArray(data.results)?data.results:[];shotPage.items=append?shotPage.items.concat(page):page;shotPage.hasMore=!!(data&&data.has_more);shotPage.nextOffset=data&&typeof data.next_offset==='number'?data.next_offset:null;shotPage.windowExhausted=!!(data&&data.window_exhausted);renderShotResults(shotPage.items)}catch(e){library.innerHTML='<div class="empty error">'+tdT('library.searchFailed')+esc(e.message)+'</div>'}finally{shotPage.loading=false}}
async function loadMoreShots(){if(!shotPage.hasMore||shotPage.nextOffset==null)return;const btn=document.getElementById('shot-more-btn');if(btn){btn.disabled=true;btn.textContent=tdT('library.loadingMore')}shotPage.offset=shotPage.nextOffset;await runShotSearch(true)}
// window_exhausted is a different sentence from "that is everything": the page
// ended exactly on MaxSearchWindow, so more may exist beyond a boundary one
// request cannot cross. Saying "全部结果" there would be a claim the engine
// never made.
function shotPageFooter(){const shown=shotPage.items.length;if(!shown)return'';let html='<div class="shot-results-foot"><span class="muted">'+esc(tdPlural('library.shotResultsShown',shown))+'</span>';if(shotPage.hasMore){html+='<button type="button" class="btn" id="shot-more-btn" onclick="loadMoreShots()">'+esc(tdT('library.loadMore'))+'</button>'}else if(shotPage.windowExhausted){html+='<span class="muted">'+esc(tdT('library.windowExhausted'))+'</span>'}else{html+='<span class="muted">'+esc(tdT('library.allShown'))+'</span>'}return html+'</div>'}
function clearSearch(){const q=document.getElementById('q');if(q)q.value='';load()}`},
	// The legacy script binds Enter on #q itself; the search bar now carries
	// the onsubmit attribute, so the duplicate listener would fire search()
	// twice per Enter.
	{anchor: `document.getElementById('q').addEventListener('keydown',e=>{if(e.key==='Enter')search()});`, replacement: ``},
	// Timeline shots are interactive buttons: reset the button chrome on .shot
	// (padding/font/color) so absolute positioning and the tone chip are
	// unchanged, and switch the cursor to pointer.
	{anchor: `.tickrule-span{position:absolute;top:0;bottom:0;border-right:1px solid var(--surface);background:var(--span-fill);display:flex;align-items:center;padding:0 8px;overflow:hidden}`,
		replacement: `.tickrule-span{position:absolute;top:0;bottom:0;border-right:1px solid var(--surface);background:var(--span-fill);display:flex;align-items:center;padding:0 8px;overflow:hidden;border-left:0;border-top:0;border-bottom:0;font:inherit;color:inherit;text-align:left;cursor:pointer}`},
	// Timeline blocks are real buttons carrying the shot payload the drawer
	// opens with (no second round trip) and an aria-label for keyboard and
	// screen-reader users: description + exact time range.
	{anchor: `<div class="tickrule-span is-possible" style="left:'+left.toFixed(3)+'%;width:'+width.toFixed(3)+'%" title="'+esc(title)+'">`,
		replacement: `<button type="button" class="tickrule-span is-possible" style="left:'+left.toFixed(3)+'%;width:'+width.toFixed(3)+'%" aria-label="'+esc(tdT('library.ariaTimeRange',{desc:description,start:fmt(start),end:fmt(end)}))+'" title="'+esc(title)+'">`},
	// Timeline blocks become clickable shot evidence: each carries its own
	// row (the same payload the drawer opens with) so the drawer never needs
	// a second round trip.
	{anchor: `title="'+esc(title)+'"><span>'+esc(description)+'</span></div>'`,
		replacement: `data-shot="'+encodeURIComponent(JSON.stringify(s))+'" data-asset="'+esc(x.id)+'" data-filename="'+esc(x.filename||'')+'" title="'+esc(title)+'"><span>'+esc(description)+'</span></button>'`},
	// An asset card with no shots yet is a browse-mode dead end; the same
	// test-drive coaching the search-empty state offers (below) goes here so
	// the first-run library coaches from both modes. The status and chips
	// boxes are class-based because every such card renders one.
	{anchor: `if(!shots.length)return '<div class="timeline-empty">'+tdT('library.timelineEmpty')+'</div>';`,
		replacement: `if(!shots.length)return '<div class="timeline-empty"><span>'+esc(tdT('library.timelineEmptyTitle'))+'</span><button class="shot-add" onclick="testDriveStart(this)">'+esc(tdT('library.timelineEmptyTry'))+'</button><div class="test-drive-status"></div><div class="test-drive-chips"></div></div>';`},
	// The drawer script lives inside the page's one <script> block (function
	// declarations hoist, so search() can call renderShotResults and the
	// blocks' delegated click handler is wired after load()).
	{anchor: `load();</script>`, replacement: `const _cp=new URLSearchParams(location.search).get('collection');if(_cp)activeCollection=_cp;loadLibrary()` + shotDrawerScript + shotSelectionScript + `</script>`},
}

// filtersRowHTML is the high-frequency filter row: date range, status and the
// saved-view/collection selector, plus the filters-toggle button that opens
// the filter drawer. The heavy vocabulary facets (shot size, camera motion,
// audio type, quality, usable-as), the duration bounds and the region/camera/
// session/asset-type controls live in the drawer (see filterDrawerHTML) so
// the default view stays light. The drawer reuses these same control ids, so
// filterQuery(), filterFacets(), clearFilters() and saveCurrentView() keep
// working unchanged — this is a move, never a rename.
func filtersRowHTML() string {
	return `<section class="filters" aria-label="[[i18n:library.filter.aria]]"><div><label for="date-from">[[i18n:library.filter.dateFrom]]</label><input id="date-from" type="date" onchange="refreshResults()"></div><div><label for="date-to">[[i18n:library.filter.dateTo]]</label><input id="date-to" type="date" onchange="refreshResults()"></div><div><label for="status-filter">[[i18n:library.filter.status]]</label><select id="status-filter" onchange="refreshResults()"><option value="">[[i18n:library.filter.statusEmpty]]</option><option value="ready">[[i18n:status.ready]]</option><option value="processing">[[i18n:status.processing]]</option><option value="queued">[[i18n:status.queued]]</option><option value="failed">[[i18n:status.failed]]</option><option value="discovered">[[i18n:status.discovered]]</option><option value="missing">[[i18n:status.missing]]</option></select></div><div><label for="collection-filter">[[i18n:library.filter.collection]]</label><select id="collection-filter" onchange="loadCollection(this.value)"><option value="">[[i18n:library.filter.collectionNone]]</option></select><p class="muted collections-hint" id="collections-hint" hidden>[[i18n:library.filter.collectionsHint]]</p><button class="shot-add" onclick="saveCurrentView()" title="[[i18n:library.filter.saveCurrentTitle]]">[[i18n:library.filter.saveCurrent]]</button></div><button type="button" id="filters-toggle" class="btn" onclick="openFilterDrawer()">[[i18n:library.filter.filters]]</button></section>`
}

// searchBarHTML is the shot-first search entry: it lives immediately below
// the page header and above the filters so a query is the first thing an
// editor meets, and it renders shot-level results (see search() and
// renderShotResults in the page script). It is a real form so Enter submits
// and the button gives keyboard/screen-reader users an explicit activation
// point — the single #q on the page.
func searchBarHTML() string {
	return `<form class="searchbar" role="search" onsubmit="search(event)"><input id="q" name="q" placeholder="[[i18n:library.searchPlaceholder]]" aria-label="[[i18n:library.searchAria]]"><button type="submit" class="search-submit">[[i18n:common.search]]</button><button type="button" class="search-clear" onclick="clearSearch()">[[i18n:common.clear]]</button></form>`
}

// activeChipsHTML is the row that lists every enabled filter as a removable
// chip (see refreshFilterUI/filterChips in the page script). It starts hidden
// and empty; the first load() fills it once any filter is active.
func activeChipsHTML() string {
	return `<div id="active-chips" class="active-chips" aria-label="[[i18n:library.filter.aria]]" hidden></div>`
}

// filterPanelsHTML is the replacement for the <section id="library"> anchor:
// the search bar, the active-filter chips, the high-frequency filter row, the
// filter drawer overlay and the processing summary. The facet options are
// generated from the normalize vocabularies, never typed into the JS.
func filterPanelsHTML() string {
	return searchBarHTML() + activeChipsHTML() + filtersRowHTML() + filterDrawerHTML() + `<div id="processing-summary" class="processing-summary" aria-label="[[i18n:library.processingSummaryAria]]"></div>` + `<div id="root-health" class="root-health" aria-label="[[i18n:library.rootsStatusAria]]"></div>`
}

// filterDrawerHTML is the right-side slide-out overlay that holds the
// semantic filters: a SHOT group (the five shot-level vocabulary facets and
// the duration bounds, which resolve through the shot's asset) and an ASSET
// group (asset type, camera, region, session). It is modeled on the shot
// drawer but keeps its own .filter-drawer class so the mobile 100vw handling
// never clashes. Apply closes the overlay and reloads; Clear all resets every
// control. The control ids are the same ones the old top row and 高级筛选
// block rendered, so the query/collection logic keeps working unchanged.
func filterDrawerHTML() string {
	var b strings.Builder
	b.WriteString(`<div class="filter-drawer" id="filter-drawer" data-filter-drawer role="dialog" aria-modal="true" aria-hidden="true"><div class="filter-drawer-backdrop" data-filter-drawer-close></div><aside class="filter-drawer-panel" aria-label="[[i18n:library.filter.filters]]"><div class="filter-drawer-head"><span class="filter-drawer-title">[[i18n:library.filter.filters]]</span><button type="button" class="filter-drawer-close" data-filter-drawer-close aria-label="[[i18n:library.closePreview]]">×</button></div><div class="filter-drawer-body">`)
	b.WriteString(`<div class="filter-group" role="group" aria-labelledby="filter-group-shot"><div class="filter-group-title" id="filter-group-shot">[[i18n:library.filter.group.shot]]</div>`)
	for _, f := range facetFields[1:] {
		b.WriteString(facetSelectHTML(f.id, f.nameKey, f.emptyKey, f.values, f.multiple))
	}
	// Duration is entered in seconds, the unit an editor thinks in, and the JS
	// converts to milliseconds before sending; empty means unset.
	b.WriteString(`<div><label for="min-duration">[[i18n:library.filter.minDuration]]</label><input id="min-duration" type="number" min="0" step="any" inputmode="numeric" placeholder="[[i18n:library.filter.unlimited]]" onchange="refreshResults()"></div><div><label for="max-duration">[[i18n:library.filter.maxDuration]]</label><input id="max-duration" type="number" min="0" step="any" inputmode="numeric" placeholder="[[i18n:library.filter.unlimited]]" onchange="refreshResults()"></div></div>`)
	b.WriteString(`<div class="filter-group" role="group" aria-labelledby="filter-group-asset"><div class="filter-group-title" id="filter-group-asset">[[i18n:library.filter.group.asset]]</div>`)
	b.WriteString(facetSelectHTML(facetFields[0].id, facetFields[0].nameKey, facetFields[0].emptyKey, facetFields[0].values, facetFields[0].multiple))
	b.WriteString(`<div><label for="camera-filter">[[i18n:library.filter.camera]]</label><input id="camera-filter" placeholder="[[i18n:library.filter.cameraPlaceholder]]" onchange="refreshResults()"></div><div><label for="region-filter">[[i18n:library.filter.region]]</label><input id="region-filter" placeholder="[[i18n:library.filter.regionPlaceholder]]" onchange="refreshResults()"></div><div><label for="session-filter">[[i18n:library.filter.session]]</label><select id="session-filter" onchange="refreshResults()"><option value="">[[i18n:library.filter.sessionEmpty]]</option></select></div></div>`)
	b.WriteString(`</div><div class="filter-drawer-foot"><button type="button" class="link-button" id="filters-clear" onclick="clearFilters()">[[i18n:library.filter.clear]]</button><button type="button" class="btn" id="filters-apply" onclick="applyFilters()">[[i18n:library.filter.apply]]</button></div></aside></div>`)
	return b.String()
}

func facetSelectHTML(id, nameKey, emptyKey string, values []string, multiple bool) string {
	var b strings.Builder
	b.WriteString(`<div><label for="` + id + `">[[i18n:` + nameKey + `]]</label><select id="` + id + `" onchange="refreshResults()"`)
	if multiple {
		b.WriteString(` multiple size="4"`)
	}
	b.WriteString(`>`)
	if !multiple {
		b.WriteString(`<option value="">[[i18n:` + emptyKey + `]]</option>`)
	}
	for _, v := range values {
		fmt.Fprintf(&b, `<option value="%s">[[i18n:%s]]</option>`, v, facetLabels[v])
	}
	b.WriteString(`</select></div>`)
	return b.String()
}

// shotDrawerHTML is the overlay that turns a timeline shot into something an
// editor can actually watch: the proxy seeks to the shot's start and plays
// its range, while the shot's own evidence (description, objects, actions,
// tags, mood) is shown beside it. It is an overlay, never a navigation, so
// the query, filters and scroll position survive. Content order: video,
// time range, description, collection actions, similar shots, detailed
// evidence, then metadata (fields + chips).
func shotDrawerHTML() string {
	return `<div class="shot-drawer" id="shot-drawer" data-shot-drawer role="dialog" aria-modal="true" aria-hidden="true"><div class="shot-drawer-backdrop" data-drawer-close></div><aside class="shot-drawer-panel" aria-label="[[i18n:library.drawerAria]]"><div class="shot-drawer-head"><button class="shot-drawer-close" data-drawer-close aria-label="[[i18n:library.closePreview]]">×</button></div><div class="shot-drawer-body"><video id="shot-video" data-shot-video controls autoplay playsinline></video><div class="shot-drawer-time" id="shot-drawer-time">—</div><p class="shot-drawer-desc" id="shot-drawer-desc">—</p><div class="shot-drawer-actions"><button id="copy-timecode-btn" onclick="copyTimecode(this)">[[i18n:library.copyTimecode]]</button><button onclick="loadSimilarShots()">[[i18n:library.similarShots]]</button><button class="shot-add" onclick="addShotToCollection(this)">[[i18n:library.addToSelection]]</button></div><div class="shot-drawer-similar" id="shot-drawer-similar"></div><div class="shot-drawer-evidence" id="shot-drawer-evidence"></div><div class="shot-drawer-fields" id="shot-drawer-fields"></div><div class="chips" id="shot-drawer-chips"></div></div></aside></div>`
}

// shotDrawerScript is appended inside the page's single script block. It
// wires the timeline blocks and the shot-result cards to the drawer and
// renders the shot-first result list (search() calls renderShotResults).
// shotEvidence folds the first three non-negated marks into the card and the
// drawer, hiding the rest behind a Show evidence toggle (id evidence-more-…).
const shotDrawerScript = `
function shotChips(shot){const chips=[];['tags','objects','actions','mood'].forEach(k=>{(shot[k]||[]).forEach(v=>chips.push('<span class="chip">'+esc(v)+'</span>'))});return chips.join('')}
function shotEvidence(shot){const ev=(shot.evidence||[]).filter(e=>!e.negated);if(!ev.length)return'';const mark=function(e){const label=esc(e.constraint);if(e.state==='confirmed')return '<span class="ev-confirmed">'+label+' ✓</span>';if(e.state==='possible')return '<span class="ev-possible">'+label+' '+tdT('library.possible')+'</span>';if(e.state==='contradicted')return '<span class="ev-contradicted">'+label+' ✗</span>';return '<span class="ev-unknown">'+label+' '+tdT('library.unconfirmed')+'</span>'};const visible=ev.slice(0,3).map(mark).join(' ');const rest=ev.slice(3);let more='';if(rest.length){const uid='evidence-more-'+Math.random().toString(36).slice(2,8);more=' <span id="'+uid+'" class="evidence-more" hidden>'+rest.map(mark).join(' ')+'</span> <button type="button" class="link-button" data-evidence-more="'+uid+'" onclick="toggleEvidence(this)">'+esc(tdT('library.showEvidence'))+'</button>'}return '<div class="shot-evidence">'+tdT('library.whyHit')+' '+visible+more+'</div>'}
function toggleEvidence(btn){const span=document.getElementById(btn.dataset.evidenceMore);if(!span)return;span.hidden=false;btn.hidden=true}
function shotResultCard(s){const score=Math.round((s.score||0)*100);return '<article class="shot-result" role="button" tabindex="0" data-shot-result data-shot="'+encodeURIComponent(JSON.stringify(s))+'" data-asset="'+esc(s.asset_id)+'" data-filename="'+esc(s.filename||'')+'" onclick="openShotDrawer(JSON.parse(decodeURIComponent(this.dataset.shot)),this.dataset.asset,this.dataset.filename)" onkeydown="if(event.target===this&&(event.key===\'Enter\'||event.key===\' \')){event.preventDefault();openShotDrawer(JSON.parse(decodeURIComponent(this.dataset.shot)),this.dataset.asset,this.dataset.filename)}"><span class="thumb-wrap"><img class="thumb" loading="lazy" src="/api/v1/assets/'+encodeURIComponent(s.asset_id)+'/thumbnail" alt="'+tdT('library.shotThumbAlt')+'" onerror="this.style.visibility=\'hidden\'"><span class="shot-result-tc">'+fmt(s.start_ms)+' → '+fmt(s.end_ms)+'</span></span><div class="shot-result-main"><div class="shot-result-head"><b class="shot-result-file">'+esc(s.filename||s.asset_id)+'</b><span class="shot-result-time">'+fmt(s.start_ms)+' — '+fmt(s.end_ms)+'</span><span class="status-pill">'+esc(tdT('library.matchPct',{pct:score}))+'</span></div><p class="shot-result-desc">'+esc(s.description||'')+'</p>'+shotEvidence(s)+'<div class="chips">'+shotChips(s)+'</div></div><div class="shot-result-foot"><button class="shot-add" data-add-shot onclick="event.stopPropagation();addShotToCollection(this)">'+tdT('library.addToCollection')+'</button></div></article>'}
function renderShotResults(shots){if(!shots.length){library.innerHTML='<div class="empty">'+esc(tdT('library.noMatch'))+'</div>'+(lastLoadedAssets&&lastLoadedAssets.length?'<div class="test-drive-box" id="test-drive-box"><div class="group-title">'+esc(tdT('library.timelineEmptyTry'))+'</div><p class="muted">'+esc(tdT('library.testDriveIntro'))+'</p><button class="shot-add" onclick="testDriveStart()">'+esc(tdT('library.timelineEmptyTry'))+'</button><div class="test-drive-status"></div><div class="test-drive-chips"></div></div>':'');return}library.innerHTML='<div class="shot-results-head"><span class="eyebrow">'+esc(tdT('library.shotResultsEyebrow'))+'</span><button class="link-button" onclick="load()">'+esc(tdT('library.backToBrowse'))+'</button></div>'+shots.map(shotResultCard).join('')+shotPageFooter()}
function openShotDrawer(shot,assetId,filename){currentShot=shot;const d=document.getElementById('shot-drawer');if(!d)return;const s=Number(shot.start_ms)||0,e=Number(shot.end_ms)||s;document.getElementById('shot-drawer-time').textContent=fmt(s)+' — '+fmt(e)+' · '+tdT('library.duration',{dur:fmt(e-s)});document.getElementById('shot-drawer-desc').textContent=shot.description||tdT('library.noDescription');document.getElementById('shot-drawer-chips').innerHTML=shotChips(shot);const evidenceBox=document.getElementById('shot-drawer-evidence');if(evidenceBox)evidenceBox.innerHTML=shotEvidence(shot);const fields=[];if(shot.confidence!=null)fields.push('<span><b>'+tdT('library.confidence')+'</b> '+esc(String(shot.confidence))+'</span>');document.getElementById('shot-drawer-fields').innerHTML=fields.join('');const similar=document.getElementById('shot-drawer-similar');if(similar)similar.innerHTML='';// The proxy fragment is clamped to one minute past the shot start and rounded to whole seconds: some browsers treat a huge or sub-second-precise fragment range as a request to load the whole asset and hang the drawer's playback.

const clampEnd=Math.min(e,s+60000);const v=document.getElementById('shot-video');v.src='/api/v1/assets/'+encodeURIComponent(assetId)+'/proxy#t='+Math.floor(s/1000)+','+Math.ceil(clampEnd/1000);tdOpenSurface('shot-drawer',null,null);v.play().catch(function(){})}
function closeShotDrawer(){const d=document.getElementById('shot-drawer');if(!d)return;tdCloseSurface('shot-drawer');const v=document.getElementById('shot-video');if(v){v.pause();v.removeAttribute('src')}}
function openShotDrawerInit(){document.addEventListener('click',function(e){const block=e.target.closest('.tickrule-span');if(!block)return;const raw=block.dataset.shot;let shot={};if(raw){try{shot=JSON.parse(decodeURIComponent(raw))}catch(_){shot={}}}openShotDrawer(shot,block.dataset.asset||'',block.dataset.filename||'')});document.querySelectorAll('[data-drawer-close]').forEach(function(el){el.addEventListener('click',closeShotDrawer)});document.addEventListener('keydown',function(e){if(e.key==='Escape')closeShotDrawer()})}
openShotDrawerInit();`

// shotSelectionScript is the selection loop: 加入收藏 (card and drawer),
// the drawer's 复制时间码 and 相似镜头 actions, the 保存当前筛选 view
// builder, and the test-drive coaching on both empty states. Every fetch
// goes through tdApiErrorMessage; the modal, drawer and empty-state elements
// use ids that collide with nothing else in the page.
const shotSelectionScript = `
function fmtTimecode(ms){ms=Math.max(0,Math.floor(Number(ms)||0));const h=Math.floor(ms/3600000),m=Math.floor(ms%3600000/60000),s=Math.floor(ms%60000/1000),mm=ms%1000;return h+':'+String(m).padStart(2,'0')+':'+String(s).padStart(2,'0')+'.'+String(mm).padStart(3,'0')}
async function copyTimecode(btn){if(!currentShot)return;const start=Number(currentShot.start_ms)||0,end=Math.max(start,Number(currentShot.end_ms)||start);const text=start===end?fmtTimecode(start):fmtTimecode(start)+'–'+fmtTimecode(end);try{if(!navigator.clipboard||!navigator.clipboard.writeText)throw Error('no clipboard api');await navigator.clipboard.writeText(text)}catch(_){const ta=document.createElement('textarea');ta.value=text;ta.style.position='fixed';ta.style.opacity='0';document.body.appendChild(ta);ta.select();try{document.execCommand('copy')}catch(_){}ta.remove()}if(btn)btn.textContent=tdT('library.copied')}
async function loadSimilarShots(){const box=document.getElementById('shot-drawer-similar');if(!box)return;const shotId=currentShot&&(currentShot.shot_id||currentShot.id);if(!shotId)return;box.innerHTML='<div class="muted">'+esc(tdT('library.loadingSimilar'))+'</div>';try{const fq=filterQuery();const r=await fetch('/api/v1/shots/'+encodeURIComponent(shotId)+'/similar'+(fq?'?'+fq.slice(1):''));if(!r.ok)throw Error(await tdApiErrorMessage(r));const hits=await r.json();const items=Array.isArray(hits)?hits:[];box.innerHTML=items.length?'<div class="group-title">'+tdT('library.similarShots')+'</div>'+items.map(function(h){return '<div class="similar-item" data-s="'+encodeURIComponent(JSON.stringify({shot_id:h.id,asset_id:h.asset_id,filename:h.filename||'',start_ms:h.start_ms,end_ms:h.end_ms,description:h.description||'',confidence:h.confidence}))+'" onclick="openSimilarShot(JSON.parse(decodeURIComponent(this.dataset.s)))"><b>'+fmt(h.start_ms)+' — '+fmt(h.end_ms)+'</b>'+esc(h.description||'')+'</div>'}).join(''):'<div class="muted">'+tdT('library.noSimilar')+'</div>'}catch(_){box.innerHTML=''}}
function openSimilarShot(s){openShotDrawer(s,s.asset_id,s.filename||'')}
function addShotToCollection(btn){let shot=null;const host=btn?btn.closest('[data-shot]'):null;if(host&&host.dataset.shot){try{shot=JSON.parse(decodeURIComponent(host.dataset.shot))}catch(_){shot=null}}if(!shot)shot=currentShot;if(!shot)return;addShotSourceBtn=btn||null;addShotModal(shot)}
function addShotModal(shot){let overlay=document.getElementById('add-shot-modal');if(!overlay){overlay=document.createElement('div');overlay.id='add-shot-modal';overlay.className='shot-modal';overlay.innerHTML='<div class="shot-modal-card"><h3>'+esc(tdT('library.addToSelection'))+'</h3><div class="shot-modal-error"></div><div class="shot-modal-list"></div><div class="shot-modal-new"><input class="shot-modal-name" placeholder="'+esc(tdT('library.addShotModal.newName'))+'"><button class="shot-add" onclick="createAndAddCollection()">'+esc(tdT('library.addShotModal.createAndAdd'))+'</button></div><div style="text-align:right;margin-top:12px"><button class="shot-add" onclick="closeAddShotModal()">'+esc(tdT('common.cancel'))+'</button></div></div>';document.body.appendChild(overlay)}overlay.setAttribute('role','dialog');overlay.setAttribute('aria-modal','true');overlay.dataset.shot=encodeURIComponent(JSON.stringify(shot));tdOpenSurface('add-shot-modal',addShotSourceBtn,'input');(async function(){const listEl=overlay.querySelector('.shot-modal-list'),errEl=overlay.querySelector('.shot-modal-error');errEl.textContent='';listEl.innerHTML='<div class="muted">'+tdT('library.addShotModal.loading')+'</div>';try{const r=await fetch('/api/v1/collections',{headers:authHeaders()});if(!r.ok){if(r.status===401){errEl.textContent=tdT('common.loginRequired');return}throw Error(await tdApiErrorMessage(r))}const list=await r.json();const items=Array.isArray(list)?list:[];listEl.innerHTML=items.length?items.map(function(c){return '<div class="shot-modal-row"><span>'+esc(c.name)+'</span><span class="muted">'+tdPlural('library.addShotModal.shotCount',c.shot_count)+'</span><button class="shot-add" data-cid="'+esc(c.id)+'" onclick="addToCollection(this.dataset.cid)">'+tdT('library.addShotModal.add')+'</button></div>'}).join(''):'<div class="muted">'+tdT('library.addShotModal.empty')+'</div>'}catch(e){errEl.textContent=tdT('library.addShotModal.readFailed')+e.message}}())}
function closeAddShotModal(){tdCloseSurface('add-shot-modal')}
async function addToCollection(cid){const overlay=document.getElementById('add-shot-modal');if(!overlay)return;let shot={};try{shot=JSON.parse(decodeURIComponent(overlay.dataset.shot))}catch(_){return}const shotId=shot.shot_id||shot.id;if(!shotId)return;try{const r=await fetch('/api/v1/collections/'+encodeURIComponent(cid)+'/shots',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({shot_id:shotId})});if(!r.ok){if(r.status===401){overlay.querySelector('.shot-modal-error').textContent=tdT('common.loginRequired');return}throw Error(await tdApiErrorMessage(r))}closeAddShotModal();if(addShotSourceBtn){addShotSourceBtn.textContent=tdT('library.copiedAdded');addShotSourceBtn.classList.add('ok')}}catch(e){overlay.querySelector('.shot-modal-error').textContent=tdT('library.addShotModal.addFailed')+e.message}}
async function createAndAddCollection(){const overlay=document.getElementById('add-shot-modal');if(!overlay)return;const input=overlay.querySelector('.shot-modal-name'),errEl=overlay.querySelector('.shot-modal-error');const name=input.value.trim();if(!name){errEl.textContent=tdT('library.addShotModal.nameRequired');return}let shot={};try{shot=JSON.parse(decodeURIComponent(overlay.dataset.shot))}catch(_){}const shotId=shot.shot_id||shot.id;try{const r=await fetch('/api/v1/collections',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({name:name})});if(!r.ok){if(r.status===401){errEl.textContent=tdT('common.loginRequired');return}throw Error(await tdApiErrorMessage(r))}const created=await r.json();if(shotId){const r2=await fetch('/api/v1/collections/'+encodeURIComponent(created.id)+'/shots',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({shot_id:shotId})});if(!r2.ok&&r2.status!==409){if(r2.status===401){errEl.textContent=tdT('common.loginRequired');return}throw Error(await tdApiErrorMessage(r2))}}closeAddShotModal();resetCollectionSelect();await loadCollections();if(addShotSourceBtn){addShotSourceBtn.textContent=tdT('library.copiedAdded');addShotSourceBtn.classList.add('ok')}}catch(e){errEl.textContent=tdT('library.addShotModal.createFailed')+e.message}}
function resetCollectionSelect(){const select=document.getElementById('collection-filter');if(!select)return;select.innerHTML='<option value="">'+esc(tdT('library.filter.collectionNone'))+'</option>'}
function isoDate(id){const v=document.getElementById(id).value;return v?new Date(v+'T00:00:00Z').toISOString():undefined}
function isoDatePlus1(id){const v=document.getElementById(id).value;if(!v)return undefined;const d=new Date(v+'T00:00:00Z');d.setUTCDate(d.getUTCDate()+1);return d.toISOString()}
async function saveCurrentView(){const name=prompt(tdT('library.saveViewPrompt'));if(name===null)return;const filter={captured_from:isoDate('date-from'),captured_to:isoDatePlus1('date-to'),region_label:document.getElementById('region-filter').value.trim(),camera_model:document.getElementById('camera-filter').value,session_id:document.getElementById('session-filter').value,status:document.getElementById('status-filter').value};Object.assign(filter,filterFacets());try{const r=await fetch('/api/v1/collections',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({name:name,filter:filter})});if(!r.ok){if(r.status===401)throw Error(tdT('common.loginRequired'));throw Error(await tdApiErrorMessage(r))}resetCollectionSelect();await loadCollections();alert(tdT('library.saveViewSaved',{name:name}))}catch(e){alert(tdT('library.saveViewFailed')+e.message)}}
async function testDriveStart(btn){let ids=(lastLoadedAssets||[]).slice(0,3);const box=btn?btn.parentElement:document.getElementById('test-drive-box');if(!box||!ids.length)return;const status=box.querySelector('.test-drive-status'),chipsBox=box.querySelector('.test-drive-chips');const b=btn||box.querySelector('button');if(b){b.disabled=true;b.textContent=tdT('library.testDrive.starting')}try{const r=await fetch('/api/v1/test-drive',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({asset_ids:ids})});if(!r.ok){if(r.status===401)throw Error(tdT('common.loginRequired'));throw Error(await tdApiErrorMessage(r))}if(status)status.innerHTML=tdT('library.testDrive.started',{link:'<a href="/progress">'+esc(tdT('library.testDrive.progressLink'))+'</a>'});testDriveChips(chipsBox,ids)}catch(e){if(status)status.textContent=tdT('library.testDrive.failed')+e.message}finally{if(b){b.disabled=false;b.textContent=tdT('library.testDrive.start')}}}
async function testDriveChips(box,ids){if(!box)return;try{const r=await fetch('/api/v1/test-drive/suggestions?assets='+ids.map(encodeURIComponent).join(','));if(!r.ok)return;const d=await r.json();const items=Array.isArray(d.suggestions)?d.suggestions:[];box.innerHTML=items.length?'<div class="group-title">'+esc(tdT('library.testDrive.trySearch'))+'</div><div class="chips">'+items.map(function(s){return '<button class="try-chip" data-q="'+esc(s)+'" onclick="document.getElementById(\'q\').value=this.dataset.q;search()">'+esc(s)+'</button>'}).join('')+'</div>':''}catch(_){box.innerHTML=''}}`

func enhanceLibraryPage(page string) string {
	for _, p := range libraryPagePatches {
		page = strings.Replace(page, p.anchor, p.replacement, 1)
	}
	return page
}
