package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/hubtls"
)

type workerMount struct {
	RootID string `json:"root_id"`
	Path   string `json:"path"`
}

var workerPlatforms = map[string]struct {
	Filename string
	Shell    string
}{
	"windows-amd64": {"timingdex-windows-amd64.exe", "powershell"},
	"linux-amd64":   {"timingdex-linux-amd64", "posix"},
	"linux-arm64":   {"timingdex-linux-arm64", "posix"},
}

func (s *Server) registerWorkerSetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /worker-setup", s.workerSetupPage)
	mux.HandleFunc("GET /api/v1/hub/worker-setup/context", s.requireTrustedRead(s.workerSetupContext))
	mux.HandleFunc("GET /api/v1/hub/worker-binaries/{platform}", s.requireTrustedRead(s.workerServeBinary))
	mux.HandleFunc("POST /api/v1/hub/worker-setup/script", s.requireHubAdmin(s.workerGenerateScript))
}

func (s *Server) workerSetupPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(brandedPage(shelledPage(workerSetupPageHTML))))
}

func (s *Server) workerSetupContext(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	hubURL := scheme + "://" + r.Host

	var fingerprint string
	tlsActive := false
	if s.tlsCert != "" {
		tlsActive = true
		if fp, err := hubtls.FingerprintCertificate(s.tlsCert); err == nil {
			fingerprint = fp
		}
	}

	roots, err := s.service.ListLibraryRoots(r.Context())
	if err != nil {
		roots = []domain.LibraryRoot{}
	}

	binariesDir := filepath.Join(s.service.DataDir(), "worker-binaries")
	availableBinaries := make(map[string]map[string]any, len(workerPlatforms))
	for key, info := range workerPlatforms {
		binPath := filepath.Join(binariesDir, info.Filename)
		fi, statErr := os.Stat(binPath)
		entry := map[string]any{"exists": false, "size_bytes": 0}
		if statErr == nil && !fi.IsDir() {
			entry["exists"] = true
			entry["size_bytes"] = fi.Size()
		}
		availableBinaries[key] = entry
	}

	type rootEntry struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	rootList := make([]rootEntry, 0, len(roots))
	for _, root := range roots {
		rootList = append(rootList, rootEntry{ID: root.ID, Path: root.Path})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"hub_url":            hubURL,
		"fingerprint":        fingerprint,
		"tls":                tlsActive,
		"library_roots":      rootList,
		"available_binaries": availableBinaries,
	})
}

func (s *Server) workerServeBinary(w http.ResponseWriter, r *http.Request) {
	platform := r.PathValue("platform")
	info, ok := workerPlatforms[platform]
	if !ok {
		http.Error(w, "unknown platform: "+platform, http.StatusNotFound)
		return
	}
	binPath := filepath.Join(s.service.DataDir(), "worker-binaries", info.Filename)
	if _, err := os.Stat(binPath); err != nil {
		http.Error(w, "binary not available for platform: "+platform, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, binPath)
}

func (s *Server) workerGenerateScript(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Platform     string        `json:"platform"`
		Name         string        `json:"name"`
		PairingToken string        `json:"pairing_token"`
		Mounts       []workerMount `json:"mounts"`
		CacheDir     string        `json:"cache_dir"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	info, ok := workerPlatforms[req.Platform]
	if !ok {
		http.Error(w, "unknown platform: "+req.Platform, http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.PairingToken) == "" {
		http.Error(w, "pairing_token is required", http.StatusBadRequest)
		return
	}

	hubURL := "http"
	if r.TLS != nil {
		hubURL = "https"
	}
	hubURL += "://" + r.Host

	// The Worker CLI has separate --config and --cache flags. Deriving the
	// config path from cache_dir also pushed a request value through
	// filepath.Join, which rewrites separators using the Hub's OS and so
	// mangles a Windows path whenever the Hub is not Windows.
	const configPath = "./timingdex-worker.json"

	switch info.Shell {
	case "powershell":
		s.writeWorkerSetupScript(w, s.generatePowerShell(hubURL, info.Filename, req.Platform, req.PairingToken, req.Name, req.Mounts, configPath, req.CacheDir))
	default:
		s.writeWorkerSetupScript(w, s.generatePOSIX(hubURL, info.Filename, req.Platform, req.PairingToken, req.Name, req.Mounts, configPath, req.CacheDir))
	}
}

func (s *Server) generatePOSIX(hubURL, filename, platform, pairingToken, name string, mounts []workerMount, configPath, cacheDir string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n")
	b.WriteString("\n")
	b.WriteString("# WARNING: This script contains a single-use credential (one-time pairing token).\n")
	b.WriteString("# Do not commit or share this file.\n")
	b.WriteString("\n")
	b.WriteString("BINARY_URL=")
	b.WriteString(quotePOSIX(hubURL + "/api/v1/hub/worker-binaries/" + platform))
	b.WriteString("\n")
	b.WriteString("BINARY_OUT=")
	b.WriteString(quotePOSIX(filename))
	b.WriteString("\n")
	b.WriteString("\n")
	b.WriteString("echo 'Downloading Timingdex Worker binary...'\n")
	b.WriteString("curl -fsSL \"$BINARY_URL\" -o \"$BINARY_OUT\"\n")
	b.WriteString("chmod +x \"$BINARY_OUT\"\n")
	b.WriteString("\n")
	b.WriteString("echo 'Enrolling Worker...'\n")

	enrollArgs := []string{
		"./" + filename, "worker", "enroll",
		"--hub", hubURL,
		"--pairing", pairingToken,
	}
	if strings.TrimSpace(name) != "" {
		enrollArgs = append(enrollArgs, "--name", strings.TrimSpace(name))
	}
	for _, m := range mounts {
		enrollArgs = append(enrollArgs, "--mount", m.RootID+"="+m.Path)
	}
	if strings.TrimSpace(cacheDir) != "" {
		enrollArgs = append(enrollArgs, "--cache", strings.TrimSpace(cacheDir))
	}
	enrollArgs = append(enrollArgs, "--config", configPath)

	// Each argument is quoted separately. Quoting the joined string instead
	// produces one enormous quoted word, which the shell reads as the name of a
	// command to execute rather than as a command plus arguments -- safe, but the
	// script cannot run at all.
	quoted := make([]string, 0, len(enrollArgs))
	for _, arg := range enrollArgs {
		quoted = append(quoted, quotePOSIX(arg))
	}
	b.WriteString(strings.Join(quoted, " "))
	b.WriteString("\n")
	b.WriteString("\n")
	b.WriteString("echo ''\n")
	b.WriteString("echo 'Enrolled successfully. Run the Worker with:'\n")
	// Single quotes, not double: inside double quotes $(...) and backticks still
	// execute, so an interpolated request value would run as a command on the
	// operator's machine even though it is only being printed.
	b.WriteString("echo ")
	b.WriteString(quotePOSIX("  ./" + filename + " worker run --config " + configPath))
	b.WriteString("\n")

	return b.String()
}

func (s *Server) generatePowerShell(hubURL, filename, platform, pairingToken, name string, mounts []workerMount, configPath, cacheDir string) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	b.WriteString("\n")
	b.WriteString("# WARNING: This script contains a single-use credential (one-time pairing token).\n")
	b.WriteString("# Do not commit or share this file.\n")
	b.WriteString("\n")
	b.WriteString("$binaryUrl = ")
	b.WriteString(quotePowerShell(hubURL + "/api/v1/hub/worker-binaries/" + platform))
	b.WriteString("\n")
	b.WriteString("$binaryOut = ")
	b.WriteString(quotePowerShell(filename))
	b.WriteString("\n")
	b.WriteString("\n")
	b.WriteString("Write-Host 'Downloading Timingdex Worker binary...'\n")
	b.WriteString("Invoke-WebRequest -Uri $binaryUrl -OutFile $binaryOut\n")
	b.WriteString("\n")
	b.WriteString("Write-Host 'Enrolling Worker...'\n")

	b.WriteString("& ")
	b.WriteString(quotePowerShell("./" + filename))
	b.WriteString(" worker enroll")
	writePSArg(&b, "--hub", hubURL)
	writePSArg(&b, "--pairing", pairingToken)
	if strings.TrimSpace(name) != "" {
		writePSArg(&b, "--name", strings.TrimSpace(name))
	}
	for _, m := range mounts {
		writePSArg(&b, "--mount", m.RootID+"="+m.Path)
	}
	if strings.TrimSpace(cacheDir) != "" {
		writePSArg(&b, "--cache", strings.TrimSpace(cacheDir))
	}
	writePSArg(&b, "--config", configPath)
	b.WriteString("\n")
	b.WriteString("\n")
	b.WriteString("Write-Host ''\n")
	b.WriteString("Write-Host 'Enrolled successfully. Run the Worker with:'\n")
	b.WriteString("Write-Host ")
	b.WriteString(quotePowerShell("  .\\" + filename + " worker run --config " + configPath))
	b.WriteString("\n")

	return b.String()
}

func writePSArg(b *strings.Builder, flag, value string) {
	b.WriteString(" `\n")
	b.WriteString("  ")
	b.WriteString(flag)
	b.WriteString(" ")
	b.WriteString(quotePowerShell(value))
}

func (s *Server) writeWorkerSetupScript(w http.ResponseWriter, script string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(script))
}

func quotePowerShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func quotePOSIX(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

const workerSetupPageHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · Worker 安装向导</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#101827;color:#edf3ff;font:15px ui-sans-serif,system-ui,-apple-system,sans-serif}header{padding:15px 5vw;border-bottom:1px solid #293953;display:flex;gap:18px;align-items:center;background:#101827ee;position:sticky;top:0;z-index:2;backdrop-filter:blur(12px)}a{color:#b8c8ff;text-decoration:none}.brand{color:#fff;font-weight:800;margin-right:auto}.wrap{max-width:960px;margin:auto;padding:30px 24px 80px}.step-nav{display:flex;gap:0;margin-bottom:28px;border-radius:10px;overflow:hidden;border:1px solid #2c3d5b}.step-nav div{flex:1;text-align:center;padding:12px;font-size:13px;font-weight:700;background:#172238;color:#6c7fa2;border-right:1px solid #2c3d5b;transition:background .2s,color .2s}.step-nav div:last-child{border-right:0}.step-nav .active{background:#2b4070;color:#eef4ff}.step{display:none}.step.active{display:block}.panel{background:#172238;border:1px solid #2c3d5b;border-radius:14px;padding:20px;margin-bottom:16px}.panel h2{margin:0 0 14px;font-size:17px}.field{margin:12px 0}.field label{font-size:12px;color:#bfcae0;font-weight:800;display:block;margin:0 0 5px}input,select,textarea{width:100%;font:inherit;padding:10px;border-radius:8px;background:#0e1728;color:#fff;border:1px solid #374965}button{border:0;border-radius:8px;padding:10px 16px;font:inherit;font-weight:800;cursor:pointer}.primary{background:#8ca7ff;color:#0d1830}.secondary{background:#2e405e;color:#eaf1ff}.success{background:#3a9668;color:#d3fce4}.muted{color:#aab8d0;font-size:13px;line-height:1.6}.row{display:flex;gap:10px;align-items:center;flex-wrap:wrap}.binary-grid{display:grid;grid-template-columns:repeat(3,1fr);gap:10px;margin:10px 0}.binary-card{border:1px solid #374965;border-radius:10px;padding:12px;text-align:center}.binary-card .avail{color:#5fcb8a;font-weight:800}.binary-card .unavail{color:#ed9b7b}.binary-card .size{color:#aab8d0;font-size:12px}.fingerprint{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px;color:#d9e5ff;word-break:break-all;background:#0e1728;border-radius:8px;padding:8px;margin:6px 0}.mount-row{display:grid;grid-template-columns:1fr 1fr;gap:8px;align-items:end;margin:8px 0;padding:8px;background:#0e1728;border-radius:8px}.mount-row .path{font-size:13px;color:#bfcae0;margin-bottom:4px}.script-box{background:#0e1728;border:1px solid #374965;border-radius:10px;padding:12px;max-height:420px;overflow:auto;white-space:pre-wrap;font:13px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;color:#d9e5ff;margin:10px 0}.copy-btn{background:#4a618a;color:#fff;border:0;border-radius:8px;padding:8px 12px;font-size:13px;cursor:pointer;font-weight:700}.center{text-align:center;margin:18px 0}.step-actions{display:flex;gap:10px;margin-top:16px}.nav-btn{background:#2b4070;color:#eef4ff}.hint{color:#ffc98d;font-size:13px;margin:8px 0}.info-line{display:flex;gap:8px;margin:6px 0;align-items:baseline}.info-label{color:#aab8d0;font-size:12px;min-width:80px}.info-value{color:#edf3ff}.tls-on{color:#5fcb8a}.tls-off{color:#ed9b7b}@media(max-width:600px){.binary-grid{grid-template-columns:1fr}.mount-row{grid-template-columns:1fr}}
</style></head><body data-worker-setup-wizard>
<!--SHELL_HEADER-->
<div class="wrap">
<div class="step-nav" id="step-nav"><div class="active" data-step="1">1. 环境概览</div><div data-step="2">2. 配置 Worker</div><div data-step="3">3. 生成安装脚本</div><div data-step="4">4. 启动 Worker</div></div>

<div class="step active" id="step-1">
<div class="panel"><h2>Hub 连接信息</h2><div id="hub-info"><div class="muted">正在加载环境信息…</div></div></div>
<div class="panel"><h2>Worker 二进制文件</h2><p class="muted">将编译好的二进制文件放入 Worker-binaries 目录后刷新页面即可生效。</p><div class="binary-grid" id="binary-grid"></div><div class="hint">💡 在本机交叉编译：<code>GOOS=windows GOARCH=amd64 go build -o timingdex-windows-amd64.exe ./cmd/timingdex</code><br>将文件放置在 <code>&lt;DataDir&gt;/worker-binaries/</code> 目录下。</div></div>
<div class="step-actions"><button class="nav-btn primary" onclick="goStep(2)">下一步 →</button></div>
</div>

<div class="step" id="step-2">
<div class="panel"><h2>Worker 配置</h2>
<div class="field"><label for="platform">目标平台</label><select id="platform"><option value="linux-amd64">Linux AMD64</option><option value="linux-arm64">Linux ARM64</option><option value="windows-amd64">Windows AMD64</option></select></div>
<div class="field"><label for="worker-name">Worker 名称（可选）</label><input id="worker-name" type="text" placeholder="例如：nas-worker-1"></div>
<div class="field"><label for="cache-dir">缓存目录（可选）</label><input id="cache-dir" type="text" placeholder="例如：/opt/timingdex/cache"></div>
</div>
<div class="panel"><h2>素材库挂载映射</h2><p class="muted">将 Hub 上的每个素材目录映射到 Worker 上的本地路径。</p><div id="mounts-panel"></div></div>
<div class="step-actions"><button class="nav-btn secondary" onclick="goStep(1)">← 上一步</button><button class="nav-btn primary" onclick="goStep(3)">下一步 →</button></div>
</div>

<div class="step" id="step-3">
<div class="panel"><h2>生成安装脚本</h2>
<p class="muted">点击下方按钮后，系统会生成一次性的配对 Token 并生成完整的安装脚本。脚本包含一次性凭据，请勿提交到版本控制或分享。</p>
<div id="script-area"><div class="center"><button class="primary" onclick="generateScript()" id="gen-btn">生成配对 Token 并安装脚本</button></div></div>
</div>
<div class="step-actions"><button class="nav-btn secondary" onclick="goStep(2)">← 上一步</button></div>
</div>

<div class="step" id="step-4">
<div class="panel"><h2>启动 Worker</h2>
<p class="muted">安装脚本执行完成后，在 Worker 机器上运行以下命令启动持续处理：</p>
<div class="script-box">timingdex worker run --config ./timingdex-worker.json</div>
<p class="muted">也可以使用 <code>timingdex worker doctor --config ./timingdex-worker.json</code> 检查 Worker 状态。</p>
<p class="muted">在 Hub 的「<a href="/workers">节点管理</a>」页面可查看 Worker 的上线状态。</p>
</div>
</div>
</div>

<script>
const esc=function(v){return String(v??'').replace(/[&<>"']/g,function(c){return {'&':'&amp;','>':'&gt;','<':'&lt;','"':'&quot;',"'":'&#39;'}[c]})};
var ctx=null,pairingToken=null;
function adminToken(){var el=document.getElementById('admin-token');return el?el.value.trim():''}
function authHeaders(base){var headers=new Headers(base||{});var token=adminToken();if(token)headers.set('Authorization','Bearer '+token);return headers}
async function json(url,opt){opt=opt||{};var r=await fetch(url,{...opt,headers:authHeaders(opt.headers)});if(!r.ok){var msg='';try{msg=await r.text()}catch(e){msg=r.statusText}throw new Error(msg)}return r.json()}
function goStep(n){for(var i=1;i<=4;i++){document.getElementById('step-'+i).className='step'+(i===n?' active':'');document.querySelectorAll('.step-nav div')[i-1].className=(i===n?'active':'')}}
function renderBinaries(){var html=[],bins=ctx.available_binaries||{},order=['linux-amd64','linux-arm64','windows-amd64'];for(var i=0;i<order.length;i++){var key=order[i],bin=bins[key]||{},name=key,exists=!!bin.exists,size=bin.size_bytes||0;html.push('<div class="binary-card"><div class="'+(exists?'avail':'unavail')+'">'+(exists?'\u2713 可用':'&times; 不可用')+'</div><div>'+esc(name)+'</div><div class="size">'+(exists?formatSize(size):'未上传')+'</div></div>')}document.getElementById('binary-grid').innerHTML=html.join('')}
function formatSize(b){if(b<1024)return b+' B';if(b<1048576)return(b/1024).toFixed(1)+' KB';return(b/1048576).toFixed(1)+' MB'}
function renderMounts(){var panel=document.getElementById('mounts-panel'),roots=ctx.library_roots||[];if(!roots.length){panel.innerHTML='<div class="muted">暂无素材库根目录。请先在启动配置中添加。</div>';return}var html=[];for(var i=0;i<roots.length;i++){var r=roots[i];html.push('<div class="mount-row"><div><div class="path">'+esc(r.id)+'<br>'+esc(r.path)+'</div></div><div><label style="font-size:12px;color:#bfcae0;font-weight:800;display:block;margin-bottom:3px">Worker 本地路径</label><input class="mount-path" data-root-id="'+esc(r.id)+'" type="text" placeholder="/mnt/footage/'+esc(r.id)+'"></div></div>')}panel.innerHTML=html.join('')}
async function init(){try{ctx=await json('/api/v1/hub/worker-setup/context');var hubInfo=document.getElementById('hub-info');hubInfo.innerHTML='<div class="info-line"><span class="info-label">Hub URL</span><span class="info-value">'+esc(ctx.hub_url)+'</span></div><div class="info-line"><span class="info-label">TLS</span><span class="info-value '+(ctx.tls?'tls-on':'tls-off')+'">'+(ctx.tls?'已启用':'未启用')+'</span></div>'+(ctx.fingerprint?'<div class="info-line"><span class="info-label">证书指纹</span><span class="fingerprint">'+esc(ctx.fingerprint)+'</span></div>':'')+'<div class="info-line"><span class="info-label">素材根目录</span><span class="info-value">'+(ctx.library_roots||[]).length+' 个</span></div>';renderBinaries();renderMounts()}catch(e){document.getElementById('hub-info').innerHTML='<div class="muted">无法获取环境信息：'+esc(e.message)+'</div>'}}
async function generateScript(){var btn=document.getElementById('gen-btn');btn.disabled=true;btn.textContent='正在生成…';try{var t=adminToken();if(!t){alert('请先在顶部填入 Hub 管理 Token');btn.disabled=false;btn.textContent='生成配对 Token 并安装脚本';return}var pairing=await json('/api/v1/hub/worker-pairings',{method:'POST'});pairingToken=pairing.token;var platform=document.getElementById('platform').value;var name=document.getElementById('worker-name').value.trim();var cacheDir=document.getElementById('cache-dir').value.trim();var mountInputs=document.querySelectorAll('.mount-path');var mounts=[];for(var i=0;i<mountInputs.length;i++){var path=mountInputs[i].value.trim();if(path){mounts.push({root_id:mountInputs[i].getAttribute('data-root-id'),path:path})}}var body=JSON.stringify({platform:platform,name:name,pairing_token:pairingToken,mounts:mounts,cache_dir:cacheDir});var script=await fetch('/api/v1/hub/worker-setup/script',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:body});if(!script.ok){var errText='';try{errText=await script.text()}catch(e){errText=script.statusText}throw new Error(errText)}var scriptText=await script.text();document.getElementById('script-area').innerHTML='<button class="copy-btn" onclick="copyScript()" id="copy-btn">复制脚本</button><div class="script-box" id="script-output">'+esc(scriptText)+'</div><div class="hint">脚本中包含一次性配对 Token，使用后即失效。请在目标 Worker 机器上执行。</div><div class="step-actions"><button class="nav-btn primary" onclick="goStep(4)">查看启动说明 →</button></div>'}catch(e){document.getElementById('script-area').innerHTML='<div class="muted" style="color:#ffb7ac">生成失败：'+esc(e.message)+'</div><div class="center"><button class="primary" onclick="generateScript()">重试</button></div>'}finally{btn.disabled=false}}
function copyScript(){var el=document.getElementById('script-output');if(!el)return;var range=document.createRange();range.selectNode(el);window.getSelection().removeAllRanges();window.getSelection().addRange(range);try{document.execCommand('copy');var btn=document.getElementById('copy-btn');btn.textContent='已复制'}catch(e){}}init();
</script></body></html>`
