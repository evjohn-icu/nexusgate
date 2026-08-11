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
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#101827;color:#edf3ff;font:14px ui-sans-serif,system-ui,-apple-system,sans-serif}header{padding:15px 5vw;border-bottom:1px solid #293953;display:flex;gap:18px;align-items:center;background:#101827ee;position:sticky;top:0;z-index:1;backdrop-filter:blur(12px)}a{color:#b8c8ff;text-decoration:none}.brand{color:#fff;font-weight:800;margin-right:auto}header input{width:auto;max-width:260px;padding:9px 10px;border-radius:8px;border:1px solid #354764;background:#0e1728;color:#fff;font:inherit}.wrap{max-width:900px;margin:auto;padding:32px 24px 64px}h1{font-size:32px;margin:0 0 6px;letter-spacing:-.04em}.muted{color:#aab8d0;line-height:1.6;margin:0}.panel{background:#172238;border:1px solid #2c3d5b;border-radius:14px;padding:20px;margin-top:20px}.panel h2{margin:0 0 6px;font-size:17px}.panel .hint{color:#9fb0ce;font-size:13px;line-height:1.6;margin:0 0 16px}.row{display:grid;grid-template-columns:220px 1fr;gap:14px;align-items:start;padding:12px 0;border-top:1px solid #24344e}.row:first-of-type{border-top:0}label{font-weight:700;color:#dce6fb}.row .note{color:#93a5c3;font-size:12px;line-height:1.5;margin:6px 0 0}input[type=number],input[type=text],input[type=time]{width:170px;padding:9px 10px;border-radius:8px;border:1px solid #354764;background:#0e1728;color:#fff;font:inherit}input[type=checkbox]{width:17px;height:17px;accent-color:#86a3ff}button{background:#86a3ff;color:#091227;border:0;border-radius:9px;padding:11px 16px;font-weight:800;cursor:pointer}button.ghost{background:#31446a;color:#dbe6ff}.actions{display:flex;gap:10px;align-items:center;margin-top:22px}.status{border-radius:10px;padding:12px 14px;margin-top:18px;line-height:1.6;display:none}.status.ok{display:block;background:#153c31;color:#a4efc8;border:1px solid #23664f}.status.bad{display:block;background:#3f2230;color:#ffc2cc;border:1px solid #6d3547}.state{display:flex;flex-wrap:wrap;gap:10px;margin-top:4px}.pill{border-radius:99px;padding:5px 11px;font-size:12px;font-weight:700;background:#243651;color:#c3d3f2}.pill.live{background:#153c31;color:#a4efc8}.pill.hold{background:#43331c;color:#ffd79a}@media(max-width:720px){.row{grid-template-columns:1fr}}</style></head><body><!--SHELL_HEADER--><main class="wrap">
<h1>设置</h1>
<p class="muted">这里的改动立即生效，不需要重启 Hub —— 处理流程每取一个作业前都会重读一次限制，所以正在跑的扫描也会被当场收紧。</p>
<section class="panel"><h2>存储概览</h2><p class="hint">原片一行是<b>估算（只读）</b>：来自入库时记录的体积，不是实时扫描 —— 原片永远留在素材库盘上，Hub 只读。其余都在 Hub 缓存盘上：派生代理、缩略图、抽音轨都可以从原片重新生成，<b>可安全释放</b>；临时文件随时可删。</p><div id="storage-rows" class="state"><span class="pill">正在读取…</span></div></section>
<section class="panel"><h2>当前状态</h2><div id="state" class="state"><span class="pill">正在读取…</span></div></section>
<section class="panel"><h2>磁盘负载限制</h2><p class="hint">处理流程是串行的，所以这里不是调并发。单个 FFmpeg 进程全速读一个 4K 文件就足以把机械盘或 NAS 链路占满，而整库扫描会让它持续几个小时 —— 真正能降低持续负载的是限制读取速率和作业间歇。两项默认都关闭。</p>
<div class="row"><label for="read-rate">读取速率上限</label><div><input id="read-rate" type="number" min="0" step="0.25" placeholder="0"><p class="note">FFmpeg 读源文件的速度上限，单位是「实时播放倍数」：1 表示一个 10 分钟的片子花 10 分钟读完。<b>0 表示不限速</b>。只有整文件读取（proxy、抽音轨）会遵守它；缩略图只解一帧，限速只会拖慢定位。下限 0.25。</p></div></div>
<div class="row"><label for="cooldown">作业间冷却</label><div><input id="cooldown" type="number" min="0" max="3600" step="5" placeholder="0"><p class="note">每完成一个作业后强制等待的秒数。这一项才是把「连续满载几小时」变成「间歇负载」的开关；只限速率的话磁盘仍然是一直在读。0 表示不等待，上限 3600。</p></div></div>
</section>
<section class="panel"><h2>磁盘空间保护</h2><p class="hint">处理流程把派生产物（缩略图、代理视频、抽音轨）写到 Hub 的缓存盘。缓存盘满了，FFmpeg 会在编码中途报「磁盘已满」—— 这类失败现在会被识别出来，作业推迟而不是原地重试烧掉重试次数；下面的设定让作业在开跑前就提前检查剩余空间。</p>
<div class="row"><label for="min-free-space">最小剩余空间</label><div><input id="min-free-space" type="number" min="0" step="1" placeholder="0"> GB<p class="note">缓存盘剩余空间低于这个值时，重作业（派生、转写、分析）不启动，直接按磁盘空间低推迟，10 分钟后自动再试，不消耗重试次数。<b>0 表示关闭检查</b>。磁盘满失败本身无论是否开启都会按同一方式推迟。</p></div></div>
</section>
<section class="panel"><h2>成本参考</h2><p class="hint">云调用（分析、转写）的估算按服务商渠道的成本元数据逐笔记入账本，这里设置当日 / 当月成本参考值。参考值只帮助观察成本，不会限制或推迟云调用；账本是追加式的事后估算记录，永远不是账单。单位与渠道成本元数据（每次请求 / 每分钟视频 / 每分钟音频）一致。<b>0 表示不显示参考值</b>。</p>
<div class="row"><label for="daily-cost-guide">每日成本参考</label><div><input id="daily-cost-guide" type="number" min="0" step="0.01" placeholder="0"><p class="note">当日（UTC）估算参考值，仅用于对照；达到或超过它不会阻止分析 / 转写。<b>0 表示不设置</b>。</p></div></div>
<div class="row"><label for="monthly-cost-guide">每月成本参考</label><div><input id="monthly-cost-guide" type="number" min="0" step="0.01" placeholder="0"><p class="note">当月（UTC）估算参考值，仅用于对照；达到或超过它不会阻止作业。<b>0 表示不设置</b>。</p></div></div>
</section>
<section class="panel"><h2>凌晨时段</h2><p class="hint">时间按 <b id="server-zone">Hub 本地时区</b> 判断，当前 Hub 时间 <b id="server-time">--:--</b>。窗口内不限冷却（夜里没人要保护，全速跑完才是重点）。结束时间早于开始时间表示跨过午夜，例如 22:00–06:00。</p>
<div class="row"><label for="off-peak">启用时段调度</label><div><input id="off-peak" type="checkbox"><p class="note">关闭后，被推迟的作业会立刻释放，不会滞留。</p></div></div>
<div class="row"><label for="off-start">开始 / 结束</label><div><input id="off-start" type="time"> <input id="off-end" type="time"><p class="note">含开始时刻，不含结束时刻。</p></div></div>
<div class="row"><label for="defer-mb">超过多大的文件推迟到时段内</label><div><input id="defer-mb" type="number" min="0" step="100" placeholder="0"> MB<p class="note">大于这个体积的素材只在时段内处理，其余随时处理。<b>0 表示不推迟任何文件</b>。用体积而不是时长做判断，因为磁盘感受到的是读了多少字节。被推迟的作业不会被启动，因此不会消耗重试次数。</p></div></div>
<div class="row"><label for="immediate-mb">小于多大的文件永不受限</label><div><input id="immediate-mb" type="number" min="0" step="10" placeholder="0"> MB<p class="note">这个体积以下的素材同时豁免推迟和限速。手机短片派生几乎不花代价，限它只会让素材库显得坏掉而省不下什么。必须小于上面那个阈值。</p></div></div>
</section>
<div class="actions"><button id="save" onclick="save()">保存</button><button class="ghost" onclick="load()">放弃修改</button></div>
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
pills.push('<span class="pill">冷却 '+(t.cooldown_seconds?esc(t.cooldown_seconds)+' 秒':'无')+'</span>');
if(t.off_peak_enabled){pills.push('<span class="pill '+(d.off_peak_open?'live':'hold')+'">时段 '+esc(t.off_peak_start)+'–'+esc(t.off_peak_end)+(d.off_peak_open?' · 进行中':' · 未开始')+'</span>')}
else{pills.push('<span class="pill">时段调度已关闭</span>')}
if(d.holding_above>0){pills.push('<span class="pill hold">正在推迟 &gt;'+esc(Math.round(d.holding_above/MB))+' MB 的素材，'+esc(d.next_off_peak)+' 开始处理</span>')}
pills.push('<span class="pill">当前实际冷却 '+esc(d.cooldown_now_s)+' 秒</span>');
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
async function loadStorage(){const rows=[['原片（估算 · 只读）','original_estimate_bytes','来自入库记录，不扫描素材盘'],['派生代理','derived_bytes','缩略图+代理+抽音轨'],['缩略图','thumbnail_bytes'],['音频','audio_bytes'],['源文件暂存','source_staging_bytes','copy 模式的本地镜像，删除只是丢缓存速度'],['临时文件','temporary_bytes','分析中间文件与未归类残留，随时可删'],['数据库','database_bytes'],['磁盘剩余','free_disk_bytes'],['可安全释放（可重建）','rebuildable_bytes','全部可以从原片重新生成']];
try{const d=await fetch('/api/v1/storage/overview',{headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await apiErrMsg(r));return r.json()});document.getElementById('storage-rows').innerHTML=rows.map(([label,key,note])=>{const v=d[key];return '<div class="row"><label>'+esc(label)+'</label><div>'+(v===undefined?'—':fmtBytes(v))+(note?'<p class="note">'+esc(note)+'</p>':'')+'</div></div>'}).join('')}catch(e){document.getElementById('storage-rows').innerHTML='<span class="pill bad">无法读取存储概览：'+esc(e.message)+'</span>'}}
async function renderStateRefresh(){try{const d=await fetch('/api/v1/pipeline/throttle',{headers:authHeaders()}).then(r=>r.ok?r.json():null);if(d)renderState(d)}catch(e){}}
</script></body></html>`
