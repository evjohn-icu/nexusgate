package api

import "net/http"

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(brandedPage(shelledPage(settingsHTML))))
}

// settingsHTML is the editable view of domain.PipelineThrottle. Like every other
// page here it is one inline constant with no build step, and the admin token
// lives only in the input element -- never in browser storage.
//
// The page deliberately shows the Hub's own clock and time zone. The off-peak
// window is evaluated in the Hub's local time, so an operator configuring
// "01:00" from a laptop in another zone would otherwise have no way to tell that
// it means the Hub's 01:00 and not theirs.
const settingsHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · 设置</title><style>

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
<h1>设置</h1>
<p class="muted">这里的改动立即生效，不用重启 Hub——处理流程每次开工前都会重读限制，正在跑的任务也会马上被约束。</p>
<section class="panel"><h2>存储概览</h2><p class="hint">「原始素材」一行是<b>估算（只读）</b>：来自入库时记的体积，不是实时扫描——原始素材永远留在素材库盘上，Hub 只读。其余都在 Hub 缓存盘上：预览视频、缩略图、音频都可以从原始素材重新生成，<b>删了能再生成</b>；临时文件随时可删。</p><div id="storage-rows"><span class="pill">正在读取…</span></div></section>
<section class="panel"><h2>当前状态</h2><div id="state" class="state-strip"><span class="pill">正在读取…</span></div></section>
<section class="panel"><h2>磁盘负载控制</h2><p class="hint">处理是一个一个来的，所以这里不是调同时跑几个。单个转码进程全速读一个 4K 文件就能把机械盘或 NAS 通道占满，整库扫描会持续几小时——真正能降负载的是限制读取速度和任务间隙。两项默认都关着。</p>
<div class="row"><label for="read-rate">读取速率上限</label><div><input id="read-rate" type="number" min="0" step="0.25" placeholder="0"><p class="note">读源文件的速度上限，单位是「实时倍数」：1 表示一个 10 分钟的片子用 10 分钟读完。<b>0 不限速</b>。只有整文件读取（proxy、抽音轨）会遵守它；缩略图只解一帧，限速只会拖慢定位。下限 0.25。</p></div></div>
<div class="row"><label for="cooldown">任务间等待</label><div><input id="cooldown" type="number" min="0" max="3600" step="5" placeholder="0"><p class="note">每完成一个任务后强制等的秒数。这一项才是把「连续满载几小时」变成「间歇负载」的开关；只限速度的话磁盘还是不停在读。0 表示不等待，上限 3600。</p></div></div>
</section>
<section class="panel"><h2>磁盘空间保护</h2><p class="hint">处理流程把生成的产物（缩略图、预览视频、抽音轨）写到 Hub 的缓存盘。缓存盘满了，FFmpeg 会在编码中途报「磁盘已满」—— 这类失败现在会被识别出来，作业延后而不是原地重试烧掉重试次数；下面的设定让作业在开跑前就提前检查剩余空间。</p>
<div class="row"><label for="min-free-space">最小剩余空间</label><div><input id="min-free-space" type="number" min="0" step="1" placeholder="0"> GB<p class="note">缓存盘剩余空间低于这个值时，重作业（生成、转写、分析）不启动，直接按磁盘空间低延后，10 分钟后自动再试，不消耗重试次数。<b>0 表示关闭检查</b>。磁盘满失败本身无论是否开启都会按同一方式延后。</p></div></div>
</section>
<section class="panel"><h2>成本参考值</h2><p class="hint">云调用（分析、转写）的估算按服务商给的计费信息逐笔记入账本，这里设置当日 / 当月成本参考值。参考值只帮助观察成本，不会限制或推迟云调用；账本是追加式的事后估算记录，永远不是账单。单位与服务商给的计费信息（每次请求 / 每分钟视频 / 每分钟音频）一致。<b>0 表示不显示参考值</b>。</p>
<div class="row"><label for="daily-cost-guide">每日成本参考</label><div><input id="daily-cost-guide" type="number" min="0" step="0.01" placeholder="0"><p class="note">当日（UTC）估算参考值，仅用于对照；达到或超过它不会阻止分析 / 转写。<b>0 表示不设置</b>。</p></div></div>
<div class="row"><label for="monthly-cost-guide">每月成本参考</label><div><input id="monthly-cost-guide" type="number" min="0" step="0.01" placeholder="0"><p class="note">当月（UTC）估算参考值，仅用于对照；达到或超过它不会阻止作业。<b>0 表示不设置</b>。</p></div></div>
</section>
<section class="panel"><h2>凌晨时段</h2><p class="hint">时间按 <b id="server-zone">Hub 本地时区</b> 判断，当前 Hub 时间 <b id="server-time">--:--</b>。窗口内不限等待（夜里没人要保护，全速跑完才是重点）。结束时间早于开始时间表示跨过午夜，例如 22:00–06:00。</p>
<div class="row"><label for="off-peak">启用时段安排</label><div><input id="off-peak" type="checkbox"><p class="note">关闭后，被延后的任务会立刻放出，不滞留。</p></div></div>
<div class="row"><label for="off-start">开始 / 结束</label><div><input id="off-start" type="time"> <input id="off-end" type="time"><p class="note">含开始时刻，不含结束时刻。</p></div></div>
<div class="row"><label for="defer-mb">超过多大的文件延后到时段内处理</label><div><input id="defer-mb" type="number" min="0" step="100" placeholder="0"> MB<p class="note">大于这个体积的素材只在时段内处理，其余随时处理。<b>0 表示不延后任何文件</b>。用体积而不是时长做判断，因为磁盘感受到的是读了多少字节。被延后的任务不会被启动，因此不会消耗重试次数。</p></div></div>
<div class="row"><label for="immediate-mb">小于多大的文件随时处理</label><div><input id="immediate-mb" type="number" min="0" step="10" placeholder="0"> MB<p class="note">这个体积以下的素材同时豁免延后和限速。手机短片生成几乎不花代价，限它只会让素材库显得坏掉而省不下什么。必须小于上面那个阈值。</p></div></div>
</section>
<div class="actions"><button id="save" class="btn btn--primary" onclick="save()">保存</button><button class="ghost" onclick="load()">放弃修改</button></div>
<div id="status" class="status"></div>
</main><script>
const MB=1048576;const GB=1073741824;
 function csrfToken(){const prefix='__Host-timingdex_csrf=';const item=document.cookie.split('; ').find(function(x){return x.indexOf(prefix)===0});return item?decodeURIComponent(item.slice(prefix.length)):''}
 function authHeaders(base){const headers=new Headers(base||{});const csrf=csrfToken();if(csrf)headers.set('X-CSRF-Token',csrf);return headers}
async function apiErrMsg(r){try{const d=await r.json();if(d&&d.error&&d.error.message)return d.error.action?(d.error.message+'（'+d.error.action+'）'):d.error.message}catch(_){}return (await r.text()).trim()}
function esc(s){return String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function val(id){return document.getElementById(id).value.trim()}
function num(id){const v=parseFloat(val(id));return Number.isFinite(v)&&v>0?v:0}
function say(message,ok){const el=document.getElementById('status');el.className='status '+(ok?'ok':'bad');el.textContent=message}
function renderState(d){const t=d.throttle||{};const pills=[];
pills.push('<span class="pill">读取速率 '+(t.read_rate?esc(t.read_rate)+'x':'不限')+'</span>');
pills.push('<span class="pill">等待 '+(t.cooldown_seconds?esc(t.cooldown_seconds)+' 秒':'无')+'</span>');
if(t.off_peak_enabled){pills.push('<span class="pill '+(d.off_peak_open?'live':'hold')+'">时段 '+esc(t.off_peak_start)+'–'+esc(t.off_peak_end)+(d.off_peak_open?' · 进行中':' · 未开始')+'</span>')}
else{pills.push('<span class="pill">时段调度已关闭</span>')}
if(d.holding_above>0){pills.push('<span class="pill hold">正在延后 &gt;'+esc(Math.round(d.holding_above/MB))+' MB 的素材，'+esc(d.next_off_peak)+' 开始处理</span>')}
pills.push('<span class="pill">当前实际等待 '+esc(d.cooldown_now_s)+' 秒</span>');
if(t.minimum_free_space_bytes>0){pills.push('<span class="pill">剩余空间保护 &gt;'+esc(Math.round(t.minimum_free_space_bytes/GB))+' GB</span>')}
if(t.daily_cost_guide>0){pills.push('<span class="pill">每日成本参考 '+esc(t.daily_cost_guide)+'</span>')}
if(t.monthly_cost_guide>0){pills.push('<span class="pill">每月成本参考 '+esc(t.monthly_cost_guide)+'</span>')}
document.getElementById('state').innerHTML=pills.join('');
document.getElementById('server-time').textContent=d.server_time||'--:--';
document.getElementById('server-zone').textContent=d.server_zone||'Hub 本地时区'}
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
async function load(){try{const d=await fetch('/api/v1/pipeline/throttle',{headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await apiErrMsg(r));return r.json()});fill(d);document.getElementById('status').className='status'}catch(e){say('无法读取设置：'+e.message,false)}}
async function save(){const b=document.getElementById('save');b.disabled=true;
const body={read_rate:num('read-rate'),cooldown_seconds:Math.round(num('cooldown')),off_peak_enabled:document.getElementById('off-peak').checked,off_peak_start:val('off-start')||'01:00',off_peak_end:val('off-end')||'07:00',defer_above_bytes:Math.round(num('defer-mb')*MB),immediate_max_bytes:Math.round(num('immediate-mb')*MB),minimum_free_space_bytes:Math.round(num('min-free-space')*GB),daily_cost_guide:num('daily-cost-guide'),monthly_cost_guide:num('monthly-cost-guide')};
try{const d=await fetch('/api/v1/pipeline/throttle',{method:'PUT',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify(body)}).then(async r=>{if(!r.ok)throw Error(await apiErrMsg(r));return r.json()});fill(d);say('已保存，下一个作业开始生效。',true)}catch(e){say('保存失败：'+e.message,false)}finally{b.disabled=false}}
load();loadStorage();setInterval(renderStateRefresh,15000);
function fmtBytes(n){const v=Number(n)||0;if(v<1024)return Math.round(v)+' B';const u=['KB','MB','GB','TB'];let i=0;let x=v;while(x>=1024&&i<u.length-1){x/=1024;i++}return x.toFixed(1)+' '+u[i]}
async function loadStorage(){const rows=[['原始素材（估算 · 只读）','original_estimate_bytes','来自入库记录，不扫描素材盘'],['生成的预览视频','derived_bytes','缩略图+预览+音频'],['缩略图','thumbnail_bytes'],['音频','audio_bytes'],['源文件缓存','source_staging_bytes','copy 模式的本地镜像，删除只是丢缓存速度'],['临时文件','temporary_bytes','分析中间文件与未归类残留，随时可删'],['数据库','database_bytes'],['磁盘剩余','free_disk_bytes'],['可安全删除（能重新生成）','rebuildable_bytes','全部可以从原始素材重新生成']];
try{const d=await fetch('/api/v1/storage/overview',{headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await apiErrMsg(r));return r.json()});document.getElementById('storage-rows').innerHTML=rows.map(([label,key,note])=>{const v=d[key];return '<div class="kv"><span class="k">'+esc(label)+'</span><span class="v">'+(v===undefined?'—':fmtBytes(v))+(note?'<div class="muted">'+esc(note)+'</div>':'')+'</span></div>'}).join('')}catch(e){document.getElementById('storage-rows').innerHTML='<span class="pill bad">无法读取存储概览：'+esc(e.message)+'</span>'}}
async function renderStateRefresh(){try{const d=await fetch('/api/v1/pipeline/throttle',{headers:authHeaders()}).then(r=>r.ok?r.json():null);if(d)renderState(d)}catch(e){}}
</script></body></html>`
