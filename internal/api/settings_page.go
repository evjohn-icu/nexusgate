package api

import "net/http"

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.serveLocalizedPage(w, r, "/settings", settingsHTML)
}

// settingsHTML is the editable view of domain.PipelineThrottle. Like every
// other page here it is one inline constant with no build step, and the admin
// token lives only in the shell's input element -- never in browser storage.
//
// The page deliberately shows the Hub's own clock and time zone. The off-peak
// window is evaluated in the Hub's local time, so an operator configuring
// "01:00" from a laptop in another zone would otherwise have no way to tell that
// it means the Hub's 01:00 and not theirs.
//
// All product copy is localized: static HTML uses [[i18n:settings.*]] markers
// that serveLocalizedPage resolves server-side, and dynamic JavaScript copy
// calls the injected tdT/tdPlural/tdFormatNumber/tdApiErrorMessage helpers.
// The GB/MB suffixes next to the number inputs and the KB/MB/GB/TB units in
// fmtBytes are data and stay verbatim in every locale.
const settingsHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · [[i18n:settings.title]]</title><style>

.panel{padding:20px;margin-top:20px}
.panel h2{margin:0 0 6px}
.panel .hint{line-height:1.6;margin:0 0 16px}
.row{display:grid;grid-template-columns:220px 1fr;gap:14px;align-items:start;padding:12px 0;border-top:1px solid var(--rule)}
.row:first-of-type{border-top:0}
label{font-weight:700;color:var(--text)}
.row .note{color:var(--text-muted);font-size:12px;line-height:1.5;margin:6px 0 0}
.row input[type=number],.row input[type=time]{max-width:170px}
.actions{display:flex;gap:10px;align-items:center;margin-top:22px}
.status{display:none}
.state-strip{display:flex;flex-wrap:wrap;gap:10px;margin-top:4px}
@media(max-width:720px){.row{grid-template-columns:1fr}
}

</style></head><body><!--SHELL_HEADER--><main class="wrap">
<h1>[[i18n:settings.title]]</h1>
<p class="muted">[[i18n:settings.intro]]</p>
<section class="panel"><h2>[[i18n:settings.storageOverview]]</h2><p class="hint">[[i18n:settings.storageHint.rawLead]]<b>[[i18n:settings.storageHint.estimate]]</b>[[i18n:settings.storageHint.rawTail]]<b>[[i18n:settings.storageHint.regenerable]]</b>[[i18n:settings.storageHint.tail]]</p><div id="storage-rows"><span class="pill">[[i18n:common.loading]]</span></div></section>
<section class="panel"><h2>[[i18n:settings.currentState]]</h2><div id="state" class="state-strip"><span class="pill">[[i18n:common.loading]]</span></div></section>
<section class="panel"><h2>[[i18n:settings.loadControl]]</h2><p class="hint">[[i18n:settings.loadHint]]</p>
<div class="row"><label for="read-rate">[[i18n:settings.readRate]]</label><div><input id="read-rate" type="number" min="0" step="0.25" placeholder="0"><p class="note">[[i18n:settings.readRateHint.lead]]<b>[[i18n:settings.readRateHint.zero]]</b>[[i18n:settings.readRateHint.tail]]</p></div></div>
<div class="row"><label for="cooldown">[[i18n:settings.cooldown]]</label><div><input id="cooldown" type="number" min="0" max="3600" step="5" placeholder="0"><p class="note">[[i18n:settings.cooldownHint]]</p></div></div>
</section>
<section class="panel"><h2>[[i18n:settings.diskProtection]]</h2><p class="hint">[[i18n:settings.diskProtectionHint]]</p>
<div class="row"><label for="min-free-space">[[i18n:settings.minFreeSpace]]</label><div><input id="min-free-space" type="number" min="0" step="1" placeholder="0"> GB<p class="note">[[i18n:settings.minFreeSpaceHint.lead]]<b>[[i18n:settings.minFreeSpaceHint.zero]]</b>[[i18n:settings.minFreeSpaceHint.tail]]</p></div></div>
</section>
<section class="panel"><h2>[[i18n:settings.costGuide]]</h2><p class="hint">[[i18n:settings.costHint.lead]]<b>[[i18n:settings.costHint.zero]]</b></p>
<div class="row"><label for="daily-cost-guide">[[i18n:settings.dailyCostGuide]]</label><div><input id="daily-cost-guide" type="number" min="0" step="0.01" placeholder="0"><p class="note">[[i18n:settings.dailyCostHint.lead]]<b>[[i18n:settings.costZeroUnset]]</b></p></div></div>
<div class="row"><label for="monthly-cost-guide">[[i18n:settings.monthlyCostGuide]]</label><div><input id="monthly-cost-guide" type="number" min="0" step="0.01" placeholder="0"><p class="note">[[i18n:settings.monthlyCostHint.lead]]<b>[[i18n:settings.costZeroUnset]]</b></p></div></div>
</section>
<section class="panel"><h2>[[i18n:settings.offPeak]]</h2><p class="hint">[[i18n:settings.offPeakHint.lead]]<b id="server-zone">[[i18n:settings.serverZoneDefault]]</b>[[i18n:settings.offPeakHint.mid]]<b id="server-time">--:--</b>[[i18n:settings.offPeakHint.tail]]</p>
<div class="row"><label for="off-peak">[[i18n:settings.enableOffPeak]]</label><div><input id="off-peak" type="checkbox"><p class="note">[[i18n:settings.enableOffPeakHint]]</p></div></div>
<div class="row"><label for="off-start">[[i18n:settings.offPeakWindow]]</label><div><input id="off-start" type="time"> <input id="off-end" type="time"><p class="note">[[i18n:settings.offPeakWindowHint]]</p></div></div>
<div class="row"><label for="defer-mb">[[i18n:settings.deferAbove]]</label><div><input id="defer-mb" type="number" min="0" step="100" placeholder="0"> MB<p class="note">[[i18n:settings.deferAboveHint.lead]]<b>[[i18n:settings.deferAboveHint.zero]]</b>[[i18n:settings.deferAboveHint.tail]]</p></div></div>
<div class="row"><label for="immediate-mb">[[i18n:settings.immediateBelow]]</label><div><input id="immediate-mb" type="number" min="0" step="10" placeholder="0"> MB<p class="note">[[i18n:settings.immediateBelowHint]]</p></div></div>
</section>
<div class="actions"><button id="save" class="btn btn--primary" onclick="save()">[[i18n:common.save]]</button><button class="ghost" onclick="load()">[[i18n:settings.discard]]</button></div>
<div id="status" class="status"></div>
</main><script>
const MB=1048576;const GB=1073741824;
 function csrfToken(){const prefix='__Host-timingdex_csrf=';const item=document.cookie.split('; ').find(function(x){return x.indexOf(prefix)===0});return item?decodeURIComponent(item.slice(prefix.length)):''}
 function authHeaders(base){const headers=new Headers(base||{});const csrf=csrfToken();if(csrf)headers.set('X-CSRF-Token',csrf);return headers}
function esc(s){return String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function val(id){return document.getElementById(id).value.trim()}
function num(id){const v=parseFloat(val(id));return Number.isFinite(v)&&v>0?v:0}
function say(message,ok){const el=document.getElementById('status');el.className='status '+(ok?'ok':'bad');el.textContent=message}
function renderState(d){const t=d.throttle||{};const pills=[];
pills.push('<span class="pill">'+esc(tdT('settings.state.readRate'))+' '+(t.read_rate?esc(tdFormatNumber(t.read_rate))+'x':esc(tdT('settings.unlimited')))+'</span>');
pills.push('<span class="pill">'+esc(tdT('settings.state.wait'))+' '+(t.cooldown_seconds?esc(tdPlural('settings.state.waitSeconds',t.cooldown_seconds)):esc(tdT('settings.state.noWait')))+'</span>');
if(t.off_peak_enabled){pills.push('<span class="pill '+(d.off_peak_open?'live':'hold')+'">'+esc(tdT('settings.state.window'))+' '+esc(t.off_peak_start)+'–'+esc(t.off_peak_end)+(d.off_peak_open?' · '+esc(tdT('settings.state.open')):' · '+esc(tdT('settings.state.pending')))+'</span>')}
else{pills.push('<span class="pill">'+esc(tdT('settings.state.offPeakDisabled'))+'</span>')}
if(d.holding_above>0){pills.push('<span class="pill hold">'+esc(tdT('settings.state.holding',{size:tdFormatNumber(Math.round(d.holding_above/MB)),time:d.next_off_peak}))+'</span>')}
pills.push('<span class="pill">'+esc(tdPlural('settings.state.cooldownNow',d.cooldown_now_s))+'</span>');
if(t.minimum_free_space_bytes>0){pills.push('<span class="pill">'+esc(tdT('settings.state.diskFloor',{n:tdFormatNumber(Math.round(t.minimum_free_space_bytes/GB))}))+'</span>')}
if(t.daily_cost_guide>0){pills.push('<span class="pill">'+esc(tdT('settings.state.dailyCost',{n:tdFormatNumber(t.daily_cost_guide)}))+'</span>')}
if(t.monthly_cost_guide>0){pills.push('<span class="pill">'+esc(tdT('settings.state.monthlyCost',{n:tdFormatNumber(t.monthly_cost_guide)}))+'</span>')}
document.getElementById('state').innerHTML=pills.join('');
document.getElementById('server-time').textContent=d.server_time||'--:--';
document.getElementById('server-zone').textContent=d.server_zone||tdT('settings.serverZoneDefault')}
function fill(d){const t=d.throttle||{};
document.getElementById('read-rate').value=t.read_rate||0;
document.getElementById('cooldown').value=t.cooldown_seconds||0;
document.getElementById('off-peak').checked=!!t.off_peak_enabled;
document.getElementById('off-start').value=t.off_peak_start||'01:00';
document.getElementById('off-end').value=t.off_peak_end||'07:00';
document.getElementById('defer-mb').value=t.defer_above_bytes?Math.round(t.defer_above_bytes/MB):0;
document.getElementById('immediate-mb').value=t.immediate_max_bytes?Math.round(t.immediate_max_bytes/MB):0;
document.getElementById('min-free-space').value=t.minimum_free_space_bytes?Math.round(t.minimum_free_space_bytes/GB):0;
document.getElementById('daily-cost-guide').value=t.daily_cost_guide||0;
document.getElementById('monthly-cost-guide').value=t.monthly_cost_guide||0;
renderState(d)}
async function load(){try{const d=await fetch('/api/v1/pipeline/throttle',{headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await tdApiErrorMessage(r));return r.json()});fill(d);document.getElementById('status').className='status'}catch(e){say(tdT('settings.loadError',{message:e.message}),false)}}
async function save(){const b=document.getElementById('save');b.disabled=true;
const body={read_rate:num('read-rate'),cooldown_seconds:Math.round(num('cooldown')),off_peak_enabled:document.getElementById('off-peak').checked,off_peak_start:val('off-start')||'01:00',off_peak_end:val('off-end')||'07:00',defer_above_bytes:Math.round(num('defer-mb')*MB),immediate_max_bytes:Math.round(num('immediate-mb')*MB),minimum_free_space_bytes:Math.round(num('min-free-space')*GB),daily_cost_guide:num('daily-cost-guide'),monthly_cost_guide:num('monthly-cost-guide')};
try{const d=await fetch('/api/v1/pipeline/throttle',{method:'PUT',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify(body)}).then(async r=>{if(!r.ok)throw Error(await tdApiErrorMessage(r));return r.json()});fill(d);say(tdT('settings.saved'),true)}catch(e){say(tdT('settings.saveError',{message:e.message}),false)}finally{b.disabled=false}}
load();loadStorage();setInterval(renderStateRefresh,15000);
function fmtBytes(n){const v=Number(n)||0;if(v<1024)return tdFormatNumber(Math.round(v))+' B';const u=['KB','MB','GB','TB'];let i=0;let x=v;while(x>=1024&&i<u.length-1){x/=1024;i++}return tdFormatNumber(x.toFixed(1))+' '+u[i]}
async function loadStorage(){const rows=[['settings.storage.original','original_estimate_bytes','settings.storage.originalNote'],['settings.storage.derived','derived_bytes','settings.storage.derivedNote'],['settings.storage.thumbnails','thumbnail_bytes'],['settings.storage.audio','audio_bytes'],['settings.storage.sourceStaging','source_staging_bytes','settings.storage.sourceStagingNote'],['settings.storage.temporary','temporary_bytes','settings.storage.temporaryNote'],['settings.storage.database','database_bytes'],['settings.storage.freeDisk','free_disk_bytes'],['settings.storage.rebuildable','rebuildable_bytes','settings.storage.rebuildableNote']];
try{const d=await fetch('/api/v1/storage/overview',{headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await tdApiErrorMessage(r));return r.json()});document.getElementById('storage-rows').innerHTML=rows.map(([label,key,note])=>{const v=d[key];return '<div class="kv"><span class="k">'+esc(tdT(label))+'</span><span class="v">'+(v===undefined?'—':fmtBytes(v))+(note?'<div class="muted">'+esc(tdT(note))+'</div>':'')+'</span></div>'}).join('')}catch(e){document.getElementById('storage-rows').innerHTML='<span class="pill bad">'+esc(tdT('settings.storageLoadError',{message:e.message}))+'</span>'}}
async function renderStateRefresh(){try{const d=await fetch('/api/v1/pipeline/throttle',{headers:authHeaders()}).then(r=>r.ok?r.json():null);if(d)renderState(d)}catch(e){}}
</script></body></html>`
