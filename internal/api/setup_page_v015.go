package api

const setupHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Timingdex · 启动配置</title>
<style>:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#101827;color:#edf3ff;font:15px system-ui,sans-serif}header{padding:15px 6vw;border-bottom:1px solid #263651;display:flex;gap:18px;align-items:center}a{color:#b8c8ff;text-decoration:none}.brand{font-weight:850;color:#fff;margin-right:auto}.wrap{max-width:920px;margin:auto;padding:46px 24px}h1{font-size:44px;letter-spacing:-.05em;margin:8px 0}.muted{color:#aab8d0;line-height:1.65}.panel{background:#172238;border:1px solid #2c3d5b;border-radius:15px;padding:20px;margin-bottom:16px}.panel-head{display:flex;justify-content:space-between;align-items:center;gap:12px;margin-bottom:8px}.panel-head h2{margin:0;font-size:18px}.pill{border-radius:99px;padding:3px 11px;font-size:12px;font-weight:800;background:#34445e;color:#c6d4f0;white-space:nowrap}.pill.ok{background:#164b39;color:#9cf0c2}.pill.warn{background:#5c4a1e;color:#ffd88a}.env-row{display:flex;justify-content:space-between;gap:12px;padding:9px 0;border-bottom:1px solid #2a3a54}.env-row:last-child{border:0}.env-row b.ok{color:#82e4ad}.env-row b.bad{color:#ff9cab}.env-row b.warn{color:#ffc783}.button{display:inline-block;margin:4px 0 0 8px;padding:10px 14px;border-radius:9px;background:#8da7ff;color:#081225;font-weight:850;text-decoration:none}.env-actions{margin-top:12px}button{background:#31446a;color:#dbe6ff;border:0;border-radius:9px;padding:9px 14px;font-weight:800;cursor:pointer}button:hover{background:#3e547e}.status{margin-top:12px;padding:12px;border-radius:9px;background:#0d1728}.ok{color:#82e4ad}.bad{color:#ff9cab}.note{margin:14px 0 0;font-size:13px}</style></head>
<body><!--SHELL_HEADER-->
<main class="wrap"><div class="muted">FIRST-RUN WIZARD</div><h1>启动配置</h1><p class="muted">第一次使用向导：先检查运行环境，再添加素材目录、配置模型能力，就可以开始扫描和处理素材。</p>
<section class="panel"><div class="panel-head"><h2>环境检查</h2><span class="pill" id="pill-env">…</span></div>
<div class="env-rows">
<div class="env-row"><span>FFmpeg</span><b id="env-ffmpeg">…</b></div>
<div class="env-row"><span>FFprobe</span><b id="env-ffprobe">…</b></div>
<div class="env-row"><span>ExifTool <em class="muted">（可选）</em></span><b id="env-exiftool">…</b></div>
<div class="env-row"><span>数据目录可写</span><b id="env-datadir">…</b></div>
<div class="env-row"><span>缓存目录可写</span><b id="env-cache">…</b></div>
<div class="env-row"><span>磁盘空间 ≥1GiB</span><b id="env-disk">…</b></div>
<div class="env-row"><span>数据库健康</span><b id="env-db">…</b></div>
</div>
<div class="env-actions"><button onclick="setupLoadStatus()">重新检查</button></div>
<div id="setup-status" class="status">正在检查…</div>
</section>
<section class="panel"><div class="panel-head"><h2>素材目录</h2><span class="pill" id="pill-roots">…</span></div>
<div id="roots-body" class="muted">检查中…</div>
<p class="muted note">Timingdex 从不写入原始素材。素材只读打开，派生文件放在缓存目录。</p>
</section>
<section class="panel"><div class="panel-head"><h2>模型能力</h2><span class="pill" id="pill-providers">…</span></div>
<div id="providers-body" class="muted">检查中…</div>
<p class="muted note">视频理解 / 语音识别 / 语义检索。模型 Key 在「模型通道」中一次录入，保存到受保护的 Hub 密钥库。</p>
</section>
<section class="panel"><div class="panel-head"><h2>状态</h2><span class="pill" id="pill-status">…</span></div>
<div id="status-body" class="muted">检查中…</div>
</section>
<p class="muted note">页面不会生成含 API Key 的终端命令，也不会把凭据写入浏览器存储。</p>
</main>
<script>
// Names here stay unique from the shared shell block (getAdminToken,
// shellAuthHeaders, statusCell, refreshStatus), so injecting the shell cannot
// clash with this page's script.
function setupPill(id,done){const el=document.getElementById(id);if(!el)return;el.className='pill '+(done?'ok':'warn');el.textContent=done?'已完成':'待处理'}
function setupLoadStatus(){
  const box=document.getElementById('setup-status');if(box){box.className='status';box.textContent='正在检查…'}
  fetch('/api/v1/setup/status').then(function(r){if(!r.ok)throw Error(String(r.status));return r.json()}).then(function(s){
    function row(id,ok,text){const el=document.getElementById(id);if(!el)return;el.className=ok?'ok':'bad';el.textContent=text}
    function opt(id,ok){const el=document.getElementById(id);if(!el)return;el.className=ok?'ok':'warn';el.textContent=ok?'✓ Ready':'⚠ Optional'}
    row('env-ffmpeg',!!s.ffmpeg,s.ffmpeg?'✓ Ready':'✗ Needs action');
    row('env-ffprobe',!!s.ffprobe,s.ffprobe?'✓ Ready':'✗ Needs action');
    opt('env-exiftool',!!s.exiftool);
    row('env-datadir',!!s.data_dir_writable,s.data_dir_writable?'✓ Ready':'✗ Needs action');
    row('env-cache',!!s.cache_writable,s.cache_writable?'✓ Ready':'✗ Needs action');
    const disk=document.getElementById('env-disk');if(disk){disk.className=s.free_disk_ok?'ok':'bad';disk.textContent=(s.free_disk_ok?'✓ Ready':'✗ Needs action')+' · '+((s.free_disk_bytes||0)/1073741824).toFixed(1)+' GiB'}
    row('env-db',!!s.db_healthy,s.db_healthy?'✓ Ready':'✗ Needs action');
    setupPill('pill-env',!!s.ffmpeg&&!!s.ffprobe&&!!s.data_dir_writable&&!!s.cache_writable&&!!s.free_disk_ok&&!!s.db_healthy);
    const roots=document.getElementById('roots-body');if(roots){roots.innerHTML=s.root_count>0?'✓ <b>'+(s.root_count||0)+'</b> 个素材目录（'+(s.asset_count||0)+' 个资产）<a href="/library-roots">管理素材目录</a>':'尚未添加素材目录。<a class="button" href="/library-roots">打开素材目录向导</a>'}setupPill('pill-roots',s.root_count>0);
    const prov=document.getElementById('providers-body');if(prov){prov.innerHTML=s.provider_count>0?'✓ 已配置 <b>'+(s.provider_count||0)+'</b> 个通道 <a href="/providers">管理模型通道</a>':'尚未配置模型通道。<a class="button" href="/providers">配置模型</a>'}setupPill('pill-providers',s.provider_count>0);
    const steps={'add_footage':['下一步：添加素材目录','添加素材目录','/library-roots'],'configure_providers':['下一步：配置模型','配置模型','/providers'],'scan_or_process':['下一步：扫描并处理素材','扫描素材','/library-roots'],'search':['可以去搜索素材了','去搜索','/'],'ready':['一切就绪','进入素材库','/']};
    const step=steps[s.next_step];const sb=document.getElementById('status-body');
    if(sb){sb.innerHTML=step?'<b>'+step[0]+'</b> <a class="button" href="'+step[2]+'">'+step[1]+'</a>':'状态接口返回了未知的下一步。'}
    setupPill('pill-status',!!s.ready);
    if(box){box.className='status ok';box.textContent='状态已更新'}
  }).catch(function(){
    if(box){box.className='status bad';box.textContent='无法连接状态接口，请确认 Hub 已运行，然后点击「重新检查」重试。'}
  })
}
setupLoadStatus();
</script>
</body></html>`
