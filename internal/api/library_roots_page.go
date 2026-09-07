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

const libraryRootsHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>NexusGate · [[i18n:roots.title]]</title><style>

/* Both status spans are .callout, which paints a bordered box even with no
   text; hide them until something is written. The leading "+" this list once
   carried was a stray combinator, and a selector list is not forgiving — one
   invalid selector kills the whole comma-separated list, so neither span was
   ever hidden. */
#step1-status:empty,#discover-status:empty{display:none}
/* goStep() toggles .active and nothing else styled it, so every step showed
   at once and the numbered nav above described a sequence that was not
   happening. Same shape as .step-panel in worker_setup_page.go. */
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
<div id="root-health-wrap" role="status" aria-live="polite"><div class="muted">[[i18n:roots.loadingHealth]]</div></div>
</div>

<nav class="steps" id="step-nav" aria-label="[[i18n:roots.wizardNavLabel]]"><span class="is-active" data-step="1" aria-current="step">1. [[i18n:roots.step1]]</span><span data-step="2">2. [[i18n:roots.step2]]</span><span data-step="3">3. [[i18n:roots.step3]]</span><span data-step="4">4. [[i18n:roots.step4]]</span></nav>

<!-- The discover panel lives inside step 1 rather than beside it. As a sibling
     it stayed on screen through steps 2-4, where clicking a share silently
     reset the wizard to step 1 and discarded the mount point the operator had
     just filled in; .step{display:none} now retires it with the rest of the
     step. It is also the answer to "where do I start" that the numbered nav
     promised and a floating second button contradicted. -->
<div class="step active" id="step-1" role="group" aria-label="[[i18n:roots.step1]]">
<div class="panel" id="discover-section">
<h2>[[i18n:roots.discoverTitle]]</h2>
<p class="muted">[[i18n:roots.discoverIntro]]</p>
<div class="step-actions">
  <button class="primary" id="discover-btn" onclick="runDiscover()">[[i18n:roots.discoverButton]]</button>
  <span class="callout" id="discover-status" role="status" aria-live="polite"></span>
</div>
<div id="discover-results" role="status" aria-live="polite"></div>
</div>

<div class="panel"><h2>[[i18n:roots.step1Title]]</h2>
<p class="muted">[[i18n:roots.discoverOr]]</p>
<p class="muted">[[i18n:roots.step1IntroBefore]]<code>//nas/Video</code>[[i18n:roots.step1Sep]]<code>smb://user@host/share</code>[[i18n:roots.step1Sep]]<code>host:/export</code>[[i18n:roots.step1IntroAfter]]</p>
<div class="field"><label for="root-input">[[i18n:roots.pathOrShareLabel]]</label><input id="root-input" type="text" placeholder="[[i18n:roots.pathPlaceholder]]"></div>
<div id="step1-status" class="callout" role="status" aria-live="polite"></div>
<div class="step-actions"><button class="primary" onclick="startInspect()">[[i18n:common.next]] →</button></div>
</div>
</div>

<div class="step" id="step-2" role="group" aria-label="[[i18n:roots.step2]]">
<div class="panel"><h2>[[i18n:roots.networkShareTitle]]</h2>
<div id="share-summary"></div>
<p class="muted" id="guide-summary"></p>
<!-- The Hub cannot see either of these about itself. Inside a container its own
     OS is the container's, /etc/unraid-version lives on a host it cannot read,
     and no path is proof: anyone can create /mnt/remotes. Rather than guess —
     a guessed platform sends the operator to mount somewhere the Hub will
     never look — the wizard asks, once. -->
<div class="field"><label for="host-platform">[[i18n:roots.hostWhere]]</label>
<select id="host-platform" onchange="regenerateGuidance()">
<option value="">[[i18n:roots.hostAuto]]</option>
<option value="unraid">[[i18n:roots.hostUnraid]]</option>
</select>
<label class="muted"><input id="host-service" type="checkbox" onchange="regenerateGuidance()"> [[i18n:roots.hostServiceLabel]]</label>
<p class="muted">[[i18n:roots.hostHintNote]]</p>
</div>
<div class="field"><label for="mountpoint">[[i18n:roots.mountpointLabel]]</label><input id="mountpoint" type="text" oninput="updateMountpointHint()" onchange="regenerateGuidance()"><div id="mountpoint-hint" class="callout callout--attention" style="display:none" role="note"></div></div>
<div id="guide-body"></div>
</div>
<div class="panel" id="compose-section" style="display:none"></div>
<div class="step-actions"><button class="btn btn--ghost" onclick="goStep(1)">← [[i18n:roots.prevStep]]</button><button class="btn btn--primary" onclick="goVerify()">[[i18n:roots.verifyDone]]</button></div>
</div>

<div class="step" id="step-3" role="group" aria-label="[[i18n:roots.step3]]">
<div class="panel"><h2>[[i18n:roots.verifyTitle]]</h2>
<p class="muted">[[i18n:roots.verifyIntro]]</p>
<div id="verify-result" role="status" aria-live="polite"></div>
<div class="step-actions"><button class="btn btn--ghost" onclick="goStep(2)">← [[i18n:roots.backToGuidance]]</button><button class="btn btn--ghost" onclick="verifyMount()">[[i18n:roots.recheck]]</button><button class="btn btn--primary" id="verify-add-btn" style="display:none" onclick="addFromVerify()">[[i18n:roots.addAsRoot]]</button></div>
</div>
</div>

<div class="step" id="step-4" role="group" aria-label="[[i18n:roots.step4]]">
<div class="panel"><h2>[[i18n:roots.addedTitle]]</h2>
<div id="added-summary" role="status" aria-live="polite"></div>
<div class="center"><button class="primary" id="scan-btn" style="display:none" onclick="startScan()">[[i18n:roots.startScan]]</button></div>
<div id="scan-result" role="status" aria-live="polite"></div>
<p class="muted">[[i18n:roots.scanQueueNote]]<a href="/progress">[[i18n:progress.title]]</a>[[i18n:roots.scanQueueNoteAfter]]</p>
</div>
</div>

</div>
<script>
const esc=function(v){return String(v??'').replace(/[&<>"']/g,function(c){return {'&':'&amp;','>':'&gt;','<':'&lt;','"':'&quot;',"'":'&#39;'}[c]})};

// internal/mount generates step titles and notes in English on purpose --
// it is also what the English CLI (nexusgate doctor) renders -- and gives
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

 function csrfToken(){var prefix='__Host-nexusgate_csrf=';var item=document.cookie.split('; ').find(function(x){return x.indexOf(prefix)===0});return item?decodeURIComponent(item.slice(prefix.length)):''}
 function authHeaders(base){var headers=new Headers(base||{});var csrf=csrfToken();if(csrf)headers.set('X-CSRF-Token',csrf);return headers}
// The status code rides on the thrown Error: every catch on this page needs
// to separate "you are not logged in" (recoverable here, on this page) from a
// genuine failure, and tdApiErrorMessage returns prose only.
async function json(url,opt){opt=opt||{};var r=await fetch(url,{...opt,headers:authHeaders(opt.headers)});if(!r.ok){var err=new Error(await tdApiErrorMessage(r));err.status=r.status;throw err}return r.json()}
// authNotice renders the one thing a 401 needs: a way to log in without
// leaving the wizard, and a retry that resumes exactly where it stopped.
// The retry is wired to the admin-auth-changed event the shell dispatches, so
// it fires on a successful login and not on a dismissed dialog.
var authRetryPending=null;
function authNotice(retry){authRetryPending=retry||null;return '<div class="callout callout--attention" role="alert"><span>'+esc(tdT('roots.authRequired'))+'</span><button type="button" class="btn btn--primary btn--sm" onclick="rootsOpenLogin(this)">'+esc(tdT('roots.authLogin'))+'</button></div>'}
function rootsOpenLogin(opener){tdAdminLoginRequired(opener)}
window.addEventListener('nexusgate:admin-auth-changed',function(ev){if(!ev.detail||!ev.detail.authenticated)return;var retry=authRetryPending;authRetryPending=null;loadRootHealth();if(typeof retry==='function')retry()});
// aria-current is set on the one active marker and removed from the rest:
// leaving it on every step (or on step 1 forever) tells a screen reader the
// wizard never moved.
function goStep(n){for(var i=1;i<=4;i++){document.getElementById('step-'+i).className='step'+(i===n?' active':'');var marker=document.querySelectorAll('.steps span')[i-1];marker.className=(i===n?'is-active':(i<n?'is-done':''));if(i===n){marker.setAttribute('aria-current','step')}else{marker.removeAttribute('aria-current')}}}

// healthTime renders a timestamp in the selected UI locale. It returns plain
// text only -- every caller passes the result through esc() before putting it
// in innerHTML, so the raw fallback is not escaped here.
function healthTime(v){if(!v)return'—';var t=new Date(v);return isNaN(t.getTime())?String(v):tdFormatDateTime(t)}
async function loadRootHealth(){
  var wrap=document.getElementById('root-health-wrap');
  if(!wrap)return;
  var list;
  try{list=await json('/api/v1/roots/health')}catch(e){wrap.innerHTML=tdAuthDenied(e)?authNotice(loadRootHealth):'<div class="callout callout--contradicted"><span>'+esc(tdT('roots.healthFailed',{message:e.message}))+'</span></div>';return}
  if(!list||!list.length){wrap.innerHTML='<div class="empty"><span class="label">'+esc(tdT('roots.noRootsYetTitle'))+'</span><div class="muted">'+esc(tdT('roots.noRootsYet'))+'</div><div class="muted">'+esc(tdT('roots.noRootsYetAction'))+'</div></div>';return}
  var warns=[];
  var rows=list.map(function(h){
    var state='state--unknown',stateKey='roots.state.unknown';
    if(h.state==='healthy'){state='state--confirmed';stateKey='status.healthy'}
    else if(h.state==='unavailable'){state='state--contradicted';stateKey='status.unavailable'}
    if(h.state==='unavailable'){
      // The reconciliation gate pauses on an unavailable root (the scan
      // service's verdict, not this page's) so assets are never marked
      // missing while the root itself is the thing that is gone.
      warns.push('<div class="callout callout--attention"><span>⚠ '+esc(tdT('roots.unavailablePause'))+'<br>'+esc(tdT('roots.lastHealthy',{time:healthTime(h.last_healthy_at)}))+'</span></div>');
    }
    // warning_details is the structured form of the same advice Doctor
    // prints (network mount, staging-copy recommendation, writable mount) --
    // see Service.RootWarningDetails. Each code maps to a roots.warning.*
    // catalog key; an unknown future code falls back to the English message
    // rather than rendering the raw key. warnings stays as the fallback for
    // payloads that predate warning_details.
    var tips=rootWarningsHTML(h);
    return '<tr><td class="health-path">'+esc(h.path)+'</td><td><span class="state '+state+'">'+esc(tdT(stateKey))+'</span></td><td>'+esc(healthTime(h.last_healthy_at))+'</td><td>'+esc(healthTime(h.last_scan_at))+'</td><td class="health-tips">'+tips+'</td></tr>';
  }).join('');
  wrap.innerHTML='<div class="table-scroll"><table class="table"><tr><th>'+esc(tdT('roots.colPath'))+'</th><th>'+esc(tdT('roots.colState'))+'</th><th>'+esc(tdT('roots.colLastHealthy'))+'</th><th>'+esc(tdT('roots.colLastScan'))+'</th><th>'+esc(tdT('roots.colTips'))+'</th></tr>'+rows+'</table></div>'+warns.join('');
}
var rootWarningKeyPrefix='roots.warning.';
function rootWarningsHTML(source,calloutClass){
  calloutClass=calloutClass||'callout callout--attention';
  var details=(source.warning_details||[]);
  if(details.length){
    return details.map(function(d){
      var msg=tdT(rootWarningKeyPrefix+d.code,d.params||{});
      if(msg===rootWarningKeyPrefix+d.code)msg=d.message;
      return '<div class="'+calloutClass+'"><span>⚠ '+esc(msg)+'</span></div>';
    }).join('');
  }
  return (source.warnings||[]).map(function(w){return '<div class="'+calloutClass+'"><span>⚠ '+esc(w)+'</span></div>'}).join('');
}
loadRootHealth();

var lastInput='';
// verifyMount inspects the MOUNT POINT, which is a local path — is_share on
// that response is therefore always false, and gating the add button on it
// would never fire. What the wizard actually knows is which flow it is in:
// renderGuidance runs only when step 1 classified the operator's input as a
// share. That is the fact worth remembering, so remember it here.
var mountingShare=false;
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
      wrap.innerHTML=discoverDiagnostics(data,hosts)||'<div class="callout">'+esc(tdT('roots.noHostsFound'))+'</div>';
      status.textContent='';return;
    }
    var rows=hosts.map(function(h){
      var name=esc(h.name||h.ip);
      var shares=(h.shares||[]);
      // probe is the discovery layer's own verdict; needs_auth is kept as the
      // fallback for a payload that predates it. "unusable" is not a softer
      // "auth": it means the host answered on 445 but is not a usable SMB2/3
      // target, and telling that operator a password will help blames them for
      // something a password cannot fix.
      var probe=h.probe||(shares.length?'ok':(h.needs_auth?'auth':'unusable'));
      var detail='';
      if(shares.length){
        detail='<div class="share-line">'+shares.map(function(s){
          return '<button type="button" class="discover-share" data-share-host="'+escAttr(h.ip)+'" data-share-name="'+escAttr(s)+'">'+esc(s)+'</button>';
        }).join('')+'</div><div class="muted">'+esc(tdT('roots.probe.ok.hint'))+'</div>';
      }else if(probe==='auth'){
        detail='<div class="callout callout--attention"><b>'+esc(tdT('roots.probe.auth.title'))+'</b><br>'+esc(tdT('roots.probe.auth.hint'))+'</div>';
      }else if(probe==='timeout'){
        detail='<div class="callout"><b>'+esc(tdT('roots.probe.timeout.title'))+'</b><br>'+esc(tdT('roots.probe.timeout.hint'))+'</div>';
      }else{
        detail='<div class="callout"><b>'+esc(tdT('roots.probe.unusable.title'))+'</b><br>'+esc(tdT('roots.probe.unusable.hint'))+'</div>';
      }
      // Every state carries the manual entry, not only the ones that failed.
      // Consumer NAS boxes ship with guest access disabled, so "auth" is the
      // common answer rather than the edge case, and a card with nothing
      // clickable on it is where discovery used to dead-end: the next wizard
      // step needs a share name, which is exactly what enumeration could not
      // get. The operator can read it off their NAS admin page.
      detail+='<div class="field"><label for="manual-'+escAttr(h.ip)+'">'+esc(tdT('roots.manualShareLabel'))+'</label>'
        +'<input id="manual-'+escAttr(h.ip)+'" type="text" class="discover-manual-share" data-share-host="'+escAttr(h.ip)+'" placeholder="'+escAttr(tdT('roots.manualSharePlaceholder'))+'">'
        +'<button type="button" class="btn btn--primary discover-manual-go" data-share-host="'+escAttr(h.ip)+'">'+esc(tdT('roots.manualShareUse'))+'</button>'
        +'<div class="muted" data-manual-error="'+escAttr(h.ip)+'"></div></div>';
      return '<div class="panel discover-host"><div class="discover-host-name">'+name+' <span class="muted">'+esc(h.ip)+'</span></div>'+detail+'</div>';
    }).join('');
    wrap.innerHTML=rows+discoverDiagnostics(data,hosts);
    status.textContent=tdPlural('roots.hostsFound',hosts.length);
  }catch(e){
    if(tdAuthDenied(e)){status.className='callout callout--attention';status.innerHTML=authNotice(runDiscover)}
    else{status.className='callout callout--contradicted';status.textContent=tdT('roots.scanFailed',{message:e.message})}
  }finally{
    btn.disabled=false;
  }
}

// discoverDiagnostics explains the result from what the scan actually did,
// rather than from the fact that the list came back short. "No device exposes
// port 445" is one of at least four different situations, and three of them
// are not the operator's LAN being empty: the Hub is in a bridge-networked
// container and swept Docker's own subnet, there was no scannable network at
// all, or a subnet was too wide to sweep and was skipped. Reporting the first
// sentence for all four sends people looking for a fault that is not there.
function discoverDiagnostics(data,hosts){
  data=data||{};
  var nets=(data.scanned_networks||[]).join(', ');
  var skipped=(data.skipped_networks||[]).join(', ');
  var lines=[];
  var note=function(cls,text){lines.push('<div class="callout '+cls+'">'+esc(text)+'</div>')};
  // One cause per empty result, most specific first, and each condition states
  // only what it can support. A sweep that ran out of budget has not shown that
  // nothing answers on 445, and a subnet skipped for being too wide is not the
  // same as having no network to sweep — saying either of those alongside
  // "nothing was found" sends the operator after a fault that is not there.
  if(!hosts.length){
    if(data.hub_containerised){
      note('callout--attention',tdT('roots.diag.containerBridge',{nets:nets}));
    }else if(!nets&&skipped){
      note('callout--attention',tdT('roots.diag.skipped',{nets:skipped}));
      skipped='';
    }else if(!nets){
      note('callout--attention',tdT('roots.diag.noNetworks'));
    }else if(!data.truncated){
      note('',tdT('roots.diag.noHosts',{nets:nets}));
    }
  }
  if(skipped){note('callout--attention',tdT('roots.diag.skipped',{nets:skipped}))}
  if(data.truncated){note('callout--attention',tdT('roots.diag.truncated'))}
  // mdns_available is false only when the multicast browse could not start at
  // all, never merely because nobody answered — so it is a fact about this
  // machine's network, and it is the reason a NAS that does advertise itself
  // went unseen. It stands on its own rather than being folded into the
  // no-subnet message, which would claim it even where multicast works fine.
  if(!hosts.length&&data.mdns_available===false){
    note('',tdT('roots.diag.noMulticast'));
  }
  // Only an explicit verdict counts. A host with no probe field at all is an
  // older payload, not an unusable host, and reporting "none of these can be
  // read" over a list the operator can see is worse than saying nothing. A
  // host needing credentials is likewise a usable SMB server, so it must not
  // be folded in here either — its own card already asks for the share name.
  var verdicts=hosts.map(function(h){return h.probe||(h.shares&&h.shares.length?'ok':'')}).filter(Boolean);
  if(hosts.length&&verdicts.length===hosts.length&&verdicts.every(function(v){return v==='unusable'||v==='timeout'})){
    note('callout--attention',tdT('roots.diag.allUnusable',{count:hosts.length}));
  }
  return lines.join('');
}

// hostHint collects the two deployment facts the Hub cannot observe about
// itself and the operator can. The server validates platform against a closed
// set and can only ever turn service on, so nothing here overrides something
// the process proved locally.
function hostHint(){
  var platform=document.getElementById('host-platform');
  var service=document.getElementById('host-service');
  return {platform:platform?platform.value:'',service:service?!!service.checked:false};
}

function escAttr(v){return String(v??'').replace(/[&<>"']/g,function(c){return {'&':'&amp;','>':'&gt;','<':'&lt;','"':'&quot;',"'":'&#39;'}[c]})}

// Share names come from whatever SMB server answered on the LAN, so they reach
// the DOM as data-* attributes read back through this one delegated listener
// and never as inline handler source. An onclick assembled by concatenation is
// parsed twice: the HTML parser decodes character references in the attribute
// value before the JS parser sees it, so escaping a quote as &#39; there
// protects nothing — it arrives at the JS parser as a quote and ends the
// string literal. escAttr is an HTML-attribute encoder and is only sound in
// that position; there is no JS-string encoder on this page because no
// external data belongs in JS source.
document.getElementById('discover-results').addEventListener('click',function(event){
  var share=event.target.closest('.discover-share');
  if(share){useShare(share.dataset.shareHost,share.dataset.shareName);return}
  var manual=event.target.closest('.discover-manual-go');
  if(manual){useManualShare(manual.dataset.shareHost)}
});

// useManualShare is the path most operators actually take, for the reason the
// card rendering explains: the anonymous probe usually returns a host with no
// share list, and the name has to come from the person reading their NAS.
function useManualShare(host){
  var input=document.querySelector('.discover-manual-share[data-share-host="'+host+'"]');
  var error=document.querySelector('[data-manual-error="'+host+'"]');
  var value=input?input.value.trim():'';
  if(!value){
    if(error){error.className='callout callout--contradicted';error.textContent=tdT('roots.shareNameRequired')}
    return;
  }
  if(error){error.className='muted';error.textContent=''}
  useShare(host,value);
}

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
  if(!response.ok){var addErr=new Error((body&&body.error&&body.error.message)||(body&&typeof body.error==='string'?body.error:'')||response.statusText||tdT('roots.addRootFailed',{status:response.status}));addErr.status=response.status;throw addErr}
  return body;
}

async function startInspect(){
  var input=document.getElementById('root-input').value.trim();
  var status=document.getElementById('step1-status');
  if(!input){status.className='callout callout--contradicted';status.textContent=tdT('roots.pathRequired');return}
  status.className='callout';status.textContent=tdT('roots.checking');
  lastInput=input;
  try{
    var inspection=await json('/api/v1/roots/inspect',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify(Object.assign({path:input},hostHint()))});
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
    if(tdAuthDenied(e)){status.className='callout callout--attention';status.innerHTML=authNotice(startInspect);return}
    status.className='callout callout--contradicted';status.textContent=tdT('roots.checkFailed',{message:e.message});
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
  mountingShare=true;
  document.getElementById('guide-summary').textContent=tdT('roots.summary.networkShare',{path:(inspection&&inspection.path)||''});
  document.getElementById('mountpoint').value=inspection.default_mountpoint||'';
  updateMountpointHint();
  renderGuideBody(inspection.guidance, guideActionKey(inspection));
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
  html+='<div class="guide-step"><div class="guide-step-title">'+esc(tdT('roots.composeStep1'))+'</div><div class="cmd"><code id="compose-yaml-code">'+esc(volume.yaml)+'</code><button type="button" class="btn" onclick="copyCmd(\'compose-yaml-code\',this)">'+esc(tdT('common.copy'))+'</button></div></div>';
  if(volume.service_yaml){
    html+='<div class="guide-step"><div class="guide-step-title">'+esc(tdT('roots.composeStep2'))+'</div><div class="cmd"><code id="compose-service-code">'+esc(volume.service_yaml)+'</code><button type="button" class="btn" onclick="copyCmd(\'compose-service-code\',this)">'+esc(tdT('common.copy'))+'</button></div></div>';
  }
  if(volume.mount_path){
    html+='<div class="guide-step"><div class="guide-step-title">'+esc(tdT('roots.composeStep3'))+'</div><div class="cmd"><code id="compose-root-code">nexusgate root add '+esc(volume.mount_path)+'</code><button type="button" class="btn" onclick="copyCmd(\'compose-root-code\',this)">'+esc(tdT('common.copy'))+'</button></div>';
    html+='<p class="muted">'+esc(tdT('roots.composeVerifyNote',{path:volume.mount_path}))+'</p></div>';
  }
  container.innerHTML=html;
}

function updateMountpointHint(){
  var hint=document.getElementById('mountpoint-hint');
  var input=document.getElementById('mountpoint');
  var platform=document.getElementById('host-platform');
  if(!hint||!input||!platform)return;
  var show=platform.value==='unraid'&&!input.value.trim();
  hint.style.display=show?'':'none';
  if(show)hint.textContent=tdT('roots.mountpointManualHint');
}

function guideActionKey(inspection){
  var details=(inspection&&inspection.warning_details)||[];
  for(var i=0;i<details.length;i++){
    if(details[i]&&details[i].code==='root.share_name_unsupported')return 'roots.shareNameUnsupportedAction';
  }
  // Keep unknown warning codes on the mount-point advice rather than a generic
  // string: the wizard does not know which of the two inputs was refused, and
  // the mount point is the one the operator can actually edit here.
  return 'roots.noGuidanceAction';
}

function renderGuideBody(guidance,actionKey){
  var container=document.getElementById('guide-body');
  if(!guidance||!guidance.steps||!guidance.steps.length){container.innerHTML='<div class="muted">'+esc(tdT('roots.noGuidance'))+'</div><div class="callout callout--attention">'+esc(tdT(actionKey||'roots.noGuidanceAction'))+'</div>';return}
  var html='';
  guidance.steps.forEach(function(step,i){
    html+='<div class="guide-step"><div class="guide-step-title">'+(i+1)+'. '+esc(guideText(step.key,step.title))+'</div>';
    // A "file-line" step's commands are a line to append to step.file, not a
    // line to run. Rendered identically to every other block — code plus a
    // copy button — an /etc/fstab entry reads as a command, and pasting it
    // into a terminal is what an operator following the wizard actually does.
    if(step.kind==='file-line'){
      html+='<div class="callout callout--attention">'+esc(tdT('roots.fileLineNote',{file:step.file||''}))+'</div>';
    }
    (step.commands||[]).forEach(function(cmd,j){
      var id='cmd-'+i+'-'+j;
      html+='<div class="cmd"><code id="'+id+'">'+esc(cmd)+'</code><button type="button" class="btn" onclick="copyCmd(\''+id+'\',this)">'+esc(tdT('common.copy'))+'</button></div>';
    });
    html+='</div>';
  });
  if(guidance.notes&&guidance.notes.length){
    html+='<div class="notes"><b>'+esc(tdT('roots.notesLabel'))+'</b><ul>'+guidance.notes.map(function(n){return '<li>'+esc(guideText(n.key,n.text))+'</li>'}).join('')+'</ul></div>';
  }
  // Only where there is something to hand over. Unraid's guidance is a
  // sequence of clicks in its own web UI and carries no commands at all;
  // offering to pass them to someone with a terminal would invent a step.
  if(guidance.steps.some(function(s){return s.commands&&s.commands.length})){
    html+='<div class="callout callout--attention">'+esc(tdT('roots.noTerminalHandoff'))+'</div>';
  }
  container.innerHTML=html;
}

async function regenerateGuidance(){
  var mp=document.getElementById('mountpoint').value.trim();
  if(!lastInput)return;
  var container=document.getElementById('guide-body');
  try{
    var inspection=await json('/api/v1/roots/inspect',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify(Object.assign({path:lastInput,mountpoint:mp},hostHint()))});
    renderGuideBody(inspection.guidance, guideActionKey(inspection));
    updateMountpointHint();
    renderComposeVolume(inspection);
  }catch(e){
    container.innerHTML='<div class="callout callout--contradicted">'+esc(tdT('roots.regenerateFailed',{message:e.message}))+'</div>';
  }
}

function goVerify(){goStep(3);verifyMount()}

async function verifyMount(){
  var mp=document.getElementById('mountpoint').value.trim();
  var el=document.getElementById('verify-result');
  document.getElementById('verify-add-btn').style.display='none';
  if(!mp){el.innerHTML='<div class="callout callout--contradicted">'+esc(tdT('roots.mountpointEmpty'))+'</div>';return}
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
      lines.push('<div class="callout callout--confirmed">'+esc(tdT('roots.existsIsDir',{path:checked}))+'</div>');
      if(inspection.network){lines.push('<div class="muted">'+esc(tdT('roots.filesystem',{label:inspection.network_label||inspection.filesystem_type,type:inspection.filesystem_type}))+'</div>')}
      // An empty LOCAL directory is still addable — root.empty_unmounted says
      // the share "may not" be mounted and is deliberately warn-but-allow. What
      // must be blocked is a mount the operator was just walked through that did
      // not take: registering it succeeds and then reports every asset missing.
      if(mountingShare&&inspection.looks_unmounted){
        lines.push('<div class="callout callout--contradicted">'+esc(tdT('roots.mountFailedNext'))+'</div>');
      }else{
        document.getElementById('verify-add-btn').style.display='';
      }
    }else if(!inspection.exists){
      lines.push('<div class="callout callout--contradicted">'+esc(tdT('roots.notExists',{path:checked}))+'</div>');
    }else{
      lines.push('<div class="callout callout--contradicted">'+esc(tdT('roots.existsNotDir',{path:checked}))+'</div>');
    }
    if(inspection.looks_unmounted){lines.push('<div class="callout callout--contradicted">'+esc(tdT('roots.looksUnmounted'))+'</div>')}
    var warningHTML=rootWarningsHTML(inspection,'callout');
    if(warningHTML)lines.push(warningHTML);
    el.innerHTML=lines.join('');
  }catch(e){
    el.innerHTML=tdAuthDenied(e)?authNotice(null):'<div class="callout callout--contradicted">'+esc(tdT('roots.checkFailed',{message:e.message}))+'</div>';
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
      el.innerHTML+='<div class="callout callout--contradicted">'+esc(tdT('roots.addFailedShareNotMounted',{message:e.message}))+'</div>';
      renderGuidance(e.inspection);
      goStep(2);
      return;
    }
    el.innerHTML+=tdAuthDenied(e)?authNotice(addFromVerify):'<div class="callout callout--contradicted">'+esc(tdT('roots.addFailed',{message:e.message}))+'</div>';
  }
}

function renderAdded(root){
  document.getElementById('added-summary').innerHTML='<div class="callout callout--confirmed">'+esc(tdT('roots.added',{path:root.path}))+'</div><div class="muted">'+esc(tdT('roots.id',{id:root.id}))+'</div>';
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
    var skipped=result.skipped_files||0;
    var noFootage=result.discovered===0&&skipped>0;
    var html='<div class="'+(noFootage?'callout callout--attention':'callout callout--confirmed')+'">'+esc(tdT('roots.scanSummary',{discovered:result.discovered,linked:result.linked,missing:result.missing,pipeline:pipeline}))+'</div>';
    if(skipped>0){var exts=(result.skipped_extensions||[]).join(', ');if(result.skipped_other>0){exts+=' +'+esc(tdT('roots.scanSkippedMore',{count:result.skipped_other}))}html+='<div class="callout">'+esc(tdPlural('roots.scanSkipped',skipped,{ext:exts}))+'</div>'}
    html+='<div class="muted" style="margin-top:8px">'+esc(tdT('roots.scanSupported',{supported:(result.supported_extensions||[]).join(', ')}))+'</div>';
    if(errors.length){html+='<div class="callout">'+esc(tdPlural('roots.scanWarnings',errors.length))+errors.map(esc).join('<br>')+'</div>'}
    html+='<div class="muted" style="margin-top:8px"><a href="/progress">'+esc(tdT('roots.viewProgress'))+' →</a></div>';
    el.innerHTML=html;
  }catch(e){
    el.innerHTML=tdAuthDenied(e)?authNotice(null):'<div class="callout callout--contradicted">'+esc(tdT('roots.scanFailed',{message:e.message}))+'</div>';
  }finally{
    btn.disabled=false;btn.textContent=tdT('roots.startScan');
  }
}

// copyCmd reports what happened. The previous version swallowed every failure
// in a bare catch and gave no feedback on success either, so an operator who
// clicked and got nothing could not tell a copied command from a dead button —
// and execCommand is deprecated, so "nothing happened" is a real outcome.
function copyCmd(id,button){
  var el=document.getElementById(id);
  if(!el)return;
  var done=function(ok){
    if(!button)return;
    var original=button.dataset.copyLabel||button.textContent;
    button.dataset.copyLabel=original;
    button.textContent=tdT(ok?'roots.copied':'roots.copyFailed');
    setTimeout(function(){button.textContent=original},1500);
  };
  if(navigator.clipboard&&navigator.clipboard.writeText){
    navigator.clipboard.writeText(el.textContent).then(function(){done(true)},function(){done(legacyCopy(el))});
    return;
  }
  done(legacyCopy(el));
}

// legacyCopy is the fallback for a browser without the async clipboard API, or
// for a page served over plain HTTP where it is unavailable.
function legacyCopy(el){
  var range=document.createRange();
  range.selectNode(el);
  var selection=window.getSelection();
  selection.removeAllRanges();
  selection.addRange(range);
  var ok=false;
  try{ok=document.execCommand('copy')}catch(e){ok=false}
  selection.removeAllRanges();
  return ok;
}
</script></body></html>`
