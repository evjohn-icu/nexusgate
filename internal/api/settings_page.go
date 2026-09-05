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
//
// The page is grouped into four panels (Storage / Performance / Schedule /
// Cost) built from the shell's .panel-head/.panel-body and .form-row rules;
// the current-state strip keeps its own panel under Storage. The off-peak
// threshold rows ship hidden and are revealed by the enable-off-peak checkbox
// (setOffPeakRowsVisible). A sticky save bar appears only once the controls
// drift from the server's last value (serverSnapshot captured by fill() and
// compared by syncStickySave), and hides again on discard or a successful
// save -- the controls' .actions buttons ride the same fill() path.
const settingsHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>NexusGate · [[i18n:settings.title]]</title><style>

.panel-body .hint{line-height:1.6}
label{font-weight:600;color:var(--text)}
.form-row input[type=number],.form-row input[type=time]{max-width:170px}
.actions{display:flex;gap:10px;align-items:center;margin-top:22px}
.state-strip{display:flex;flex-wrap:wrap;gap:10px;margin-top:4px}
.sticky-save{position:fixed;left:0;right:0;bottom:0;z-index:35;display:flex;align-items:center;justify-content:space-between;gap:var(--s2);padding:var(--s3) var(--s5);background:var(--surface);border-top:1px solid var(--rule)}
.storage-bar{position:relative;height:14px;border:1px solid var(--rule);border-radius:var(--r-sm);background-color:var(--surface);background-image:repeating-linear-gradient(90deg,var(--graticule) 0 1px,transparent 1px 48px);overflow:hidden;margin-top:var(--s3)}
.storage-bar-fill{position:absolute;top:0;left:0;bottom:0;width:0;background:var(--span-fill);border-right:1px solid var(--surface)}
.storage-bar-scale{display:flex;justify-content:space-between;font-family:var(--font-data);font-size:11px;font-variant-numeric:tabular-nums;color:var(--text-faint);margin-top:4px}
@media(max-width:720px){.form-row{grid-template-columns:1fr}
}

</style></head><body><!--SHELL_HEADER--><main class="wrap">
<header class="pagehead"><h1>[[i18n:settings.title]]</h1><p class="muted">[[i18n:settings.intro]]</p></header>
<section class="panel"><div class="panel-head"><h2>[[i18n:settings.storage]]</h2></div><div class="panel-body">
<p class="hint">[[i18n:settings.storageHint.rawLead]]<b>[[i18n:settings.storageHint.estimate]]</b>[[i18n:settings.storageHint.rawTail]]<b>[[i18n:settings.storageHint.regenerable]]</b>[[i18n:settings.storageHint.tail]]</p>
<div id="storage-rows"><span class="pill">[[i18n:common.loading]]</span></div>
<div class="storage-bar" id="storage-bar"><div class="storage-bar-fill" id="storage-bar-fill"></div></div>
<div class="storage-bar-scale"><span>0</span><span id="storage-bar-pct">—</span></div>
</div></section>
<section class="panel"><div class="panel-head"><h2>[[i18n:settings.currentState]]</h2></div><div class="panel-body"><div id="state" class="state-strip"><span class="pill">[[i18n:common.loading]]</span></div></div></section>
<section class="panel"><div class="panel-head"><h2>[[i18n:settings.performance]]</h2></div><div class="panel-body">
<div class="form-row"><label for="read-rate">[[i18n:settings.readRate]]</label><div><input id="read-rate" type="number" min="0" step="0.25" placeholder="0"><p class="note">[[i18n:settings.readRateHint.lead]]<b>[[i18n:settings.readRateHint.zero]]</b>[[i18n:settings.readRateHint.tail]]</p></div></div>
<div class="form-row"><label for="cooldown">[[i18n:settings.cooldown]]</label><div><input id="cooldown" type="number" min="0" max="3600" step="5" placeholder="0"><p class="note">[[i18n:settings.cooldownHint]]</p></div></div>
<div class="form-row"><label for="min-free-space">[[i18n:settings.minFreeSpace]]</label><div><input id="min-free-space" type="number" min="0" step="1" placeholder="0"> GB<p class="note">[[i18n:settings.minFreeSpaceHint.lead]]<b>[[i18n:settings.minFreeSpaceHint.zero]]</b>[[i18n:settings.minFreeSpaceHint.tail]]</p></div></div>
</div></section>
<section class="panel"><div class="panel-head"><h2>[[i18n:settings.schedule]]</h2></div><div class="panel-body">
<p class="hint">[[i18n:settings.offPeakHint.lead]]<b id="server-zone">[[i18n:settings.serverZoneDefault]]</b>[[i18n:settings.offPeakHint.mid]]<b id="server-time">--:--</b>[[i18n:settings.offPeakHint.tail]]</p>
<div class="form-row"><label for="off-peak">[[i18n:settings.enableOffPeak]]</label><div><input id="off-peak" type="checkbox"><p class="note">[[i18n:settings.enableOffPeakHint]]</p></div></div>
<div class="form-row" hidden><label for="off-start">[[i18n:settings.offPeakWindow]]</label><div><input id="off-start" type="time"> <input id="off-end" type="time"><p class="note">[[i18n:settings.offPeakWindowHint]]</p></div></div>
<div class="form-row" hidden><label for="defer-mb">[[i18n:settings.deferAbove]]</label><div><input id="defer-mb" type="number" min="0" step="100" placeholder="0"> MB<p class="note">[[i18n:settings.deferAboveHint.lead]]<b>[[i18n:settings.deferAboveHint.zero]]</b>[[i18n:settings.deferAboveHint.tail]]</p></div></div>
<div class="form-row" hidden><label for="immediate-mb">[[i18n:settings.immediateBelow]]</label><div><input id="immediate-mb" type="number" min="0" step="10" placeholder="0"> MB<p class="note">[[i18n:settings.immediateBelowHint]]</p></div></div>
</div></section>
<section class="panel"><div class="panel-head"><h2>[[i18n:settings.cost]]</h2></div><div class="panel-body">
<p class="hint">[[i18n:settings.costHint.lead]]<b>[[i18n:settings.costHint.zero]]</b></p>
<div class="form-row"><label for="daily-cost-guide">[[i18n:settings.dailyCostGuide]]</label><div><input id="daily-cost-guide" type="number" min="0" step="0.01" placeholder="0"><p class="note">[[i18n:settings.dailyCostHint.lead]]<b>[[i18n:settings.costZeroUnset]]</b></p></div></div>
<div class="form-row"><label for="monthly-cost-guide">[[i18n:settings.monthlyCostGuide]]</label><div><input id="monthly-cost-guide" type="number" min="0" step="0.01" placeholder="0"><p class="note">[[i18n:settings.monthlyCostHint.lead]]<b>[[i18n:settings.costZeroUnset]]</b></p></div></div>
</div></section>
<div class="actions"><button id="save" class="btn btn--primary" onclick="save()">[[i18n:common.save]]</button><button class="ghost" onclick="load()">[[i18n:settings.discard]]</button></div>
<div id="status" class="status" role="status" aria-live="polite"></div>
</main>
<div id="sticky-save" class="sticky-save" hidden><span>[[i18n:settings.unsavedChanges]]</span><div><button type="button" class="ghost" id="discard-btn">[[i18n:settings.discard]]</button><button type="button" class="btn btn--primary" id="save-changes-btn">[[i18n:settings.saveChanges]]</button></div></div>
<script>
const MB=1048576;const GB=1073741824;
 function csrfToken(){const prefix='__Host-nexusgate_csrf=';const item=document.cookie.split('; ').find(function(x){return x.indexOf(prefix)===0});return item?decodeURIComponent(item.slice(prefix.length)):''}
 function authHeaders(base){const headers=new Headers(base||{});const csrf=csrfToken();if(csrf)headers.set('X-CSRF-Token',csrf);return headers}
function esc(s){return String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function val(id){return document.getElementById(id).value.trim()}
function num(id){const v=parseFloat(val(id));return Number.isFinite(v)&&v>0?v:0}
function say(message,ok){const el=document.getElementById('status');el.className='callout '+(ok?'callout--confirmed':'callout--contradicted');el.textContent=message}
let serverSnapshot=null;
function captureSnapshot(d){const t=d.throttle||{};serverSnapshot={read_rate:t.read_rate||0,cooldown_seconds:t.cooldown_seconds||0,off_peak_enabled:!!t.off_peak_enabled,off_peak_start:t.off_peak_start||'01:00',off_peak_end:t.off_peak_end||'07:00',defer_above_bytes:t.defer_above_bytes||0,immediate_max_bytes:t.immediate_max_bytes||0,minimum_free_space_bytes:t.minimum_free_space_bytes||0,daily_cost_guide:t.daily_cost_guide||0,monthly_cost_guide:t.monthly_cost_guide||0}}
function currentValues(){return{read_rate:num('read-rate'),cooldown_seconds:Math.round(num('cooldown')),off_peak_enabled:document.getElementById('off-peak').checked,off_peak_start:val('off-start')||'01:00',off_peak_end:val('off-end')||'07:00',defer_above_bytes:Math.round(num('defer-mb')*MB),immediate_max_bytes:Math.round(num('immediate-mb')*MB),minimum_free_space_bytes:Math.round(num('min-free-space')*GB),daily_cost_guide:num('daily-cost-guide'),monthly_cost_guide:num('monthly-cost-guide')}}
function valuesEqual(a,b){return a.read_rate===b.read_rate&&a.cooldown_seconds===b.cooldown_seconds&&a.off_peak_enabled===b.off_peak_enabled&&a.off_peak_start===b.off_peak_start&&a.off_peak_end===b.off_peak_end&&a.defer_above_bytes===b.defer_above_bytes&&a.immediate_max_bytes===b.immediate_max_bytes&&a.minimum_free_space_bytes===b.minimum_free_space_bytes&&a.daily_cost_guide===b.daily_cost_guide&&a.monthly_cost_guide===b.monthly_cost_guide}
function syncStickySave(){const bar=document.getElementById('sticky-save');if(!bar)return;bar.hidden=!serverSnapshot||valuesEqual(currentValues(),serverSnapshot)}
function setOffPeakRowsVisible(visible){['off-start','defer-mb','immediate-mb'].forEach(function(id){const el=document.getElementById(id);if(!el)return;const row=el.closest('.form-row');if(row)row.hidden=!visible})}
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
setOffPeakRowsVisible(!!t.off_peak_enabled);captureSnapshot(d);renderState(d);syncStickySave()}
async function load(){try{const d=await fetch('/api/v1/pipeline/throttle',{headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await tdApiErrorMessage(r));return r.json()});fill(d);const st=document.getElementById('status');st.className='status';st.textContent=''}catch(e){say(tdT('settings.loadError',{message:e.message}),false)}}
async function save(){const b=document.getElementById('save');b.disabled=true;
const body={read_rate:num('read-rate'),cooldown_seconds:Math.round(num('cooldown')),off_peak_enabled:document.getElementById('off-peak').checked,off_peak_start:val('off-start')||'01:00',off_peak_end:val('off-end')||'07:00',defer_above_bytes:Math.round(num('defer-mb')*MB),immediate_max_bytes:Math.round(num('immediate-mb')*MB),minimum_free_space_bytes:Math.round(num('min-free-space')*GB),daily_cost_guide:num('daily-cost-guide'),monthly_cost_guide:num('monthly-cost-guide')};
try{const d=await fetch('/api/v1/pipeline/throttle',{method:'PUT',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify(body)}).then(async r=>{if(!r.ok)throw Error(await tdApiErrorMessage(r));return r.json()});fill(d);say(tdT('settings.saved'),true)}catch(e){say(tdT('settings.saveError',{message:e.message}),false)}finally{b.disabled=false}}
['read-rate','cooldown','off-start','off-end','defer-mb','immediate-mb','min-free-space','daily-cost-guide','monthly-cost-guide'].forEach(function(id){const el=document.getElementById(id);if(!el)return;el.addEventListener('input',syncStickySave);el.addEventListener('change',syncStickySave)});
document.getElementById('off-peak').addEventListener('change',function(){setOffPeakRowsVisible(this.checked);syncStickySave()});
document.getElementById('discard-btn').addEventListener('click',function(){load()});
document.getElementById('save-changes-btn').addEventListener('click',function(){save()});
load();loadStorage();setInterval(renderStateRefresh,15000);
function fmtBytes(n){const v=Number(n)||0;if(v<1024)return tdFormatNumber(Math.round(v))+' B';const u=['KB','MB','GB','TB'];let i=0;let x=v;while(x>=1024&&i<u.length-1){x/=1024;i++}return tdFormatNumber(x.toFixed(1))+' '+u[i]}
async function loadStorage(){const rows=[['settings.storage.original','original_estimate_bytes','settings.storage.originalNote'],['settings.storage.derived','derived_bytes','settings.storage.derivedNote'],['settings.storage.rebuildable','rebuildable_bytes','settings.storage.rebuildableNote'],['settings.storage.freeDisk','free_disk_bytes']];
try{const d=await fetch('/api/v1/storage/overview',{headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await tdApiErrorMessage(r));return r.json()});document.getElementById('storage-rows').innerHTML=rows.map(([label,key,note])=>{const v=d[key];return '<div class="kv"><span class="k">'+esc(tdT(label))+'</span><span class="v">'+(v===undefined?'—':fmtBytes(v))+(note?'<div class="muted">'+esc(tdT(note))+'</div>':'')+'</span></div>'}).join('');renderStorageBar(d)}catch(e){document.getElementById('storage-rows').innerHTML='<span class="pill bad">'+esc(tdT('settings.storageLoadError',{message:e.message}))+'</span>'}}
function renderStorageBar(d){const used=(Number(d.original_estimate_bytes)||0)+(Number(d.derived_bytes)||0)+(Number(d.rebuildable_bytes)||0);const free=Number(d.free_disk_bytes)||0;const fillEl=document.getElementById('storage-bar-fill');const pctEl=document.getElementById('storage-bar-pct');if(!free||used<=0){if(fillEl)fillEl.style.width='0';if(pctEl)pctEl.textContent='—';return}const pct=Math.min(100,Math.round(used/free*100));if(fillEl)fillEl.style.width=pct+'%';if(pctEl)pctEl.textContent=tdFormatNumber(pct)+'%'}
async function renderStateRefresh(){try{const d=await fetch('/api/v1/pipeline/throttle',{headers:authHeaders()}).then(r=>r.ok?r.json():null);if(d)renderState(d)}catch(e){}}
</script></body></html>`
