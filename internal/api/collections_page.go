package api

import "net/http"

func (s *Server) collectionsPage(w http.ResponseWriter, r *http.Request) {
	s.serveLocalizedPage(w, r, "/collections", collectionsHTML)
}

// collectionsHTML is the Collections page: the basket of shots pinned from
// library search results, one expandable card per collection. Like every
// other page here it is one inline constant with no build step, and the admin
// token lives only in the shell's input element -- never in browser storage.
// The page keeps its own CSS minimal because the shell supplies the chrome;
// mutating actions (reorder, remove, delete) are admin routes, so they fail
// with the API's envelope error when the token input is empty. All product
// copy is localized: static HTML uses [[i18n:collections.*]] markers that
// serveLocalizedPage resolves server-side, and dynamic JavaScript copy calls
// the injected tdT/tdPlural/tdFormatDateTime/tdApiErrorMessage helpers.
const collectionsHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>NexusGate · [[i18n:collections.title]]</title><style>

.collections{display:grid;gap:12px;margin-top:20px}
.collections .panel{margin-bottom:0}
.coll-panel .coll-toggle{justify-content:space-between;cursor:pointer;width:100%;text-align:left;border:0;background:transparent;color:inherit;font:inherit}
.coll-panel.open .coll-toggle{border-bottom:1px solid var(--rule)}
.coll-title{display:grid;gap:3px;min-width:0}
.coll-title b{font-size:16px;color:var(--text)}
.coll-desc{color:var(--text-muted);font-size:12px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.coll-meta{color:var(--text-muted);font-size:12px;white-space:nowrap;font-family:var(--font-data);font-variant-numeric:tabular-nums}
.coll-tools{display:flex;justify-content:flex-end;gap:8px;padding:12px 18px 0}
.coll-shots{display:grid;gap:10px;padding:14px 18px 18px}
.shot-row{display:flex;justify-content:space-between;gap:14px;align-items:flex-start;border:1px solid var(--rule);border-radius:10px;padding:11px 13px;background:var(--surface)}
.shot-info{display:grid;gap:3px;min-width:0}
.shot-info b{color:var(--text);font-size:13px;word-break:break-all}
.shot-time{color:var(--text-muted);font-size:12px;font-weight:700;font-family:var(--font-data);font-variant-numeric:tabular-nums}
.shot-desc{color:var(--text-muted);font-size:12px;line-height:1.5}
.chips{display:flex;gap:5px;flex-wrap:wrap;margin-top:2px}
.shot-actions{display:flex;gap:6px;flex-wrap:wrap;justify-content:flex-end}
.player-wrap{width:100%}
.shot-player{width:100%;border-radius:10px;background:var(--inset)}
.coll-muted{color:var(--text-muted);font-size:12px;padding:4px 2px}
@media(max-width:760px){.wrap{padding:24px 16px 48px}
.coll-panel .coll-toggle{display:grid;grid-template-columns:minmax(0,1fr);gap:7px;align-items:start}
.coll-meta{white-space:normal;overflow-wrap:anywhere}
.coll-tools{justify-content:flex-start;flex-wrap:wrap}
.shot-row{display:grid;grid-template-columns:minmax(0,1fr);gap:10px}
.shot-actions{justify-content:flex-start;max-width:100%}
}

</style></head><body><!--SHELL_HEADER--><main class="wrap">
<header class="pagehead">
<h1>[[i18n:collections.title]]</h1>
<p class="muted">[[i18n:collections.intro]]</p>
</header>
<div id="collections-status" class="status" role="status" aria-live="polite"></div>
<section id="collections" class="collections"></section>
</main><script>
const shotCache={};
 function csrfToken(){const prefix='__Host-nexusgate_csrf=';const item=document.cookie.split('; ').find(function(x){return x.indexOf(prefix)===0});return item?decodeURIComponent(item.slice(prefix.length)):''}
 function authHeaders(base){const headers=new Headers(base||{});const csrf=csrfToken();if(csrf)headers.set('X-CSRF-Token',csrf);return headers}
function esc(s){return String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function collSay(message,ok){const el=document.getElementById('collections-status');el.className='callout '+(ok?'callout--confirmed':'callout--contradicted');el.textContent=message}
function fmtClock(ms){const s=Math.max(0,Math.round((ms||0)/1000));const h=Math.floor(s/3600),m=Math.floor(s%3600/60),sec=s%60;const mm=String(m).padStart(2,'0'),ss=String(sec).padStart(2,'0');return h?h+':'+mm+':'+ss:mm+':'+ss}
function fmtDur(ms){const s=Math.max(0,Math.round((ms||0)/1000));const m=Math.floor(s/60),sec=s%60;return m>=60?fmtClock(ms):m+':'+String(sec).padStart(2,'0')}
async function collectionsLoad(){try{const list=await fetch('/api/v1/collections',{headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await tdApiErrorMessage(r));return r.json()});collectionsRender(Array.isArray(list)?list:[]);const st=document.getElementById('collections-status');st.className='status';st.textContent=''}catch(e){document.getElementById('collections').innerHTML='';collSay(tdT('collections.loadError',{message:e.message}),false)}}
function collectionsRender(list){const el=document.getElementById('collections');if(!list.length){el.innerHTML='<div class="empty"><b>'+esc(tdT('collections.empty'))+'</b><span>'+esc(tdT('collections.emptyWhy'))+'</span><a class="btn btn--primary" href="/">'+esc(tdT('collections.empty.goLibrary'))+'</a></div>';return}el.innerHTML=list.map(collectionCard).join('')}
 function collectionCard(c){const count=Number.isFinite(Number(c.shot_count))?Number(c.shot_count):0;return '<div class="panel coll-panel" data-coll="'+esc(c.id)+'"><button type="button" class="panel-head coll-toggle" data-action="toggle-collection" aria-expanded="false" aria-controls="coll-shots-'+esc(c.id)+'"><div class="coll-title"><b>'+esc(c.name)+'</b>'+(c.description?'<span class="coll-desc">'+esc(c.description)+'</span>':'')+'</div><span class="coll-meta">'+esc(tdPlural('collections.shotCount',count))+' · '+esc(tdT('collections.duration',{dur:fmtDur(c.total_duration_ms||0)}))+' · '+esc(tdT('collections.createdAt',{date:tdFormatDateTime(new Date(c.created_at))}))+'</span></button><div class="coll-tools"><button class="ghost danger" data-action="delete-collection">'+esc(tdT('collections.deleteCollection'))+'</button></div><div class="coll-shots" data-shots="'+esc(c.id)+'" hidden></div></div>'}
function collectionToggle(id){const box=document.querySelector('[data-shots="'+id+'"]');const card=document.querySelector('[data-coll="'+id+'"]');if(!box||!card)return;const head=card.querySelector('.coll-toggle');if(!box.hidden){box.hidden=true;card.classList.remove('open');if(head)head.setAttribute('aria-expanded','false');return}box.hidden=false;card.classList.add('open');if(head)head.setAttribute('aria-expanded','true');if(box.dataset.loaded)return;box.dataset.loaded='1';shotsLoad(id)}
async function shotsLoad(id){const box=document.querySelector('[data-shots="'+id+'"]');if(!box)return;box.innerHTML='<div class="coll-muted">'+esc(tdT('collections.loadingShots'))+'</div>';try{const shots=await fetch('/api/v1/collections/'+encodeURIComponent(id)+'/shots',{headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await tdApiErrorMessage(r));return r.json()});shotCache[id]=Array.isArray(shots)?shots:[];shotsRender(id,shotCache[id])}catch(e){box.innerHTML='';collSay(tdT('collections.loadShotsError',{message:e.message}),false)}}
function shotsRender(id,shots){const box=document.querySelector('[data-shots="'+id+'"]');if(!box)return;if(!shots.length){box.innerHTML='<div class="coll-muted">'+esc(tdT('collections.emptyShots'))+'</div>';return}box.innerHTML=shots.map(function(s,i){return shotRow(id,s,i,shots.length)}).join('')}
function shotRow(cid,s,i,total){const objects=(s.objects||[]).map(function(o){return '<span class="chip">'+esc(o)+'</span>'}).join('');return '<div class="shot-row" data-cid="'+esc(cid)+'" data-shot="'+i+'"><div class="shot-info"><b>'+esc(s.filename||'')+'</b><span class="shot-time">'+fmtClock(s.start_ms)+' – '+fmtClock(s.end_ms)+'</span>'+(s.description?'<span class="shot-desc">'+esc(s.description)+'</span>':'')+(objects?'<span class="chips">'+objects+'</span>':'')+'</div><div class="shot-actions"><button class="ghost arrow" title="'+esc(tdT('collections.moveUp'))+'" data-action="move-shot" data-dir="-1"'+(i===0?' disabled':'')+'>↑</button><button class="ghost arrow" title="'+esc(tdT('collections.moveDown'))+'" data-action="move-shot" data-dir="1"'+(i===total-1?' disabled':'')+'>↓</button><button class="ghost" data-action="play-shot">'+esc(tdT('collections.play'))+'</button><button class="ghost" data-action="copy-timecode">'+esc(tdT('collections.copyTimecode'))+'</button><button class="ghost danger" data-action="remove-shot">'+esc(tdT('common.remove'))+'</button></div><div class="player-wrap" hidden></div></div>'}
async function shotMove(cid,i,dir){const shots=shotCache[cid];if(!shots||window.collectionReorders[cid])return;const j=i+dir;if(j<0||j>=shots.length)return;window.collectionReorders[cid]=true;const moved=shots.splice(i,1)[0];shots.splice(j,0,moved);const ids=shots.map(function(s){return s.shot_id});try{const response=await fetch('/api/v1/collections/'+encodeURIComponent(cid)+'/shots/reorder',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({shot_ids:ids})});if(!response.ok){if(response.status===409){collSay(tdT('collections.orderConflictRefreshing'),false);await shotsLoad(cid);return}throw Error(await tdApiErrorMessage(response))}shotsRender(cid,shots)}catch(e){collSay(tdT('collections.reorderError',{message:e.message}),false);await shotsLoad(cid)}finally{delete window.collectionReorders[cid]}}
async function shotRemove(cid,i){const shot=shotCache[cid]&&shotCache[cid][i];if(!shot)return;try{await fetch('/api/v1/collections/'+encodeURIComponent(cid)+'/shots/'+encodeURIComponent(shot.shot_id),{method:'DELETE',headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await tdApiErrorMessage(r))});await shotsLoad(cid);await collectionsLoad()}catch(e){collSay(tdT('collections.removeShotError',{message:e.message}),false)}}
async function collectionDelete(id){if(!confirm(tdT('collections.deleteConfirm')))return;try{await fetch('/api/v1/collections/'+encodeURIComponent(id),{method:'DELETE',headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await tdApiErrorMessage(r))});await collectionsLoad();collSay(tdT('collections.deleted'),true)}catch(e){collSay(tdT('collections.deleteError',{message:e.message}),false)}}
function shotPlay(cid,i){const shot=shotCache[cid]&&shotCache[cid][i];if(!shot)return;const row=document.querySelector('[data-shot="'+i+'"][data-cid="'+cid+'"]');if(!row)return;const wrap=row.querySelector('.player-wrap');document.querySelectorAll('.shot-player').forEach(function(v){if(v.closest('.player-wrap')!==wrap)v.pause()});if(!wrap||!wrap.hidden){if(wrap)wrap.hidden=true;return}let video=wrap.querySelector('video');if(!video){video=document.createElement('video');video.className='shot-player';video.controls=true;video.preload='metadata';video.src='/api/v1/assets/'+encodeURIComponent(shot.asset_id)+'/proxy#t='+(shot.start_ms/1000).toFixed(3)+','+(shot.end_ms/1000).toFixed(3);wrap.appendChild(video)}wrap.hidden=false;video.currentTime=shot.start_ms/1000;video.play().catch(function(){})}
async function copyTimecode(cid,i){const shot=shotCache[cid]&&shotCache[cid][i];if(!shot)return;const text=fmtClock(shot.start_ms)+'–'+fmtClock(shot.end_ms);try{await navigator.clipboard.writeText(text);collSay(tdT('collections.timecodeCopied',{tc:text}),true)}catch(e){const ta=document.createElement('textarea');ta.value=text;document.body.appendChild(ta);ta.select();try{document.execCommand('copy');collSay(tdT('collections.timecodeCopied',{tc:text}),true)}catch(e2){collSay(tdT('collections.copyTimecodeError',{message:e2.message}),false)}ta.remove()}}
window.collectionReorders={};document.getElementById('collections').addEventListener('click',function(e){const target=e.target.closest('[data-action]');if(!target)return;const card=target.closest('[data-coll]'),cid=card&&card.dataset.coll;if(!cid)return;const row=target.closest('[data-shot]'),i=row&&Number(row.dataset.shot);if(target.dataset.action==='toggle-collection')collectionToggle(cid);else if(target.dataset.action==='delete-collection'){e.stopPropagation();collectionDelete(cid)}else if(row&&target.dataset.action==='move-shot')shotMove(cid,i,Number(target.dataset.dir));else if(row&&target.dataset.action==='play-shot')shotPlay(cid,i);else if(row&&target.dataset.action==='copy-timecode')copyTimecode(cid,i);else if(row&&target.dataset.action==='remove-shot')shotRemove(cid,i)});collectionsLoad();
</script></body></html>`
