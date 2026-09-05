package api

// legacyLibraryIndexHTML is the pre-v0.15 library base that library_page_v015.go's patch chain overlays.
// All product copy is referenced through [[i18n:library.*]] markers (static HTML)
// and tdT('library.*') calls (dynamic JS); the markers are resolved by
// serveLocalizedPage, so the same constant renders in every UI locale.
const legacyLibraryIndexHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>NexusGate · [[i18n:library.title]]</title><style>

.legend{display:flex;gap:12px;color:var(--text-muted);font-size:12px;align-items:center}
.library{display:grid;gap:13px}
.asset-row{display:grid;grid-template-columns:220px minmax(220px,.75fr) minmax(380px,1.75fr);gap:18px;align-items:stretch;padding:13px;background:var(--surface);border:1px solid var(--rule);border-radius:16px;transition:border-color .15s,transform .15s}
.asset-row:hover{border-color:var(--rule-strong);transform:translateY(-1px)}
.thumb,.thumb-empty{width:100%;height:100%;min-height:126px;aspect-ratio:16/9;object-fit:cover;border-radius:10px;background:var(--inset)}
.thumb-empty{display:grid;place-items:center;color:var(--text-muted);font-size:12px;border:1px dashed var(--rule-strong)}
.asset-info{display:flex;min-width:0;flex-direction:column;justify-content:center;padding:4px 0}
.filename{font-size:16px;font-weight:800;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;letter-spacing:.01em}
.asset-meta{margin-top:7px;color:var(--text-muted);font-size:12px;font-family:var(--font-data);font-variant-numeric:tabular-nums}
.summary{margin:10px 0;color:var(--text-muted);line-height:1.45}
.chips{display:flex;flex-wrap:wrap;gap:5px}
.chip.voice{color:var(--ev-confirmed)}
.asset-details{display:flex;flex-wrap:wrap;gap:5px;margin-top:8px}
.detail{font-size:11px;color:var(--text-muted);background:var(--inset);border-radius:6px;padding:3px 6px}
.detail b{color:var(--text);margin-right:4px}
.timeline-card{min-width:0;display:flex;flex-direction:column;justify-content:center;padding:4px 3px}
.timeline-head{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:10px}
.timeline-title{font-size:12px;font-weight:800;color:var(--text-muted)}
.duration{color:var(--text-muted);font-size:12px;font-family:var(--font-data);font-variant-numeric:tabular-nums}
.tickrule{display:flex;flex-direction:column;gap:6px;min-width:0}
.tickrule-track{position:relative;height:34px;border:1px solid var(--rule);border-radius:var(--r-sm);background-color:var(--surface);background-image:repeating-linear-gradient(90deg,var(--graticule) 0 1px,transparent 1px 48px);overflow:hidden}
.tickrule-span{position:absolute;top:0;bottom:0;border-right:1px solid var(--surface);background:var(--span-fill);display:flex;align-items:center;padding:0 8px;overflow:hidden}
.tickrule-span>span{font-family:var(--font-data);font-size:11px;color:var(--text);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.tickrule-span.is-hit{background:var(--ev-confirmed-wash);box-shadow:inset 0 0 0 1px var(--ev-confirmed)}
.tickrule-span.is-possible{background:var(--ev-draft-wash);box-shadow:inset 0 0 0 1px var(--ev-draft)}
.tickrule-span:last-child{border-right:0}
.tickrule-scale{display:flex;justify-content:space-between;font-family:var(--font-data);font-size:11px;font-variant-numeric:tabular-nums;color:var(--text-faint)}
.timeline-empty{min-height:34px;display:flex;flex-direction:column;align-items:flex-start;justify-content:center;gap:var(--s2);border:1px dashed var(--rule-strong);border-radius:var(--r-sm);color:var(--text-muted);font-size:12px;padding:var(--s2) var(--s3)}
.empty.error{color:var(--ev-contradicted)}
.loading{color:var(--text-muted);font-size:13px;padding:24px}
@media(max-width:980px){.asset-row{grid-template-columns:170px minmax(190px,.8fr) minmax(280px,1.4fr)}}
@media(max-width:720px){.wrap{padding:28px 20px 44px}
.pagehead{display:block}
.legend{margin-top:16px}
.asset-row{grid-template-columns:1fr;gap:13px}
.thumb,.thumb-empty{min-height:auto;height:auto}
.timeline-card{padding:0}
.asset-info{padding:0}
}

</style></head><body><!--SHELL_HEADER--><main class="wrap" data-library-browser><header class="pagehead"><div><div class="eyebrow">[[i18n:library.eyebrow]]</div><h1>[[i18n:library.heading]]</h1><p class="muted">[[i18n:library.subtitle]]</p></div><div class="legend"><span><i></i> [[i18n:library.legend.shots]]</span><span><i></i> [[i18n:library.legend.range]]</span><span><i></i> [[i18n:library.legend.semantics]]</span></div></header><section id="library" class="library" aria-live="polite"><div class="loading">[[i18n:library.loading]]</div></section></main><script>
const library=document.getElementById('library');const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));const fmt=ms=>{ms=Math.max(0,Math.floor((ms||0)/1000));return String(Math.floor(ms/60)).padStart(2,'0')+':'+String(ms%60).padStart(2,'0')};
function loadShots(id){return fetch('/api/v1/assets/'+encodeURIComponent(id)+'/shots').then(r=>r.ok?r.json():[]).then(x=>Array.isArray(x)?x:[]).catch(()=>[])}
function chips(x){const values=[x.asset_type,x.camera_motion,x.lighting,...(x.mood_tags||[])].filter(Boolean).slice(0,5);return values.map(v=>'<span class="chip">'+esc(v)+'</span>').join('')+(x.has_speech?'<span class="chip voice">'+tdT('library.hasSpeech')+'</span>':'')}
function timeline(x,shots){const duration=Math.max(Number(x.duration_ms)||0,1);if(!shots.length)return '<div class="timeline-empty">'+tdT('library.timelineEmpty')+'</div>';const labels=['00:00',fmt(duration/2),fmt(duration)];const blocks=shots.map(s=>{const start=Math.max(0,Number(s.start_ms)||0),end=Math.max(start,Number(s.end_ms)||start),left=Math.min(100,start/duration*100),width=Math.max(1,(end-start)/duration*100),description=s.description||((s.tags||[]).join(' · '))||tdT('library.unnamedShot'),title=fmt(start)+' — '+fmt(end)+' · '+description;return '<div class="tickrule-span is-possible" style="left:'+left.toFixed(3)+'%;width:'+width.toFixed(3)+'%" title="'+esc(title)+'"><span>'+esc(description)+'</span></div>'}).join('');return '<div class="tickrule" aria-label="'+tdT('library.timelineAria')+'"><div class="tickrule-track">'+blocks+'</div><div class="tickrule-scale"><span>'+labels[0]+'</span><span>'+labels[1]+'</span><span>'+labels[2]+'</span></div></div>'}
function thumbnail(x){return x.thumbnail_url?'<img class="thumb" loading="lazy" src="'+esc(x.thumbnail_url)+'" alt="'+esc(tdT('library.thumbnailAlt',{name:x.filename}))+'" onerror="this.outerHTML=\'<div class=&quot;thumb-empty&quot;>'+tdT('library.thumbnailUnavailable')+'</div>\'">':'<div class="thumb-empty">'+tdT('library.thumbnailNone')+'</div>'}
function optionalDetails(x){const fields=[[tdT('library.detail.camera'),x.camera_model],[tdT('library.detail.region'),x.region_label],[tdT('library.detail.session'),x.session_id],[tdT('library.detail.color'),x.source_color],[tdT('library.detail.profile'),x.color_profile],[tdT('library.detail.rawFormat'),x.raw_format],[tdT('library.detail.preview'),x.preview_status]].filter(([,value])=>value);return fields.length?'<div class="asset-details" aria-label="'+tdT('library.detailAria')+'">'+fields.map(([label,value])=>'<span class="detail"><b>'+esc(label)+'</b>'+esc(value)+'</span>').join('')+'</div>':''}
function row(x,shots){return '<article class="asset-row"><div>'+thumbnail(x)+'</div><div class="asset-info"><div class="filename" title="'+esc(x.filename)+'">'+esc(x.filename||tdT('library.unnamedAsset'))+'</div><div class="asset-meta">'+fmt(x.duration_ms)+' · '+esc(x.orientation||tdT('library.orientationUnknown'))+' · '+esc(x.state||tdT('library.stateUnknown'))+'</div><p class="summary">'+esc(x.summary||tdT('library.summaryPending'))+'</p><div class="chips">'+chips(x)+'</div>'+optionalDetails(x)+'</div><div class="timeline-card"><div class="timeline-head"><span class="timeline-title">'+tdT('library.timelineTitle')+'</span><span class="duration">'+fmt(x.duration_ms)+'</span></div>'+timeline(x,shots)+'</div></article>'}
async function load(ids){library.innerHTML='<div class="loading">'+tdT('library.loadingTimeline')+'</div>';try{let data=await fetch('/api/v1/assets?limit=300').then(r=>r.ok?r.json():[]);data=Array.isArray(data)?data:[];if(ids)data=data.filter(x=>ids.includes(x.id));if(!data.length){library.innerHTML='<div class="empty">'+esc(tdT('library.emptyTitle'))+'<a href="/setup" style="color:var(--brand);text-decoration:underline">'+esc(tdT('library.emptySetupLink'))+'</a>'+esc(tdT('library.emptyTail'))+'<br><a class="btn btn--primary" href="/library-roots" style="margin-top:14px">'+esc(tdT('library.emptyRootsLink'))+'</a></div>';return}const rows=await Promise.all(data.map(async x=>row(x,await loadShots(x.id))));library.innerHTML=rows.join('')}catch(e){library.innerHTML='<div class="empty error">'+esc(tdT('library.loadFailed'))+esc(e.message)+'</div>'}}
async function search(){const q=document.getElementById('q').value.trim();if(!q)return load();try{const ids=await fetch('/api/v1/search?q='+encodeURIComponent(q)).then(r=>r.ok?r.json():[]);load(Array.isArray(ids)?ids:[])}catch(e){library.innerHTML='<div class="empty error">'+esc(tdT('library.searchFailed'))+esc(e.message)+'</div>'}}document.getElementById('q').addEventListener('keydown',e=>{if(e.key==='Enter')search()});load();</script></body></html>`
