package api

const setupHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Timingdex · [[i18n:setup.title]]</title>
<style>.panel{background:var(--raised);border:1px solid var(--rule);border-radius:15px;padding:20px;margin-bottom:16px}.panel-head{display:flex;justify-content:space-between;align-items:center;gap:12px;margin-bottom:8px}.panel-head h2{margin:0;font-size:18px}.pill{border-radius:99px;padding:3px 11px;font-size:12px;font-weight:800;background:var(--inset);color:var(--text-muted);white-space:nowrap}.pill.ok{background:var(--ev-confirmed-wash);color:var(--ev-confirmed)}.pill.warn{background:var(--ev-attention-wash);color:var(--ev-attention)}.env-row{display:flex;justify-content:space-between;gap:12px;padding:9px 0;border-bottom:1px solid var(--rule)}.env-row:last-child{border:0}.env-row b.ok{color:var(--ev-confirmed)}.env-row b.bad{color:var(--ev-contradicted)}.env-row b.warn{color:var(--ev-attention)}.env-actions{margin-top:12px}.status{margin-top:12px;padding:12px;border-radius:9px;background:var(--inset)}.ok{color:var(--ev-confirmed)}.bad{color:var(--ev-contradicted)}.note{margin:14px 0 0;font-size:13px}</style></head>
<body><!--SHELL_HEADER-->
<main class="wrap"><div class="muted">[[i18n:setup.subtitle]]</div><h1>[[i18n:setup.title]]</h1><p class="muted">[[i18n:setup.intro]]</p>
<section class="panel"><div class="panel-head"><h2>[[i18n:setup.section.env]]</h2><span class="pill" id="pill-env">…</span></div>
<div class="env-rows">
<div class="env-row"><span>FFmpeg</span><b id="env-ffmpeg">…</b></div>
<div class="env-row"><span>FFprobe</span><b id="env-ffprobe">…</b></div>
<div class="env-row"><span>ExifTool <em class="muted">[[i18n:setup.env.optional]]</em></span><b id="env-exiftool">…</b></div>
<div class="env-row"><span>[[i18n:setup.env.dataDirWritable]]</span><b id="env-datadir">…</b></div>
<div class="env-row"><span>[[i18n:setup.env.cacheWritable]]</span><b id="env-cache">…</b></div>
<div class="env-row"><span>[[i18n:setup.env.diskSpace]]</span><b id="env-disk">…</b></div>
<div class="env-row"><span>[[i18n:setup.env.dbHealth]]</span><b id="env-db">…</b></div>
</div>
<div class="env-actions"><button onclick="setupLoadStatus()">[[i18n:common.refresh]]</button></div>
<div id="setup-status" class="status">[[i18n:setup.checking]]</div>
</section>
<section class="panel"><div class="panel-head"><h2>[[i18n:setup.section.roots]]</h2><span class="pill" id="pill-roots">…</span></div>
<div id="roots-body" class="muted">[[i18n:setup.checking]]</div>
<p class="muted note">[[i18n:setup.note.readOnly]]</p>
</section>
<section class="panel"><div class="panel-head"><h2>[[i18n:setup.section.providers]]</h2><span class="pill" id="pill-providers">…</span></div>
<div id="providers-body" class="muted">[[i18n:setup.checking]]</div>
<p class="muted note">[[i18n:setup.note.providers]]</p>
</section>
<section class="panel"><div class="panel-head"><h2>[[i18n:setup.section.status]]</h2><span class="pill" id="pill-status">…</span></div>
<div id="status-body" class="muted">[[i18n:setup.checking]]</div>
</section>
<p class="muted note">[[i18n:setup.note.security]]</p>
</main>
<script>
// Names here stay unique from the shared shell block (getAdminToken,
// shellAuthHeaders, statusCell, refreshStatus), so injecting the shell cannot
// clash with this page's script.
function esc(s){return String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function setupPill(id,done){const el=document.getElementById(id);if(!el)return;el.className='pill '+(done?'ok':'warn');el.textContent=done?tdT('setup.pill.done'):tdT('setup.pill.pending')}
function setupLoadStatus(){
  const box=document.getElementById('setup-status');if(box){box.className='status';box.textContent=tdT('setup.checking')}
  fetch('/api/v1/setup/status').then(function(r){if(!r.ok)throw Error(String(r.status));return r.json()}).then(function(s){
    function row(id,ok,text){const el=document.getElementById(id);if(!el)return;el.className=ok?'ok':'bad';el.textContent=text}
    function opt(id,ok){const el=document.getElementById(id);if(!el)return;el.className=ok?'ok':'warn';el.textContent=ok?tdT('setup.env.ok'):tdT('setup.env.optionalWarn')}
    row('env-ffmpeg',!!s.ffmpeg,s.ffmpeg?tdT('setup.env.ok'):tdT('setup.env.fix'));
    row('env-ffprobe',!!s.ffprobe,s.ffprobe?tdT('setup.env.ok'):tdT('setup.env.fix'));
    opt('env-exiftool',!!s.exiftool);
    row('env-datadir',!!s.data_dir_writable,s.data_dir_writable?tdT('setup.env.ok'):tdT('setup.env.fix'));
    row('env-cache',!!s.cache_writable,s.cache_writable?tdT('setup.env.ok'):tdT('setup.env.fix'));
    const disk=document.getElementById('env-disk');if(disk){disk.className=s.free_disk_ok?'ok':'bad';disk.textContent=(s.free_disk_ok?tdT('setup.env.ok'):tdT('setup.env.fix'))+' · '+tdFormatNumber((s.free_disk_bytes||0)/1073741824)+' GiB'}
    row('env-db',!!s.db_healthy,s.db_healthy?tdT('setup.env.ok'):tdT('setup.env.fix'));
    setupPill('pill-env',!!s.ffmpeg&&!!s.ffprobe&&!!s.data_dir_writable&&!!s.cache_writable&&!!s.free_disk_ok&&!!s.db_healthy);
    const rootsN=s.root_count||0,assetN=s.asset_count||0;
    const roots=document.getElementById('roots-body');if(roots){roots.innerHTML=rootsN>0?'✓ <b>'+tdFormatNumber(rootsN)+'</b> '+tdPlural('setup.roots.count',rootsN)+tdT('setup.roots.assetOpen')+'<b>'+tdFormatNumber(assetN)+'</b> '+tdPlural('setup.roots.assets',assetN)+tdT('setup.roots.assetClose')+'<a href="/library-roots">'+esc(tdT('setup.roots.manage'))+'</a>':'<span class="muted">'+esc(tdT('setup.roots.empty'))+'</span><a class="button" href="/library-roots">'+esc(tdT('setup.roots.openWizard'))+'</a>'}setupPill('pill-roots',rootsN>0);
    const prov=document.getElementById('providers-body');if(prov){prov.innerHTML=s.provider_count>0?'✓ '+esc(tdT('setup.providers.done'))+' <b>'+tdFormatNumber(s.provider_count||0)+'</b> '+tdPlural('setup.providers.count',s.provider_count||0)+' <a href="/providers">'+esc(tdT('setup.providers.manage'))+'</a>':'<span class="muted">'+esc(tdT('setup.providers.empty'))+'</span><a class="button" href="/providers">'+esc(tdT('setup.providers.configure'))+'</a>'}setupPill('pill-providers',s.provider_count>0);
    const steps={'add_footage':['setup.steps.addFootage.lead','setup.steps.addFootage.button','/library-roots'],'configure_providers':['setup.steps.configureProviders.lead','setup.steps.configureProviders.button','/providers'],'scan_or_process':['setup.steps.scanOrProcess.lead','setup.steps.scanOrProcess.button','/library-roots'],'search':['setup.steps.search.lead','setup.steps.search.button','/'],'ready':['setup.steps.ready.lead','setup.steps.ready.button','/']};
    const step=steps[s.next_step];const sb=document.getElementById('status-body');
    if(sb){sb.innerHTML=step?'<b>'+esc(tdT(step[0]))+'</b> <a class="button" href="'+step[2]+'">'+esc(tdT(step[1]))+'</a>':'<span class="muted">'+esc(tdT('setup.steps.unknown'))+'</span>'}
    setupPill('pill-status',!!s.ready);
    if(box){box.className='status ok';box.textContent=tdT('setup.status.updated')}
  }).catch(function(){
    if(box){box.className='status bad';box.textContent=tdT('setup.status.fetchFailed')}
  })
}
setupLoadStatus();
</script>
</body></html>`
