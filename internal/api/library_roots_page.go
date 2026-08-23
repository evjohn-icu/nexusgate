package api

import "net/http"

// libraryRootsPage is the wizard that closes the gap /setup used to promise
// and never delivered: adding a library root that lives on a network share.
// Like every other page here it is one inline HTML/CSS/JS constant with no
// build step and no admin token in browser storage — see adminToken() below,
// copied from the same idiom worker_setup_page.go uses.
//
// It is a standalone constant, not a strings.Replace overlay on an existing
// page: nothing else in this file patches library_page_v015.go-style, and
// nothing patches this file either, so there is nothing here for a future
// edit to silently break.
func (s *Server) libraryRootsPage(w http.ResponseWriter, r *http.Request) {
	s.serveLocalizedPage(w, r, "/library-roots", libraryRootsHTML)
}

const libraryRootsHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · [[i18n:roots.title]]</title><style>

.step{display:none}
.step.active{display:block}
.panel{padding:20px;margin-bottom:16px}
.panel h2{margin:0 0 14px}
.field{margin:12px 0}
.health-path{font-family:var(--font-data);font-size:13px;word-break:break-all}
.discover-host{margin-bottom:10px;padding:12px 14px}
.discover-host-name{font-weight:800;font-size:13px;margin-bottom:8px}
.discover-share{display:inline-block;margin:3px 6px 3px 0;padding:4px 10px;background:var(--inset);border:1px solid var(--rule-strong);border-radius:6px;color:var(--text);font-family:var(--font-data);font-size:12px;cursor:pointer}
.discover-share:hover{border-color:var(--accent,currentColor)}
.share-line{font-family:var(--font-data);font-size:13px;color:var(--text-muted);word-break:break-all;background:var(--inset);border-radius:8px;padding:8px;margin:6px 0}
.guide-step{margin:16px 0}
.guide-step-title{font-weight:800;font-size:13px;color:var(--text);margin-bottom:7px}
.cmd{display:flex;gap:8px;align-items:flex-start;background:var(--inset);border:1px solid var(--rule-strong);border-radius:8px;padding:9px 10px;margin:6px 0}
.cmd code{flex:1;font-family:var(--font-data);font-size:12px;line-height:1.55;color:var(--text-muted);word-break:break-all;white-space:pre-wrap}
.notes{margin-top:14px;padding:12px 14px;background:var(--inset);border-radius:8px;color:var(--text-muted);font-size:12px;line-height:1.6}
.notes ul{margin:6px 0 0;padding-left:18px}
.center{text-align:center;margin:18px 0}
.step-actions{display:flex;gap:10px;margin-top:16px}
@media(max-width:600px){.cmd{flex-direction:column}
}

</style></head><body data-library-roots-wizard>
<!--SHELL_HEADER-->
<div class="wrap">
<div class="panel" id="root-health-section">
<h2>[[i18n:roots.healthTitle]]</h2>
<p class="muted">[[i18n:roots.healthIntro]]</p>
<div id="root-health-wrap"><div class="muted">[[i18n:roots.loadingHealth]]</div></div>
</div>

<div class="step-nav" id="step-nav"><div class="active" data-step="1">1. [[i18n:roots.step1]]</div><div data-step="2">2. [[i18n:roots.step2]]</div><div data-step="3">3. [[i18n:roots.step3]]</div><div data-step="4">4. [[i18n:roots.step4]]</div></div>

<div class="panel" id="discover-section">
<h2>[[i18n:roots.discoverTitle]]</h2>
<p class="muted">[[i18n:roots.discoverIntro]]</p>
<div class="step-actions">
  <button class="primary" id="discover-btn" onclick="runDiscover()">[[i18n:roots.discoverButton]]</button>
  <span class="hint" id="discover-status"></span>
</div>
<div id="discover-results"></div>
</div>

<div class="step active" id="step-1">
<div class="panel"><h2>[[i18n:roots.step1Title]]</h2>
<p class="muted">[[i18n:roots.step1IntroBefore]]<code>//nas/Video</code>[[i18n:roots.step1Sep]]<code>smb://user@host/share</code>[[i18n:roots.step1Sep]]<code>host:/export</code>[[i18n:roots.step1IntroAfter]]</p>
<div class="field"><label for="root-input">[[i18n:roots.pathOrShareLabel]]</label><input id="root-input" type="text" placeholder="[[i18n:roots.pathPlaceholder]]"></div>
<div id="step1-status" class="hint"></div>
<div class="step-actions"><button class="primary" onclick="startInspect()">[[i18n:common.next]] →</button></div>
</div>
</div>

<div class="step" id="step-2">
<div class="panel"><h2>[[i18n:roots.networkShareTitle]]</h2>
<div id="share-summary"></div>
<p class="muted" id="guide-summary"></p>
<div class="field"><label for="mountpoint">[[i18n:roots.mountpointLabel]]</label><input id="mountpoint" type="text" onchange="regenerateGuidance()"></div>
<div id="guide-body"></div>
</div>
<div class="panel" id="compose-section" style="display:none"></div>
<div class="step-actions"><button class="nav-btn secondary" onclick="goStep(1)">← [[i18n:roots.prevStep]]</button><button class="nav-btn primary" onclick="goVerify()">[[i18n:roots.verifyDone]]</button></div>
</div>

<div class="step" id="step-3">
<div class="panel"><h2>[[i18n:roots.verifyTitle]]</h2>
<p class="muted">[[i18n:roots.verifyIntro]]</p>
<div id="verify-result"></div>
<div class="step-actions"><button class="nav-btn secondary" onclick="goStep(2)">← [[i18n:roots.backToGuidance]]</button><button class="nav-btn secondary" onclick="verifyMount()">[[i18n:roots.recheck]]</button><button class="nav-btn primary" id="verify-add-btn" style="display:none" onclick="addFromVerify()">[[i18n:roots.addAsRoot]]</button></div>
</div>
</div>

<div class="step" id="step-4">
<div class="panel"><h2>[[i18n:roots.addedTitle]]</h2>
<div id="added-summary"></div>
<div class="center"><button class="primary" id="scan-btn" style="display:none" onclick="startScan()">[[i18n:roots.startScan]]</button></div>
<div id="scan-result"></div>
<p class="muted">[[i18n:roots.scanQueueNote]]<a href="/progress">[[i18n:progress.title]]</a>[[i18n:roots.scanQueueNoteAfter]]</p>
</div>
</div>

</div>
<script>
const esc=function(v){return String(v??'').replace(/[&<>"']/g,function(c){return {'&':'&amp;','>':'&gt;','<':'&lt;','"':'&quot;',"'":'&#39;'}[c]})};

// internal/mount generates step titles and notes in English on purpose --
// it is also what the English CLI (timingdex doctor) renders -- and gives
// each one a stable Key (mount.Step.Key, mount.Note.Key) precisely so a
// consumer that needs another language can translate by Key instead of by
// matching English text. The catalog keys roots.guide.<key> are that
// translation, kept in the shared locale catalogs (this page's fragment).
// guideText() below falls back to the English text (step.title / note.text /
// volume.warning) rather than rendering nothing when a key is missing,
// because a wizard with one English line is still usable and a blank step is
// not. TestLibraryRootsPageTranslatesEveryGuidanceKey in
// library_roots_page_test.go asserts every Key mount.Guidance can currently
// produce has a roots.guide.* entry in the fragment, so a Key added to mount
// without a matching entry fails the build instead of shipping silently in
// English. The key prefix lives in a variable so tdT is never called with a
// literal partial key (the mount keys themselves are trusted constants from
// internal/mount, never user input, so concatenation is safe).
var guideKeyPrefix='roots.guide.';
function guideText(key,fallback){var msg=tdT(guideKeyPrefix+key);return msg===guideKeyPrefix+key?fallback:msg}

 function csrfToken(){var prefix='__Host-timingdex_csrf=';var item=document.cookie.split('; ').find(function(x){return x.indexOf(prefix)===0});return item?decodeURIComponent(item.slice(prefix.length)):''}
 function authHeaders(base){var headers=new Headers(base||{});var csrf=csrfToken();if(csrf)headers.set('X-CSRF-Token',csrf);return headers}
async function json(url,opt){opt=opt||{};var r=await fetch(url,{...opt,headers:authHeaders(opt.headers)});if(!r.ok)throw new Error(await tdApiErrorMessage(r));return r.json()}
function goStep(n){for(var i=1;i<=4;i++){document.getElementById('step-'+i).className='step'+(i===n?' active':'');document.querySelectorAll('.step-nav div')[i-1].className=(i===n?'active':'')}}

// healthTime renders a timestamp in the selected UI locale. It returns plain
// text only -- every caller passes the result through esc() before putting it
// in innerHTML, so the raw fallback is not escaped here.
function healthTime(v){if(!v)return'—';var t=new Date(v);return isNaN(t.getTime())?String(v):tdFormatDateTime(t)}
async function loadRootHealth(){
  var wrap=document.getElementById('root-health-wrap');
  if(!wrap)return;
  var list;
  try{list=await json('/api/v1/roots/health')}catch(e){wrap.innerHTML='<div class="muted">'+esc(tdT('roots.healthAuthRequired'))+'</div>';return}
  if(!list||!list.length){wrap.innerHTML='<div class="muted">'+esc(tdT('roots.noRootsYet'))+'</div>';return}
  var warns=[];
  var rows=list.map(function(h){
    var pill='muted',stateKey='roots.state.unknown';
    if(h.state==='healthy'){pill='ok';stateKey='status.healthy'}
    else if(h.state==='unavailable'){pill='err';stateKey='status.unavailable'}
    if(h.state==='unavailable'){
      // The reconciliation gate pauses on an unavailable root (the scan
      // service's verdict, not this page's) so assets are never marked
      // missing while the root itself is the thing that is gone.
      warns.push('<div class="health-warn">⚠ '+esc(tdT('roots.unavailablePause'))+'<br>'+esc(tdT('roots.lastHealthy',{time:healthTime(h.last_healthy_at)}))+'</div>');
    }
    // warning_details is the structured form of the same advice Doctor
    // prints (network mount, staging-copy recommendation, writable mount) --
    // see Service.RootWarningDetails. Each code maps to a roots.warning.*
    // catalog key; an unknown future code falls back to the English message
    // rather than rendering the raw key. warnings stays as the fallback for
    // payloads that predate warning_details.
    var tips=rootWarningsHTML(h);
    return '<tr><td class="health-path">'+esc(h.path)+'</td><td><span class="health-pill '+pill+'">'+esc(tdT(stateKey))+'</span></td><td>'+esc(healthTime(h.last_healthy_at))+'</td><td>'+esc(healthTime(h.last_scan_at))+'</td><td class="health-tips">'+tips+'</td></tr>';
  }).join('');
  wrap.innerHTML='<table class="health-table"><tr><th>'+esc(tdT('roots.colPath'))+'</th><th>'+esc(tdT('roots.colState'))+'</th><th>'+esc(tdT('roots.colLastHealthy'))+'</th><th>'+esc(tdT('roots.colLastScan'))+'</th><th>'+esc(tdT('roots.colTips'))+'</th></tr>'+rows+'</table>'+warns.join('');
}
var rootWarningKeyPrefix='roots.warning.';
function rootWarningsHTML(h){
  var details=(h.warning_details||[]);
  if(details.length){
    return details.map(function(d){
      var msg=tdT(rootWarningKeyPrefix+d.code,d.params||{});
      if(msg===rootWarningKeyPrefix+d.code)msg=d.message;
      return '<div class="root-warning">⚠ '+esc(msg)+'</div>';
    }).join('');
  }
  return (h.warnings||[]).map(function(w){return '<div class="root-warning">⚠ '+esc(w)+'</div>'}).join('');
}
loadRootHealth();

var lastInput='';
var lastVerifyPath='';
var addedRoot=null;

// runDiscover calls the Hub-admin-gated /api/v1/roots/discover endpoint (the
// admin session is established through the shell's token input) and renders
// the discovered SMB hosts. Clicking a share fills the path input and moves
// into the existing mount wizard. Anonymous-only: hosts that need credentials
// are shown as such and left to the mount flow.
async function runDiscover(){
  var btn=document.getElementById('discover-btn');
  var status=document.getElementById('discover-status');
  var wrap=document.getElementById('discover-results');
  btn.disabled=true;status.textContent=tdT('roots.scanning');
  wrap.innerHTML='';
  try{
    var data=await json('/api/v1/roots/discover',{method:'POST',headers:authHeaders({'Content-Type':'application/json'})});
    var hosts=(data&&data.hosts)||[];
    if(!hosts.length){
      wrap.innerHTML='<div class="hint">'+esc(tdT('roots.noHostsFound'))+'</div>';
      status.textContent='';return;
    }
    var rows=hosts.map(function(h){
      var name=esc(h.name||h.ip);
      var shares=(h.shares||[]);
      var detail='';
      if(shares.length){
        detail='<div class="share-line">'+shares.map(function(s){
          return '<button type="button" class="discover-share" onclick="useShare(\''+escAttr(h.ip)+'\',\''+escAttr(s)+'\')">'+esc(s)+'</button>';
        }).join('')+'</div>';
      }else if(h.needs_auth){
        detail='<div class="hint">'+esc(tdT('roots.shareNeedsAuth'))+'</div>';
      }else{
        detail='<div class="hint">'+esc(tdT('roots.noReadableShares'))+'</div>';
      }
      return '<div class="panel discover-host"><div class="discover-host-name">'+name+' <span class="muted">'+esc(h.ip)+'</span></div>'+detail+'</div>';
    }).join('');
    wrap.innerHTML=rows;
    status.textContent=tdPlural('roots.hostsFound',hosts.length);
  }catch(e){
    status.className='hint bad';status.textContent=tdT('roots.scanFailed',{message:e.message});
  }finally{
    btn.disabled=false;
  }
}

function escAttr(v){return String(v??'').replace(/[&<>"']/g,function(c){return {'&':'&amp;','>':'&gt;','<':'&lt;','"':'&quot;',"'":'&#39;'}[c]})}

// useShare fills the path input with the SMB share address and starts the
// mount inspection (the same flow as typing //host/share manually).
function useShare(host,share){
  document.getElementById('root-input').value='//'+host+'/'+share;
  startInspect();
}

async function addRoot(path){
  var response=await fetch('/api/v1/roots',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({path:path})});
  var body=null;
  try{body=await response.json()}catch(e){}
  if(response.status===422&&body&&body.inspection){
    var shareErr=new Error((body.error&&body.error.message)||(typeof body.error==='string'?body.error:'')||tdT('roots.shareNotMounted'));
    shareErr.shareNotMounted=true;
    shareErr.inspection=body.inspection;
    throw shareErr;
  }
  if(!response.ok){throw new Error((body&&body.error&&body.error.message)||(body&&typeof body.error==='string'?body.error:'')||response.statusText||tdT('roots.addRootFailed',{status:response.status}))}
  return body;
}

async function startInspect(){
  var input=document.getElementById('root-input').value.trim();
  var status=document.getElementById('step1-status');
  if(!input){status.className='hint bad';status.textContent=tdT('roots.pathRequired');return}
  status.className='hint';status.textContent=tdT('roots.checking');
  lastInput=input;
  try{
    var inspection=await json('/api/v1/roots/inspect',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({path:input})});
    if(inspection.is_share){
      renderGuidance(inspection);
      status.textContent='';
      goStep(2);
      return;
    }
    var root=await addRoot(input);
    addedRoot=root;
    renderAdded(root);
    status.textContent='';
    goStep(4);
  }catch(e){
    if(e.shareNotMounted){
      renderGuidance(e.inspection);
      status.textContent='';
      goStep(2);
      return;
    }
    status.className='hint bad';status.textContent=tdT('roots.checkFailed',{message:e.message});
  }
}

function renderGuidance(inspection){
  var share=inspection.share||{};
  document.getElementById('share-summary').innerHTML='<div class="share-line">'+esc((share.protocol||'').toUpperCase())+' · '+esc(share.host||'')+' / '+esc(share.name||'')+(share.user?' · '+esc(tdT('roots.shareUser',{user:share.user})):'')+'</div>';
  // Guidance.Summary is always the same English template ("%s is a network
  // share, not a local path...") with only the share address substituted --
  // see mount.Guidance -- so there is exactly one sentence to translate, and
  // it is derived from inspection.path (already the parsed share address,
  // see RootInspection.Path in internal/app/service.go) rather than given a
  // Key of its own the way steps and notes are.
  document.getElementById('guide-summary').textContent=tdT('roots.summary.networkShare',{path:(inspection&&inspection.path)||''});
  document.getElementById('mountpoint').value=inspection.default_mountpoint||'';
  renderGuideBody(inspection.guidance);
  renderComposeVolume(inspection);
}

// compose_volume is only present when internal/app.InspectRootPath finds the
// Hub itself running containerised (mount.Host.Container) -- on a bare-metal
// host the mount commands above are already the right answer and this
// section would be noise, so it stays hidden by default (see
// style="display:none" on #compose-section in the markup).
function renderComposeVolume(inspection){
  var container=document.getElementById('compose-section');
  var volume=inspection&&inspection.compose_volume;
  if(!volume){container.style.display='none';container.innerHTML='';return}
  container.style.display='';
  var protocol=((inspection.share||{}).protocol||'').toLowerCase();
  var html='<h2>'+esc(tdT('roots.composeTitle'))+'</h2><p class="muted">'+esc(tdT('roots.composeIntro'))+'</p>';
  if(protocol==='smb'){
    html+='<p class="muted">'+esc(tdT('roots.composeNfsRecommended'))+'</p>';
  }else if(protocol==='nfs'){
    html+='<p class="muted">'+esc(tdT('roots.composeNfsPreferred'))+'</p>';
  }
  if(volume.warning){
    html+='<div class="callout callout--contradicted"><b>'+esc(tdT('roots.warningLabel'))+'</b>'+esc(guideText(volume.warning_key,volume.warning))+'</div>';
  }
  html+='<div class="guide-step"><div class="guide-step-title">'+esc(tdT('roots.composeStep1'))+'</div><div class="cmd"><code id="compose-yaml-code">'+esc(volume.yaml)+'</code><button type="button" class="copy-btn" onclick="copyCmd(\'compose-yaml-code\')">'+esc(tdT('common.copy'))+'</button></div></div>';
  if(volume.service_yaml){
    html+='<div class="guide-step"><div class="guide-step-title">'+esc(tdT('roots.composeStep2'))+'</div><div class="cmd"><code id="compose-service-code">'+esc(volume.service_yaml)+'</code><button type="button" class="copy-btn" onclick="copyCmd(\'compose-service-code\')">'+esc(tdT('common.copy'))+'</button></div></div>';
  }
  if(volume.mount_path){
    html+='<div class="guide-step"><div class="guide-step-title">'+esc(tdT('roots.composeStep3'))+'</div><div class="cmd"><code id="compose-root-code">timingdex root add '+esc(volume.mount_path)+'</code><button type="button" class="copy-btn" onclick="copyCmd(\'compose-root-code\')">'+esc(tdT('common.copy'))+'</button></div>';
    html+='<p class="muted">'+esc(tdT('roots.composeVerifyNote',{path:volume.mount_path}))+'</p></div>';
  }
  container.innerHTML=html;
}

function renderGuideBody(guidance){
  var container=document.getElementById('guide-body');
  if(!guidance||!guidance.steps){container.innerHTML='<div class="muted">'+esc(tdT('roots.noGuidance'))+'</div>';return}
  var html='';
  guidance.steps.forEach(function(step,i){
    html+='<div class="guide-step"><div class="guide-step-title">'+(i+1)+'. '+esc(guideText(step.key,step.title))+'</div>';
    (step.commands||[]).forEach(function(cmd,j){
      var id='cmd-'+i+'-'+j;
      html+='<div class="cmd"><code id="'+id+'">'+esc(cmd)+'</code><button type="button" class="copy-btn" onclick="copyCmd(\''+id+'\')">'+esc(tdT('common.copy'))+'</button></div>';
    });
    html+='</div>';
  });
  if(guidance.notes&&guidance.notes.length){
    html+='<div class="notes"><b>'+esc(tdT('roots.notesLabel'))+'</b><ul>'+guidance.notes.map(function(n){return '<li>'+esc(guideText(n.key,n.text))+'</li>'}).join('')+'</ul></div>';
  }
  container.innerHTML=html;
}

async function regenerateGuidance(){
  var mp=document.getElementById('mountpoint').value.trim();
  if(!lastInput)return;
  var container=document.getElementById('guide-body');
  try{
    var inspection=await json('/api/v1/roots/inspect',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({path:lastInput,mountpoint:mp})});
    renderGuideBody(inspection.guidance);
    renderComposeVolume(inspection);
  }catch(e){
    container.innerHTML='<div class="hint bad">'+esc(tdT('roots.regenerateFailed',{message:e.message}))+'</div>';
  }
}

function goVerify(){goStep(3);verifyMount()}

async function verifyMount(){
  var mp=document.getElementById('mountpoint').value.trim();
  var el=document.getElementById('verify-result');
  document.getElementById('verify-add-btn').style.display='none';
  if(!mp){el.innerHTML='<div class="hint bad">'+esc(tdT('roots.mountpointEmpty'))+'</div>';return}
  el.innerHTML='<div class="muted">'+esc(tdT('roots.checking'))+'</div>';
  try{
    var inspection=await json('/api/v1/roots/inspect',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({path:mp})});
    lastVerifyPath=inspection.container_path||mp;
    var lines=[];
    // A containerised Hub cannot stat the host-side path the operator was
    // told to mount, so the server checked the translated one. Name both,
    // rather than reporting a verdict about a path the operator never typed.
    var checked=inspection.container_path||mp;
    if(inspection.container_path){lines.push('<div class="muted">'+esc(tdT('roots.containerPathNote',{hostPath:mp,containerPath:inspection.container_path}))+'</div>')}
    if(inspection.exists&&inspection.is_dir){
      lines.push('<div class="hint ok">'+esc(tdT('roots.existsIsDir',{path:checked}))+'</div>');
      if(inspection.network){lines.push('<div class="muted">'+esc(tdT('roots.filesystem',{label:inspection.network_label||inspection.filesystem_type,type:inspection.filesystem_type}))+'</div>')}
      document.getElementById('verify-add-btn').style.display='';
    }else if(!inspection.exists){
      lines.push('<div class="hint bad">'+esc(tdT('roots.notExists',{path:checked}))+'</div>');
    }else{
      lines.push('<div class="hint bad">'+esc(tdT('roots.existsNotDir',{path:checked}))+'</div>');
    }
    if(inspection.looks_unmounted){lines.push('<div class="hint bad">'+esc(tdT('roots.looksUnmounted'))+'</div>')}
    (inspection.warnings||[]).forEach(function(w){lines.push('<div class="hint">'+esc(w)+'</div>')});
    el.innerHTML=lines.join('');
  }catch(e){
    el.innerHTML='<div class="hint bad">'+esc(tdT('roots.checkFailed',{message:e.message}))+'</div>';
  }
}

async function addFromVerify(){
  var mp=lastVerifyPath||document.getElementById('mountpoint').value.trim();
  var el=document.getElementById('verify-result');
  try{
    var root=await addRoot(mp);
    addedRoot=root;
    renderAdded(root);
    goStep(4);
  }catch(e){
    if(e.shareNotMounted){
      el.innerHTML+='<div class="hint bad">'+esc(tdT('roots.addFailedShareNotMounted',{message:e.message}))+'</div>';
      renderGuidance(e.inspection);
      goStep(2);
      return;
    }
    el.innerHTML+='<div class="hint bad">'+esc(tdT('roots.addFailed',{message:e.message}))+'</div>';
  }
}

function renderAdded(root){
  document.getElementById('added-summary').innerHTML='<div class="hint ok">'+esc(tdT('roots.added',{path:root.path}))+'</div><div class="muted">'+esc(tdT('roots.id',{id:root.id}))+'</div>';
  document.getElementById('scan-result').innerHTML='';
  document.getElementById('scan-btn').style.display='';
}

async function startScan(){
  if(!addedRoot)return;
  var btn=document.getElementById('scan-btn');
  btn.disabled=true;btn.textContent=tdT('roots.scanningRoot');
  var el=document.getElementById('scan-result');
  try{
    var result=await json('/api/v1/roots/'+encodeURIComponent(addedRoot.id)+'/scan',{method:'POST'});
    var errors=(result.errors||[]);
    var pipeline=result.pipeline_status==='started'?tdT('roots.scanStarted'):(result.pipeline_status==='already_running'?tdT('roots.scanAlreadyRunning'):tdT('roots.scanCompleted'));
    el.innerHTML='<div class="hint ok">'+esc(tdT('roots.scanSummary',{discovered:result.discovered,linked:result.linked,missing:result.missing,pipeline:pipeline}))+'</div>'+(errors.length?'<div class="hint">'+esc(tdPlural('roots.scanWarnings',errors.length))+errors.map(esc).join('<br>')+'</div>':'')+'<div class="muted" style="margin-top:8px"><a href="/progress">'+esc(tdT('roots.viewProgress'))+' →</a></div>';
  }catch(e){
    el.innerHTML='<div class="hint bad">'+esc(tdT('roots.scanFailed',{message:e.message}))+'</div>';
  }finally{
    btn.disabled=false;btn.textContent=tdT('roots.startScan');
  }
}

function copyCmd(id){
  var el=document.getElementById(id);
  if(!el)return;
  var range=document.createRange();
  range.selectNode(el);
  var selection=window.getSelection();
  selection.removeAllRanges();
  selection.addRange(range);
  try{document.execCommand('copy')}catch(e){}
  selection.removeAllRanges();
}
</script></body></html>`
