package api

import "net/http"

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(shelledPage(brandedPage(settingsHTML))))
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
<p class="muted">这里的改动立即生效，不需要重启 Hub —— 管线每取一个作业前都会重读一次限制，所以正在跑的扫描也会被当场收紧。</p>
<section class="panel"><h2>当前状态</h2><div id="state" class="state"><span class="pill">正在读取…</span></div></section>
<section class="panel"><h2>磁盘负载限制</h2><p class="hint">管线是串行的，所以这里不是调并发。单个 FFmpeg 进程全速读一个 4K 文件就足以把机械盘或 NAS 链路占满，而整库扫描会让它持续几个小时 —— 真正能降低持续负载的是限制读取速率和作业间歇。两项默认都关闭。</p>
<div class="row"><label for="read-rate">读取速率上限</label><div><input id="read-rate" type="number" min="0" step="0.25" placeholder="0"><p class="note">FFmpeg 读源文件的速度上限，单位是「实时播放倍数」：1 表示一个 10 分钟的片子花 10 分钟读完。<b>0 表示不限速</b>。只有整文件读取（proxy、抽音轨）会遵守它；缩略图只解一帧，限速只会拖慢定位。下限 0.25。</p></div></div>
<div class="row"><label for="cooldown">作业间冷却</label><div><input id="cooldown" type="number" min="0" max="3600" step="5" placeholder="0"><p class="note">每完成一个作业后强制等待的秒数。这一项才是把「连续满载几小时」变成「间歇负载」的开关；只限速率的话磁盘仍然是一直在读。0 表示不等待，上限 3600。</p></div></div>
</section>
<section class="panel"><h2>凌晨时段</h2><p class="hint">时间按 <b id="server-zone">Hub 本地时区</b> 判断，当前 Hub 时间 <b id="server-time">--:--</b>。窗口内不限冷却（夜里没人要保护，全速跑完才是重点）。结束时间早于开始时间表示跨过午夜，例如 22:00–06:00。</p>
<div class="row"><label for="off-peak">启用时段调度</label><div><input id="off-peak" type="checkbox"><p class="note">关闭后，被推迟的作业会立刻释放，不会滞留。</p></div></div>
<div class="row"><label for="off-start">开始 / 结束</label><div><input id="off-start" type="time"> <input id="off-end" type="time"><p class="note">含开始时刻，不含结束时刻。</p></div></div>
<div class="row"><label for="defer-mb">超过多大的文件推迟到时段内</label><div><input id="defer-mb" type="number" min="0" step="100" placeholder="0"> MB<p class="note">大于这个体积的素材只在时段内处理，其余随时处理。<b>0 表示不推迟任何文件</b>。用体积而不是时长做判断，因为磁盘感受到的是读了多少字节。被推迟的作业不会被租用，因此不会消耗重试次数。</p></div></div>
<div class="row"><label for="immediate-mb">小于多大的文件永不受限</label><div><input id="immediate-mb" type="number" min="0" step="10" placeholder="0"> MB<p class="note">这个体积以下的素材同时豁免推迟和限速。手机短片派生几乎不花代价，限它只会让素材库显得坏掉而省不下什么。必须小于上面那个阈值。</p></div></div>
</section>
<div class="actions"><button id="save" onclick="save()">保存</button><button class="ghost" onclick="load()">放弃修改</button></div>
<div id="status" class="status"></div>
</main><script>
const MB=1048576;
function adminToken(){const el=document.getElementById('admin-token');return el?el.value.trim():''}
function authHeaders(base){const headers=new Headers(base||{});const token=adminToken();if(token)headers.set('Authorization','Bearer '+token);return headers}
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
renderState(d)}
async function load(){try{const d=await fetch('/api/v1/pipeline/throttle',{headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await r.text());return r.json()});fill(d);document.getElementById('status').className='status'}catch(e){say('无法读取设置：'+e.message,false)}}
async function save(){const b=document.getElementById('save');b.disabled=true;
const body={read_rate:num('read-rate'),cooldown_seconds:Math.round(num('cooldown')),off_peak_enabled:document.getElementById('off-peak').checked,off_peak_start:val('off-start')||'01:00',off_peak_end:val('off-end')||'07:00',defer_above_bytes:Math.round(num('defer-mb')*MB),immediate_max_bytes:Math.round(num('immediate-mb')*MB)};
try{const d=await fetch('/api/v1/pipeline/throttle',{method:'PUT',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify(body)}).then(async r=>{if(!r.ok)throw Error(await r.text());return r.json()});fill(d);say('已保存，下一个作业开始生效。',true)}catch(e){say('保存失败：'+e.message,false)}finally{b.disabled=false}}
load();setInterval(renderStateRefresh,15000);
async function renderStateRefresh(){try{const d=await fetch('/api/v1/pipeline/throttle',{headers:authHeaders()}).then(r=>r.ok?r.json():null);if(d)renderState(d)}catch(e){}}
</script></body></html>`
