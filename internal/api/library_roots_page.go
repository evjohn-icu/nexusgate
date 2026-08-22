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
<h2>素材目录状态</h2>
<p class="muted">每个已加素材目录的健康状态。目录离线时，缺文件的核对会暂停，不会把素材标成缺失。</p>
<div id="root-health-wrap"><div class="muted">正在读取…</div></div>
</div>

<div class="step-nav" id="step-nav"><div class="active" data-step="1">1. 输入路径</div><div data-step="2">2. 接入说明</div><div data-step="3">3. 验证接入</div><div data-step="4">4. 添加并扫描</div></div>

<div class="panel" id="discover-section">
<h2>发现内网共享</h2>
<p class="muted">扫描局域网里的 NAS / SMB 服务器，找到可以直接添加的共享。这里只做匿名探测，需要密码的共享会标出来，接入密码走下一步向导、不会经过浏览器。</p>
<div class="step-actions">
  <button class="primary" id="discover-btn" onclick="runDiscover()">扫描内网共享</button>
  <span class="hint" id="discover-status"></span>
</div>
<div id="discover-results"></div>
</div>

<div class="step active" id="step-1">
<div class="panel"><h2>本机目录，或者 NAS / SMB 共享</h2>
<p class="muted">直接输入本机能访问的目录，会立即添加为素材目录。如果输入的是网络共享地址（例如 <code>//nas/Video</code>、<code>smb://user@host/share</code>、<code>host:/export</code>），会先给出接入命令，不会猜密码，也不会替你执行接入。</p>
<div class="field"><label for="root-input">路径或共享地址</label><input id="root-input" type="text" placeholder="/Users/me/Footage 或 //nas.local/Video"></div>
<div id="step1-status" class="hint"></div>
<div class="step-actions"><button class="primary" onclick="startInspect()">下一步 →</button></div>
</div>
</div>

<div class="step" id="step-2">
<div class="panel"><h2>这是一个网络共享</h2>
<div id="share-summary"></div>
<p class="muted" id="guide-summary"></p>
<div class="field"><label for="mountpoint">接入点（可改，会重新生成下面的命令）</label><input id="mountpoint" type="text" onchange="regenerateGuidance()"></div>
<div id="guide-body"></div>
</div>
<div class="panel" id="compose-section" style="display:none"></div>
<div class="step-actions"><button class="nav-btn secondary" onclick="goStep(1)">← 上一步</button><button class="nav-btn primary" onclick="goVerify()">我已完成接入，验证 →</button></div>
</div>

<div class="step" id="step-3">
<div class="panel"><h2>验证接入</h2>
<p class="muted">让 Hub 确认接入点是否存在、是不是目录、是不是网络共享。</p>
<div id="verify-result"></div>
<div class="step-actions"><button class="nav-btn secondary" onclick="goStep(2)">← 返回接入说明</button><button class="nav-btn secondary" onclick="verifyMount()">重新检查</button><button class="nav-btn primary" id="verify-add-btn" style="display:none" onclick="addFromVerify()">添加为素材目录 →</button></div>
</div>
</div>

<div class="step" id="step-4">
<div class="panel"><h2>已添加素材目录</h2>
<div id="added-summary"></div>
<div class="center"><button class="primary" id="scan-btn" style="display:none" onclick="startScan()">开始扫描</button></div>
<div id="scan-result"></div>
<p class="muted">扫描会在后台把发现的素材加进处理队列；进度在「<a href="/progress">处理进度</a>」看。</p>
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
  'create-mountpoint':'创建接入点',
  'smb-credentials-file':'把凭据写入仅 root 可读的文件',
  'smb-mount':'以只读方式接入共享',
  'nfs-mount':'以只读方式接入 NFS 导出目录',
  'fstab':'写入 /etc/fstab，让挂载在重启后依然生效',
  'add-root':'把接入点添加为素材目录',
  'add-root-in-container':'回到 Hub 容器里执行 —— 宿主机上的这个目录在容器内是另一个路径，要记录的是容器内的那个',
  'add-root-not-visible':'这个挂载点不在绑定进本容器的目录之下，Hub 永远看不到它。请改挂到绑定目录之下再重跑本向导，或者用能覆盖它的绑定重建容器。',
  'windows-map':'接入共享',
  'darwin-mount':'接入共享（密码会在终端提示里输入，不会出现在命令里）',
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
  return path+' 是网络共享，不是本机路径。请先接入它，再把接入点添加为素材目录。';
}
 function csrfToken(){var prefix='__Host-timingdex_csrf=';var item=document.cookie.split('; ').find(function(x){return x.indexOf(prefix)===0});return item?decodeURIComponent(item.slice(prefix.length)):''}
 function authHeaders(base){var headers=new Headers(base||{});var csrf=csrfToken();if(csrf)headers.set('X-CSRF-Token',csrf);return headers}
async function apiErrMsg(r){try{const d=await r.json();if(d&&d.error&&d.error.message)return d.error.action?(d.error.message+'（'+d.error.action+'）'):d.error.message}catch(_){}return (await r.text()).trim()}
async function json(url,opt){opt=opt||{};var r=await fetch(url,{...opt,headers:authHeaders(opt.headers)});if(!r.ok)throw new Error(await apiErrMsg(r));return r.json()}
function goStep(n){for(var i=1;i<=4;i++){document.getElementById('step-'+i).className='step'+(i===n?' active':'');document.querySelectorAll('.step-nav div')[i-1].className=(i===n?'active':'')}}

function healthTime(v){if(!v)return'—';var t=new Date(v);return isNaN(t.getTime())?esc(v):t.toLocaleString()}
async function loadRootHealth(){
  var wrap=document.getElementById('root-health-wrap');
  if(!wrap)return;
  var list;
  try{list=await json('/api/v1/roots/health')}catch(e){wrap.innerHTML='<div class="muted">需要 Hub 管理口令才能查看目录状态。</div>';return}
  if(!list||!list.length){wrap.innerHTML='<div class="muted">暂无素材目录。添加后这里会显示健康状态。</div>';return}
  var warns=[];
  var rows=list.map(function(h){
    var pill='muted',label='未知';
    if(h.state==='healthy'){pill='ok';label='正常'}
    else if(h.state==='unavailable'){pill='err';label='离线'}
    if(h.state==='unavailable'){
      // The reconciliation gate pauses on an unavailable root (the scan
      // service's verdict, not this page's) so assets are never marked
      // missing while the root itself is the thing that is gone.
      warns.push('<div class="health-warn">⚠ 目录离线 — 暂停缺失文件核对（不会把素材标记为缺失）。<br>上次正常：'+esc(healthTime(h.last_healthy_at))+'</div>');
    }
    // warnings is the same advice Doctor prints for a root (network mount,
    // staging-copy recommendation, writable mount) — see Service.RootWarnings.
    var tips=(h.warnings||[]).map(function(w){return '<div class="root-warning">⚠ '+esc(w)+'</div>'}).join('');
    return '<tr><td class="health-path">'+esc(h.path)+'</td><td><span class="health-pill '+pill+'">'+label+'</span></td><td>'+esc(healthTime(h.last_healthy_at))+'</td><td>'+esc(healthTime(h.last_scan_at))+'</td><td class="health-tips">'+tips+'</td></tr>';
  }).join('');
  wrap.innerHTML='<table class="health-table"><tr><th>路径</th><th>状态</th><th>上次正常</th><th>上次扫描</th><th>提示</th></tr>'+rows+'</table>'+warns.join('');
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
  btn.disabled=true;status.textContent='正在扫描内网…（约 15 秒）';
  wrap.innerHTML='';
  try{
    var data=await json('/api/v1/roots/discover',{method:'POST',headers:authHeaders({'Content-Type':'application/json'})});
    var hosts=(data&&data.hosts)||[];
    if(!hosts.length){
      wrap.innerHTML='<div class="hint">没找到内网 SMB 服务器。可能是本机没有局域网接口、网络禁用了组播，或没有设备开放 445 端口。可以手动输入下方路径。</div>';
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
        detail='<div class="hint">需要凭据（接入密码走下一步向导）</div>';
      }else{
        detail='<div class="hint">未发现可读共享</div>';
      }
      return '<div class="panel discover-host"><div class="discover-host-name">'+name+' <span class="muted">'+esc(h.ip)+'</span></div>'+detail+'</div>';
    }).join('');
    wrap.innerHTML=rows;
    status.textContent='发现 '+hosts.length+' 台主机。';
  }catch(e){
    status.className='hint bad';status.textContent='扫描失败：'+esc(e.message);
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
    var shareErr=new Error((body.error&&body.error.message)||(typeof body.error==='string'?body.error:'')||'这个共享还没接入');
    shareErr.shareNotMounted=true;
    shareErr.inspection=body.inspection;
    throw shareErr;
  }
  if(!response.ok){throw new Error((body&&body.error&&body.error.message)||(body&&typeof body.error==='string'?body.error:'')||response.statusText||('添加素材目录失败（'+response.status+'）'))}
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
  var html='<h2>另一种方式：Docker Compose 挂载卷</h2><p class="muted">这个 Hub 运行在容器里，容器本身无法像宿主机那样执行上面的挂载命令。把下面这段配置粘贴进 docker-compose.yml 的 volumes: 下，下面第二段把它挂进 hub 和 worker 两个服务——Worker 也要，它租到派生作业后会自己打开源文件，只挂进 Hub 会让每个下发的作业都以文件不存在告终。</p>';
  if(protocol==='smb'){
    html+='<p class="muted">推荐优先使用 NFS：如果这台 NAS 支持导出 NFS 共享，请改用 NFS 地址（例如 host:/export）重新执行本向导——NFS 形式的挂载卷不需要任何凭据，可以彻底避免下面这个问题。</p>';
  }else if(protocol==='nfs'){
    html+='<p class="muted">推荐路线：NFS 形式的挂载卷不需要任何凭据，是这两种形式里更安全的一种。</p>';
  }
  if(volume.warning){
    html+='<div class="callout callout--contradicted"><b>警告：</b>'+esc(tr(volume.warning_key,volume.warning))+'</div>';
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
  if(!guidance||!guidance.steps){container.innerHTML='<div class="muted">没有可用的接入说明。</div>';return}
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
  if(!mp){el.innerHTML='<div class="hint bad">接入点是空的。</div>';return}
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
      lines.push('<div class="hint bad">'+esc(checked)+' 尚不存在。请先完成上一步的接入命令，再点击「重新检查」。</div>');
    }else{
      lines.push('<div class="hint bad">'+esc(checked)+' 存在，但不是目录。</div>');
    }
    if(inspection.looks_unmounted){lines.push('<div class="hint bad">这个目录是空的，而且它本身不是接入点——共享可能没真正接上。</div>')}
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
      el.innerHTML+='<div class="hint bad">添加失败：共享未接入。'+esc(e.message)+'</div>';
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
    var pipeline=result.pipeline_status==='started'?'已自动开始处理':(result.pipeline_status==='already_running'?'已有处理任务正在运行':'已完成扫描');
    el.innerHTML='<div class="hint ok">发现 '+esc(result.discovered)+' · 关联 '+esc(result.linked)+' · 缺失 '+esc(result.missing)+' · '+pipeline+'</div>'+(errors.length?'<div class="hint">扫描有 '+errors.length+' 个警告：'+errors.map(esc).join('<br>')+'</div>':'')+'<div class="muted" style="margin-top:8px"><a href="/progress">查看处理进度 →</a></div>';
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
