package api

import "strings"

var libraryIndexHTML = enhanceLibraryPage(legacyLibraryIndexHTML)

func enhanceLibraryPage(page string) string {
	page = strings.NewReplacer(
		"FOOTAGE LIBRARY · SHOT LEVEL", "EXPIRED FOOTAGE · RECLAIMED",
		"素材库 · 镜头浏览", "素材回收站 · 镜头浏览",
		"从缩略图、素材语义到每个时间段的镜头内容，一眼看清你的素材里有什么可以用。", "把被遗忘的镜头重新放回时间轴：从缩略图、语义到准确时间段，找回仍然值得使用的素材。",
	).Replace(page)
	page = strings.Replace(page,
		`.library{display:grid;gap:13px}`,
		`.filters{display:grid;grid-template-columns:repeat(6,minmax(120px,1fr)) auto;gap:9px;margin:0 0 12px;padding:13px;background:#121f34;border:1px solid #2b3e5b;border-radius:14px}.filters label{display:block;color:#93a6c6;font-size:10px;font-weight:800;letter-spacing:.06em;margin-bottom:5px}.filters input,.filters select{width:100%;padding:8px 9px}.filters button{align-self:end}.processing-summary{display:flex;gap:7px;flex-wrap:wrap;margin:0 0 18px}.status-pill{border:1px solid #385072;border-radius:999px;padding:5px 9px;color:#bed0ed;font-size:12px}.status-pill b{color:#fff}.group-title{margin:20px 2px 8px;color:#cbd8f0;font-size:13px;font-weight:850;letter-spacing:.02em}.library{display:grid;gap:13px}`,
		1,
	)
	page = strings.Replace(page,
		`<section id="library" class="library"`,
		`<section class="filters" aria-label="素材筛选"><div><label for="date-from">开始日期</label><input id="date-from" type="date" onchange="load()"></div><div><label for="date-to">结束日期</label><input id="date-to" type="date" onchange="load()"></div><div><label for="region-filter">地区</label><input id="region-filter" placeholder="例如 中国 · 深圳 · 南山" onchange="load()"></div><div><label for="camera-filter">相机</label><input id="camera-filter" placeholder="例如 Sony FX3" onchange="load()"></div><div><label for="session-filter">拍摄场次</label><select id="session-filter" onchange="load()"><option value="">全部场次</option></select></div><div><label for="status-filter">处理状态</label><select id="status-filter" onchange="load()"><option value="">全部状态</option><option value="ready">可用</option><option value="processing">处理中</option><option value="queued">等待中</option><option value="failed">失败</option><option value="discovered">未处理</option><option value="missing">原片缺失</option></select></div><div><label for="collection-filter">保存的视图</label><select id="collection-filter" onchange="loadCollection(this.value)"><option value="">不使用</option></select></div><button onclick="clearFilters()">清除筛选</button></section><div id="processing-summary" class="processing-summary" aria-label="处理状态汇总"></div><section id="library" class="library"`,
		1,
	)
	page = strings.Replace(page,
		`async function load(ids)`,
		`let activeCollection='';function filterQuery(){const values={date_from:document.getElementById('date-from').value,date_to:document.getElementById('date-to').value,region:document.getElementById('region-filter').value.trim(),camera:document.getElementById('camera-filter').value,session:document.getElementById('session-filter').value,status:document.getElementById('status-filter').value};const query=new URLSearchParams();Object.entries(values).forEach(([key,value])=>{if(value)query.set(key,value)});return query.toString()?'&'+query.toString():''}function clearFilters(){activeCollection='';['date-from','date-to','region-filter','camera-filter'].forEach(id=>document.getElementById(id).value='');document.getElementById('session-filter').value='';document.getElementById('status-filter').value='';document.getElementById('collection-filter').value='';load()}function groupKey(x){const date=x.captured_at?String(x.captured_at).slice(0,10):'日期未知';return [date,x.region_label||'地区未知',x.camera_model||'相机未知',x.session_id||'未归类场次'].join(' · ')}async function loadSessions(){const select=document.getElementById('session-filter');try{const response=await fetch('/api/v1/shoot-sessions?limit=500');if(!response.ok)throw Error('sessions unavailable');const sessions=await response.json();(Array.isArray(sessions)?sessions:[]).forEach(s=>{const option=document.createElement('option');option.value=s.id;const date=s.starts_at?String(s.starts_at).slice(0,10):'日期未知';const details=[date,s.camera_label,s.region_label].filter(Boolean).join(' · ');option.textContent=(s.title||s.id)+(details?' · '+details:'');select.appendChild(option)})}catch(_){select.innerHTML='<option value="">场次列表暂不可用</option>'}}async function loadCollections(){const select=document.getElementById('collection-filter');try{const items=await fetch('/api/v1/collections').then(r=>r.ok?r.json():[]);items.forEach(item=>{const option=document.createElement('option');option.value=item.id;option.textContent=item.name;select.appendChild(option)})}catch(_){}}async function loadSummary(){try{const data=await fetch('/api/v1/library/processing-summary?limit=300'+filterQuery()).then(r=>r.ok?r.json():null);if(!data)return;const labels={ready:'可用',processing:'处理中',queued:'等待中',failed:'失败',discovered:'未处理',missing:'原片缺失'};document.getElementById('processing-summary').innerHTML=Object.entries(labels).map(([key,label])=>'<span class="status-pill">'+label+' <b>'+esc((data.by_status||{})[key]||0)+'</b></span>').join('')}catch(_){}}async function loadCollection(id){activeCollection=id||'';load()}function loadLibrary(){loadSessions();loadCollections();load()}async function load(ids)`,
		1,
	)
	page = strings.Replace(page,
		`fetch('/api/v1/assets?limit=300')`,
		`fetch(activeCollection?'/api/v1/collections/'+encodeURIComponent(activeCollection)+'/assets?limit=300':'/api/v1/assets?limit=300'+filterQuery())`,
		1,
	)
	page = strings.Replace(page,
		`const rows=await Promise.all(data.map(async x=>row(x,await loadShots(x.id))));library.innerHTML=rows.join('')`,
		`await loadSummary();const groups=new Map();data.forEach(x=>{const key=groupKey(x);if(!groups.has(key))groups.set(key,[]);groups.get(key).push(x)});const sections=[];for(const [key,items] of groups){const rows=await Promise.all(items.map(async x=>row(x,await loadShots(x.id))));sections.push('<div class="group-title">'+esc(key)+'</div>'+rows.join(''))}library.innerHTML=sections.join('')`,
		1,
	)
	page = strings.Replace(page,
		`@media(max-width:720px){`,
		`@media(max-width:980px){.filters{grid-template-columns:repeat(3,1fr)}}@media(max-width:720px){.filters{grid-template-columns:1fr 1fr}`,
		1,
	)
	page = strings.Replace(page, `load();</script>`, `loadLibrary();</script>`, 1)
	return page
}
