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
func (s *Server) libraryRootsPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(brandedPage(shelledPage(libraryRootsHTML))))
}

const libraryRootsHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · 添加素材目录</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#101827;color:#edf3ff;font:15px ui-sans-serif,system-ui,-apple-system,sans-serif}header{padding:15px 5vw;border-bottom:1px solid #293953;display:flex;gap:18px;align-items:center;background:#101827ee;position:sticky;top:0;z-index:2;backdrop-filter:blur(12px)}a{color:#b8c8ff;text-decoration:none}.brand{color:#fff;font-weight:800;margin-right:auto}.wrap{max-width:960px;margin:auto;padding:30px 24px 80px}.step-nav{display:flex;gap:0;margin-bottom:28px;border-radius:10px;overflow:hidden;border:1px solid #2c3d5b}.step-nav div{flex:1;text-align:center;padding:12px;font-size:13px;font-weight:700;background:#172238;color:#6c7fa2;border-right:1px solid #2c3d5b;transition:background .2s,color .2s}.step-nav div:last-child{border-right:0}.step-nav .active{background:#2b4070;color:#eef4ff}.step{display:none}.step.active{display:block}.panel{background:#172238;border:1px solid #2c3d5b;border-radius:14px;padding:20px;margin-bottom:16px}.panel h2{margin:0 0 14px;font-size:17px}.field{margin:12px 0}.field label{font-size:12px;color:#bfcae0;font-weight:800;display:block;margin:0 0 5px}input,select,textarea{width:100%;font:inherit;padding:10px;border-radius:8px;background:#0e1728;color:#fff;border:1px solid #374965}button{border:0;border-radius:8px;padding:10px 16px;font:inherit;font-weight:800;cursor:pointer}.primary{background:#8ca7ff;color:#0d1830}.secondary{background:#2e405e;color:#eaf1ff}.success{background:#3a9668;color:#d3fce4}.muted{color:#aab8d0;font-size:13px;line-height:1.6}.hint{font-size:13px;margin:8px 0;color:#ffc98d}.hint.ok{color:#82e4ad}.hint.bad{color:#ffb7ac}.step-actions{display:flex;gap:10px;margin-top:16px}.nav-btn{background:#2b4070;color:#eef4ff}.share-line{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px;color:#d9e5ff;word-break:break-all;background:#0e1728;border-radius:8px;padding:8px;margin:6px 0}.guide-step{margin:16px 0}.guide-step-title{font-weight:800;font-size:13px;color:#dce6fb;margin-bottom:7px}.cmd{display:flex;gap:8px;align-items:flex-start;background:#0e1728;border:1px solid #374965;border-radius:8px;padding:9px 10px;margin:6px 0}.cmd code{flex:1;font:12px/1.55 ui-monospace,SFMono-Regular,Menlo,monospace;color:#d9e5ff;word-break:break-all;white-space:pre-wrap}.copy-btn{background:#4a618a;color:#fff;border:0;border-radius:7px;padding:6px 10px;font-size:12px;cursor:pointer;font-weight:700;white-space:nowrap}.notes{margin-top:14px;padding:12px 14px;background:#0e1728;border-radius:8px;color:#c7d3ea;font-size:12px;line-height:1.6}.notes ul{margin:6px 0 0;padding-left:18px}.center{text-align:center;margin:18px 0}.danger{background:#3f2230;color:#ffc2cc;border:1px solid #6d3547;border-radius:10px;padding:12px 14px;margin:10px 0 14px;font-size:13px;line-height:1.6}.danger b{color:#ffe1e6}@media(max-width:600px){.cmd{flex-direction:column}}
</style></head><body data-library-roots-wizard>
<!--SHELL_HEADER-->
<div class="wrap">
<div class="step-nav" id="step-nav"><div class="active" data-step="1">1. 输入路径或共享</div><div data-step="2">2. 挂载说明</div><div data-step="3">3. 验证挂载</div><div data-step="4">4. 添加并扫描</div></div>

<div class="step active" id="step-1">
<div class="panel"><h2>本机目录，或者 NAS / SMB 共享</h2>
<p class="muted">直接输入本机已经能访问的目录，会立即添加为素材目录。如果输入的是网络共享地址（例如 <code>//nas/Video</code>、<code>smb://user@host/share</code>、<code>host:/export</code>），会先给出挂载命令，不会猜测密码，也不会替你执行挂载。</p>
<div class="field"><label for="root-input">路径或共享地址</label><input id="root-input" type="text" placeholder="/Users/me/Footage 或 //nas.local/Video"></div>
<div id="step1-status" class="hint"></div>
<div class="step-actions"><button class="primary" onclick="startInspect()">下一步 →</button></div>
</div>
</div>

<div class="step" id="step-2">
<div class="panel"><h2>这是一个网络共享</h2>
<div id="share-summary"></div>
<p class="muted" id="guide-summary"></p>
<div class="field"><label for="mountpoint">挂载点（可修改，会重新生成下面的命令）</label><input id="mountpoint" type="text" onchange="regenerateGuidance()"></div>
<div id="guide-body"></div>
</div>
<div class="panel" id="compose-section" style="display:none"></div>
<div class="step-actions"><button class="nav-btn secondary" onclick="goStep(1)">← 上一步</button><button class="nav-btn primary" onclick="goVerify()">我已完成挂载，验证 →</button></div>
</div>

<div class="step" id="step-3">
<div class="panel"><h2>验证挂载</h2>
<p class="muted">向 Hub 确认挂载点现在是否存在、是不是目录，以及它是不是一个网络挂载。</p>
<div id="verify-result"></div>
<div class="step-actions"><button class="nav-btn secondary" onclick="goStep(2)">← 返回挂载说明</button><button class="nav-btn secondary" onclick="verifyMount()">重新检查</button><button class="nav-btn primary" id="verify-add-btn" style="display:none" onclick="addFromVerify()">添加为素材目录 →</button></div>
</div>
</div>

<div class="step" id="step-4">
<div class="panel"><h2>已添加素材目录</h2>
<div id="added-summary"></div>
<div class="center"><button class="primary" id="scan-btn" style="display:none" onclick="startScan()">开始扫描</button></div>
<div id="scan-result"></div>
<p class="muted">扫描会在后台把发现的素材加入处理队列；进度可以在「<a href="/progress">处理进度</a>」查看。</p>
</div>
</div>

</div>
<script>
const esc=function(v){return String(v??'').replace(/[&<>"']/g,function(c){return {'&':'&amp;','>':'&gt;','<':'&lt;','"':'&quot;',"'":'&#39;'}[c]})};

// internal/mount generates step titles and notes in English on purpose --
// it is also what the English CLI (timingdex doctor) renders -- and gives
// each one a stable Key (mount.Step.Key, mount.Note.Key) precisely so a
// consumer that needs another language can translate by Key instead of by
// matching English text. This table is that translation, kept here because
// this is where every other piece of this project's Chinese UI copy lives.
// A Key missing from this table is not a bug in mount, it is a translation
// that has not been added yet; tr() below falls back to the English text
// rather than rendering nothing, because a wizard with one English line is
// still usable and a blank step is not. TestLibraryRootsPageTranslatesEveryGuidanceKey
// in library_roots_page_test.go asserts every Key mount.Guidance can
// currently produce has an entry here, so a Key added to mount without a
// matching entry here fails the build instead of shipping silently in
// English.
const MOUNT_TR={
  'create-mountpoint':'创建挂载点',
  'smb-credentials-file':'把凭据写入仅 root 可读的文件',
  'smb-mount':'以只读方式挂载共享',
  'nfs-mount':'以只读方式挂载 NFS 导出目录',
  'fstab':'写入 /etc/fstab，让挂载在重启后依然生效',
  'add-root':'把挂载点添加为素材目录',
  'add-root-in-container':'回到 Hub 容器里执行 —— 宿主机上的这个目录在容器内是另一个路径，要记录的是容器内的那个',
  'add-root-not-visible':'这个挂载点不在绑定进本容器的目录之下，Hub 永远看不到它。请改挂到绑定目录之下再重跑本向导，或者用能覆盖它的绑定重建容器。',
  'windows-map':'映射共享',
  'darwin-mount':'挂载共享（密码会在终端提示中输入，不会出现在命令里）',
  'container-run-on-host':'请在运行 Docker 的宿主机上执行以下命令——不是在这个容器里面',
  'read-only':'挂载被特意设为只读。素材本身从不会被写入；只读挂载能让这一点对整个共享成立，而不只是对本程序成立。',
  'staging-copy':'为网络素材目录在 config.json 中设置 "source_staging": {"mode": "copy"}。如果每次生成衍生文件都要让 FFmpeg 通过 SMB 读取 4K 源文件，NAS 素材库会慢到无法忍受；copy 模式会先把源文件缓存到 cache/sources/，之后不再重复经过网络读取。',
  'worker-same-path':'节点（Worker）需要在同样的路径下看到同样的素材；如果做不到，配对时要显式指定 --mount root-id=它自己的路径。',
  'container-cannot-mount':'这个容器无法自己挂载共享，这是刻意为之：它以固定的非特权用户运行，没有 CAP_SYS_ADMIN；拥有这个权限，或者把 docker.sock 挂进容器，都会让它拿到本应被拒绝的宿主机挂载权限——其中 docker.sock 更糟，因为那等于拿到了 Docker 宿主机上不受限制的 root 权限。这就是为什么上面的步骤要在 Docker 宿主机上执行，而不是在容器里。如果共享是在容器启动之后才挂载到宿主机上的，容器内默认看不到它，除非 docker-compose.yml 里素材目录的 bind 挂载设置了 "bind.propagation: rslave"；如果没有设置，需要在宿主机完成挂载后重建容器（docker compose up -d）。',
  'wsl-namespace':'在 WSL2 下，交互式终端里挂载的共享，对 systemd 服务和其他 SSH 会话是不可见的，因为它们处于不同的挂载命名空间。上面命令中的 nsenter 前缀会把挂载操作放进 PID 1 的命名空间执行，这样所有进程都能看到它。',
  'wsl-not-persistent':'WSL2 在发行版重启后不会保留挂载。需要重新执行挂载命令，或者把它加入 /etc/wsl.conf 的启动命令。',
  'windows-service-drive-letter':'在自己的会话中映射的盘符，对以其他账户运行的服务并不存在。如果 Hub 是以服务方式运行的，请改用 UNC 路径 \\\\host\\share\\folder，而不是盘符。',
  'compose-smb-cleartext':'Docker 内置的 local 卷驱动会把 driver_opts 直接传给 mount(2) 系统调用，从不经过 mount.cifs 这个辅助程序，所以本指南其他地方使用的 credentials=/path/to/file 凭据文件写法在这里会被拒绝，报错 invalid argument（docker/cli#2802，自 2020-10-20 起一直未修复）。密码只能写在 o: 参数里，这意味着它会以明文形式保留在 docker volume inspect 的输出中，以及 /var/lib/docker/volumes/卷名/opts.json 文件里。这个项目里的其他密钥都保存在加密存储中，永远不会进入 SQLite、API 响应或日志——这种写法只是 NAS 无法导出 NFS 时的记录在案的备选方案，不是默认推荐。另外要注意：最容易泄漏的地方其实是你自己的 docker-compose.yml —— 那是会被提交进 git 的文件。把密码换成 ${NAS_PASSWORD} 并放进未跟踪的 .env 可以让它不进版本库，但救不了 opts.json，因为 Docker 存的是解析后的值。'
};
function tr(key,fallback){var v=MOUNT_TR[key];return v!==undefined?v:fallback}

// Guidance.Summary is always the same English template ("%s is a network
// share, not a local path...") with only the share address substituted --
// see mount.Guidance -- so there is exactly one sentence to translate, and
// it is derived from inspection.path (already the parsed share address,
// see RootInspection.Path in internal/app/service.go) rather than given a
// Key of its own the way steps and notes are.
function translatedSummary(inspection){
  var path=(inspection&&inspection.path)||'';
  return path+' 是一个网络共享，不是本机路径。请先挂载它，再把挂载点添加为素材目录。';
}
function adminToken(){var el=document.getElementById('admin-token');return el?el.value.trim():''}
function authHeaders(base){var headers=new Headers(base||{});var token=adminToken();if(token)headers.set('Authorization','Bearer '+token);return headers}
async function json(url,opt){opt=opt||{};var r=await fetch(url,{...opt,headers:authHeaders(opt.headers)});if(!r.ok){var msg='';try{msg=await r.text()}catch(e){msg=r.statusText}throw new Error(msg)}return r.json()}
function goStep(n){for(var i=1;i<=4;i++){document.getElementById('step-'+i).className='step'+(i===n?' active':'');document.querySelectorAll('.step-nav div')[i-1].className=(i===n?'active':'')}}

var lastInput='';
var lastVerifyPath='';
var addedRoot=null;

async function addRoot(path){
  var response=await fetch('/api/v1/roots',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({path:path})});
  var body=null;
  try{body=await response.json()}catch(e){}
  if(response.status===422&&body&&body.inspection){
    var shareErr=new Error(body.error||'该共享尚未挂载');
    shareErr.shareNotMounted=true;
    shareErr.inspection=body.inspection;
    throw shareErr;
  }
  if(!response.ok){throw new Error((body&&body.error)||response.statusText||('添加素材目录失败（'+response.status+'）'))}
  return body;
}

async function startInspect(){
  var input=document.getElementById('root-input').value.trim();
  var status=document.getElementById('step1-status');
  if(!input){status.className='hint bad';status.textContent='请输入路径或共享地址';return}
  status.className='hint';status.textContent='正在检查…';
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
    status.className='hint bad';status.textContent='检查失败：'+e.message;
  }
}

function renderGuidance(inspection){
  var share=inspection.share||{};
  document.getElementById('share-summary').innerHTML='<div class="share-line">'+esc((share.protocol||'').toUpperCase())+' · '+esc(share.host||'')+' / '+esc(share.name||'')+(share.user?' · 用户 '+esc(share.user):'')+'</div>';
  document.getElementById('guide-summary').textContent=translatedSummary(inspection);
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
  var html='<h2>另一种方式：Docker Compose 挂载卷</h2><p class="muted">这个 Hub 运行在容器里，容器本身无法像宿主机那样执行上面的挂载命令。把下面这段配置粘贴进 docker-compose.yml 的 volumes: 下，下面第二段把它挂进 hub 和 worker 两个服务——Worker 也要，它租到 derive 作业后会自己打开源文件，只挂进 Hub 会让每个下发的作业都以文件不存在告终。</p>';
  if(protocol==='smb'){
    html+='<p class="muted">推荐优先使用 NFS：如果这台 NAS 支持导出 NFS 共享，请改用 NFS 地址（例如 host:/export）重新执行本向导——NFS 形式的挂载卷不需要任何凭据，可以彻底避免下面这个问题。</p>';
  }else if(protocol==='nfs'){
    html+='<p class="muted">推荐路线：NFS 形式的挂载卷不需要任何凭据，是这两种形式里更安全的一种。</p>';
  }
  if(volume.warning){
    html+='<div class="danger"><b>警告：</b>'+esc(tr(volume.warning_key,volume.warning))+'</div>';
  }
  html+='<div class="guide-step"><div class="guide-step-title">1. 粘贴到 docker-compose.yml 顶层的 volumes: 下</div><div class="cmd"><code id="compose-yaml-code">'+esc(volume.yaml)+'</code><button type="button" class="copy-btn" onclick="copyCmd(\'compose-yaml-code\')">复制</button></div></div>';
  if(volume.service_yaml){
    html+='<div class="guide-step"><div class="guide-step-title">2. 合并进 services: —— 已有 volumes: 的服务追加那一行即可</div><div class="cmd"><code id="compose-service-code">'+esc(volume.service_yaml)+'</code><button type="button" class="copy-btn" onclick="copyCmd(\'compose-service-code\')">复制</button></div></div>';
  }
  if(volume.mount_path){
    html+='<div class="guide-step"><div class="guide-step-title">3. docker compose up -d 之后，要记录的素材目录是容器内的这个路径</div><div class="cmd"><code id="compose-root-code">timingdex root add '+esc(volume.mount_path)+'</code><button type="button" class="copy-btn" onclick="copyCmd(\'compose-root-code\')">复制</button></div>';
    html+='<p class="muted">走这条路时，上面第 3 步的验证请填 '+esc(volume.mount_path)+'——它已经是容器内的路径，不需要再做翻译。</p></div>';
  }
  container.innerHTML=html;
}

function renderGuideBody(guidance){
  var container=document.getElementById('guide-body');
  if(!guidance||!guidance.steps){container.innerHTML='<div class="muted">没有可用的挂载说明。</div>';return}
  var html='';
  guidance.steps.forEach(function(step,i){
    html+='<div class="guide-step"><div class="guide-step-title">'+(i+1)+'. '+esc(tr(step.key,step.title))+'</div>';
    (step.commands||[]).forEach(function(cmd,j){
      var id='cmd-'+i+'-'+j;
      html+='<div class="cmd"><code id="'+id+'">'+esc(cmd)+'</code><button type="button" class="copy-btn" onclick="copyCmd(\''+id+'\')">复制</button></div>';
    });
    html+='</div>';
  });
  if(guidance.notes&&guidance.notes.length){
    html+='<div class="notes"><b>注意事项</b><ul>'+guidance.notes.map(function(n){return '<li>'+esc(tr(n.key,n.text))+'</li>'}).join('')+'</ul></div>';
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
    container.innerHTML='<div class="hint bad">重新生成命令失败：'+esc(e.message)+'</div>';
  }
}

function goVerify(){goStep(3);verifyMount()}

async function verifyMount(){
  var mp=document.getElementById('mountpoint').value.trim();
  var el=document.getElementById('verify-result');
  document.getElementById('verify-add-btn').style.display='none';
  if(!mp){el.innerHTML='<div class="hint bad">挂载点为空。</div>';return}
  el.innerHTML='<div class="muted">正在检查…</div>';
  try{
    var inspection=await json('/api/v1/roots/inspect',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:JSON.stringify({path:mp})});
    lastVerifyPath=inspection.container_path||mp;
    var lines=[];
    // A containerised Hub cannot stat the host-side path the operator was
    // told to mount, so the server checked the translated one. Name both,
    // rather than reporting a verdict about a path the operator never typed.
    var checked=inspection.container_path||mp;
    if(inspection.container_path){lines.push('<div class="muted">宿主机上的 '+esc(mp)+' 在本容器内是 '+esc(inspection.container_path)+'，以下检查针对的是后者。</div>')}
    if(inspection.exists&&inspection.is_dir){
      lines.push('<div class="hint ok">'+esc(checked)+' 存在且是目录。</div>');
      if(inspection.network){lines.push('<div class="muted">文件系统：'+esc(inspection.network_label||inspection.filesystem_type)+'（'+esc(inspection.filesystem_type)+'）</div>')}
      document.getElementById('verify-add-btn').style.display='';
    }else if(!inspection.exists){
      lines.push('<div class="hint bad">'+esc(checked)+' 尚不存在。请先完成上一步的挂载命令，再点击「重新检查」。</div>');
    }else{
      lines.push('<div class="hint bad">'+esc(checked)+' 存在，但不是目录。</div>');
    }
    if(inspection.looks_unmounted){lines.push('<div class="hint bad">这个目录是空的，并且它本身不是挂载点——共享可能没有真正挂载上。</div>')}
    (inspection.warnings||[]).forEach(function(w){lines.push('<div class="hint">'+esc(w)+'</div>')});
    el.innerHTML=lines.join('');
  }catch(e){
    el.innerHTML='<div class="hint bad">检查失败：'+esc(e.message)+'</div>';
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
      el.innerHTML+='<div class="hint bad">添加失败：共享未挂载。'+esc(e.message)+'</div>';
      renderGuidance(e.inspection);
      goStep(2);
      return;
    }
    el.innerHTML+='<div class="hint bad">添加失败：'+esc(e.message)+'</div>';
  }
}

function renderAdded(root){
  document.getElementById('added-summary').innerHTML='<div class="hint ok">已添加素材目录：'+esc(root.path)+'</div><div class="muted">ID：'+esc(root.id)+'</div>';
  document.getElementById('scan-result').innerHTML='';
  document.getElementById('scan-btn').style.display='';
}

async function startScan(){
  if(!addedRoot)return;
  var btn=document.getElementById('scan-btn');
  btn.disabled=true;btn.textContent='正在扫描…';
  var el=document.getElementById('scan-result');
  try{
    var result=await json('/api/v1/roots/'+encodeURIComponent(addedRoot.id)+'/scan',{method:'POST'});
    var errors=(result.errors||[]);
    el.innerHTML='<div class="hint ok">发现 '+esc(result.discovered)+' · 关联 '+esc(result.linked)+' · 缺失 '+esc(result.missing)+'</div>'+(errors.length?'<div class="hint bad">'+errors.map(esc).join('<br>')+'</div>':'');
  }catch(e){
    el.innerHTML='<div class="hint bad">扫描失败：'+esc(e.message)+'</div>';
  }finally{
    btn.disabled=false;btn.textContent='开始扫描';
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
