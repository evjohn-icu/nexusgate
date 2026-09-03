package worker

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

// LocalAdmin is the Worker's own settings page, served on loopback.
//
// It exists for exactly the three values the Hub cannot push down: the hub URL,
// the pinned certificate fingerprint and the node token. Everything else a
// Worker obeys — throttle, mounts, capabilities — travels on the connection
// those three establish, so it can be Hub-decided; these three are the trust
// anchor and have to be editable on the machine itself.
//
// A browser page rather than a native dialog is not a shortcut: this project
// already ships every UI as an inline HTML constant with no build step, it costs
// no dependency, and it behaves identically on Windows and Linux. The Windows
// tray only has to open a URL.
type LocalAdmin struct {
	configPath string
	// pathToken gates every route. The listener is on a loopback port, which
	// means any other process on the machine could reach it; without the token
	// that process could read the hub URL and replace the node token. It lives in
	// the URL because the only client is a browser the tray launches itself.
	pathToken string
	listener  net.Listener
}

func NewLocalAdmin(configPath string) (*LocalAdmin, error) {
	if strings.TrimSpace(configPath) == "" {
		return nil, errors.New("worker settings server requires a config path")
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate settings token: %w", err)
	}
	// Port 0 lets the OS pick: a fixed port would collide with whatever else the
	// operator runs, and the tray reads the real address back out anyway.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind worker settings server: %w", err)
	}
	return &LocalAdmin{configPath: configPath, pathToken: hex.EncodeToString(raw), listener: listener}, nil
}

func (a *LocalAdmin) Address() string { return a.listener.Addr().String() }

func (a *LocalAdmin) pathPrefix() string { return "/s/" + a.pathToken }

// URL is what the tray hands to the browser. It carries the token, so it must be
// treated like a credential: never logged at info level, never printed anywhere
// it would be captured.
func (a *LocalAdmin) URL() string { return "http://" + a.Address() + a.pathPrefix() + "/" }

func (a *LocalAdmin) Close() error { return a.listener.Close() }

func (a *LocalAdmin) Serve(ctx context.Context) error {
	server := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.Serve(a.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *LocalAdmin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+a.pathPrefix()+"/{$}", a.page)
	mux.HandleFunc("GET "+a.pathPrefix()+"/config", a.readConfig)
	mux.HandleFunc("PUT "+a.pathPrefix()+"/config", a.writeConfig)
	// Anything else — including a near-miss token — gets 404. A 401 would confirm
	// that a settings server is listening on this port, which a prober scanning
	// loopback ports should not learn.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// authorized compares in constant time. The comparison is cheap and the
// alternative leaks token bytes through response timing to a local attacker who
// can retry freely.
func (a *LocalAdmin) authorized(path string) bool {
	rest, ok := strings.CutPrefix(path, "/s/")
	if !ok {
		return false
	}
	candidate, _, _ := strings.Cut(rest, "/")
	if len(candidate) != len(a.pathToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(a.pathToken)) == 1
}

// configView is deliberately not Config. The node token is a credential: it can
// be replaced but never read back, the same rule the Hub applies to its admin
// token and provider keys.
type configView struct {
	HubURL          string            `json:"hub_url"`
	Fingerprint     string            `json:"certificate_fingerprint"`
	TokenConfigured bool              `json:"token_configured"`
	TokenHint       string            `json:"token_hint"`
	CacheDir        string            `json:"cache_dir,omitempty"`
	Mounts          map[string]string `json:"mounts,omitempty"`
	WorkerName      string            `json:"worker_name,omitempty"`
}

// tokenHint identifies which token is stored without revealing enough to use it.
func tokenHint(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 4 {
		return "****"
	}
	return "****" + trimmed[len(trimmed)-4:]
}

func (a *LocalAdmin) currentConfig() (Config, error) {
	config, err := LoadConfig(a.configPath)
	if err == nil {
		return config, nil
	}
	// A Worker that has never enrolled has no valid config, and that is exactly
	// when this page is most needed. Fall back to whatever parses so the operator
	// can fill in the gaps rather than being locked out by the validation.
	raw, readErr := readFileIfPresent(a.configPath)
	if readErr != nil {
		return Config{}, err
	}
	var partial Config
	if len(raw) > 0 {
		if unmarshalErr := json.Unmarshal(raw, &partial); unmarshalErr != nil {
			return Config{}, unmarshalErr
		}
	}
	return partial, nil
}

func (a *LocalAdmin) readConfig(w http.ResponseWriter, r *http.Request) {
	config, err := a.currentConfig()
	if err != nil {
		http.Error(w, "worker configuration is unreadable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeLocalJSON(w, http.StatusOK, configView{
		HubURL:          config.HubURL,
		Fingerprint:     config.CertificateFingerprint,
		TokenConfigured: strings.TrimSpace(config.Token) != "",
		TokenHint:       tokenHint(config.Token),
		CacheDir:        config.CacheDir,
		Mounts:          config.Mounts,
		WorkerName:      config.Registration.Name,
	})
}

func (a *LocalAdmin) writeConfig(w http.ResponseWriter, r *http.Request) {
	var request struct {
		HubURL      string  `json:"hub_url"`
		Fingerprint string  `json:"certificate_fingerprint"`
		Token       *string `json:"token"`
		CacheDir    *string `json:"cache_dir"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&request); err != nil {
		http.Error(w, "invalid settings payload", http.StatusBadRequest)
		return
	}

	// Start from the stored config and change only the submitted fields. Building
	// a fresh Config from the request would silently drop the mounts and the
	// registration, which this page never shows and therefore cannot resend.
	config, err := a.currentConfig()
	if err != nil {
		http.Error(w, "worker configuration is unreadable: "+err.Error(), http.StatusInternalServerError)
		return
	}

	hubURL, err := normalizeHubURL(request.HubURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fingerprint, err := validateFingerprint(request.Fingerprint)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	config.HubURL = hubURL
	config.CertificateFingerprint = fingerprint
	// An absent or empty token means "keep the current one": the common edit is
	// moving the Hub to a new address, and forcing the operator to re-paste a
	// credential they cannot read back would make that edit impossible.
	if request.Token != nil && strings.TrimSpace(*request.Token) != "" {
		config.Token = strings.TrimSpace(*request.Token)
	}
	if request.CacheDir != nil {
		config.CacheDir = strings.TrimSpace(*request.CacheDir)
	}
	if strings.TrimSpace(config.Token) == "" {
		http.Error(w, "a node token is required; enroll this Worker first", http.StatusBadRequest)
		return
	}

	if err := SaveConfig(a.configPath, config); err != nil {
		http.Error(w, "could not save worker configuration: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.readConfig(w, r)
}

// normalizeHubURL rejects anything the Worker client cannot use as a base. A URL
// carrying a path is the trap worth naming: the client appends its own API paths,
// so "https://nas:8787/api/v1" produces requests to /api/v1/api/v1/... that fail
// with a confusing 404 far from the cause.
func normalizeHubURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", errors.New("hub_url is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("hub_url is not a URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("hub_url must start with http:// or https://")
	}
	if parsed.Host == "" {
		return "", errors.New("hub_url is missing a host")
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("hub_url must be a bare origin, with no path or query")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// validateFingerprint accepts an empty value because a Hub with TLS off has no
// certificate to pin. Anything present must be a full SHA-256 digest: a
// truncated or mistyped one would be compared and fail every connection, which
// looks like a network fault rather than a typo.
//
// It canonicalises through normalizeFingerprint -- the same function the client
// uses when it compares a presented certificate -- so the stored form is exactly
// the compared form. Normalising differently here would let the page accept a
// fingerprint that could never match.
func validateFingerprint(raw string) (string, error) {
	trimmed := normalizeFingerprint(strings.TrimSpace(raw))
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) != 64 {
		return "", fmt.Errorf("certificate_fingerprint must be 64 hex characters, got %d", len(trimmed))
	}
	if _, err := hex.DecodeString(trimmed); err != nil {
		return "", errors.New("certificate_fingerprint must be hexadecimal")
	}
	return trimmed, nil
}

func writeLocalJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The page and its data are per-process and token-scoped; a cached copy in the
	// browser would outlive the token.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func runtimeIsWindows() bool { return runtime.GOOS == "windows" }

func readFileIfPresent(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

func (a *LocalAdmin) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Same house style as the Hub pages: one inline constant, no build step. The
	// fetch paths are relative so the token in the URL carries over without the
	// page ever containing it as a literal.
	_, _ = w.Write([]byte(workerSettingsHTML))
}

const workerSettingsHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>NexusSlate Worker · 设置</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#101827;color:#edf3ff;font:14px ui-sans-serif,system-ui,-apple-system,sans-serif}.wrap{max-width:640px;margin:auto;padding:36px 22px 60px}h1{font-size:26px;margin:0 0 6px;letter-spacing:-.03em}.muted{color:#aab8d0;line-height:1.6;margin:0}.panel{background:#172238;border:1px solid #2c3d5b;border-radius:14px;padding:20px;margin-top:20px}.panel h2{margin:0 0 4px;font-size:16px}.hint{color:#9fb0ce;font-size:12px;line-height:1.6;margin:0 0 14px}label{display:block;font-weight:700;color:#dce6fb;margin:14px 0 5px}input{width:100%;padding:10px;border-radius:8px;border:1px solid #354764;background:#0e1728;color:#fff;font:inherit}.note{color:#93a5c3;font-size:12px;line-height:1.5;margin:6px 0 0}button{background:#86a3ff;color:#091227;border:0;border-radius:9px;padding:11px 16px;font-weight:800;cursor:pointer;margin-top:20px}.status{border-radius:10px;padding:12px 14px;margin-top:16px;line-height:1.6;display:none}.status.ok{display:block;background:#153c31;color:#a4efc8;border:1px solid #23664f}.status.bad{display:block;background:#3f2230;color:#ffc2cc;border:1px solid #6d3547}.kv{display:flex;gap:8px;margin:5px 0;font-size:13px}.kv b{color:#aab8d0;font-weight:700;min-width:78px}code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;color:#cfe0ff;word-break:break-all}</style></head><body><div class="wrap">
<h1>Worker 设置</h1>
<p class="muted">这台机器上的本地设置。只有下面三项无法由 Hub 下发——它们是建立连接本身所需要的；挂载、限速、能力开关都由 Hub 决定。</p>
<div class="panel"><h2>当前节点</h2><div id="current" class="muted">正在读取…</div></div>
<div class="panel"><h2>连接设置</h2><p class="hint">改完保存即写入 worker.json。正在运行的 Worker 需要退出后重新启动才会用新设置。</p>
<label for="hub">Hub 地址</label><input id="hub" placeholder="https://nas:8787"><p class="note">只填协议加主机名和端口，不要带路径——客户端会自己接 API 路径。</p>
<label for="fp">证书指纹（SHA-256）</label><input id="fp" placeholder="64 位十六进制，Hub 启动时会打印"><p class="note">留空表示不做钉扎，仅在 Hub 关闭 TLS 时才这样用。冒号、空格和 sha256 前缀会被自动去掉。</p>
<label for="token">节点 Token</label><input id="token" type="password" autocomplete="off" placeholder="留空表示保持当前 Token 不变"><p class="note">出于安全，已存的 Token 不会回显。只有在重新配对拿到新 Token 时才需要填这里。</p>
<button id="save" onclick="save()">保存</button><div id="status" class="status"></div></div>
</div><script>
function esc(s){return String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function say(m,ok){const el=document.getElementById('status');el.className='status '+(ok?'ok':'bad');el.textContent=m}
function render(d){const mounts=Object.entries(d.mounts||{});
document.getElementById('current').innerHTML=
'<div class="kv"><b>节点名</b><span>'+esc(d.worker_name||'未命名')+'</span></div>'+
'<div class="kv"><b>Token</b><span>'+(d.token_configured?'<code>'+esc(d.token_hint)+'</code>':'未配置——请先在 Hub 上配对')+'</span></div>'+
'<div class="kv"><b>缓存</b><span><code>'+esc(d.cache_dir||'系统临时目录')+'</code></span></div>'+
'<div class="kv"><b>挂载</b><span>'+(mounts.length?mounts.map(([k,v])=>'<code>'+esc(k)+' → '+esc(v)+'</code>').join('<br>'):'无')+'</span></div>';
document.getElementById('hub').value=d.hub_url||'';
document.getElementById('fp').value=d.certificate_fingerprint||''}
async function load(){try{const d=await fetch('config',{cache:'no-store'}).then(async r=>{if(!r.ok)throw Error(await r.text());return r.json()});render(d)}catch(e){say('无法读取配置：'+e.message,false)}}
async function save(){const b=document.getElementById('save');b.disabled=true;
const body={hub_url:document.getElementById('hub').value.trim(),certificate_fingerprint:document.getElementById('fp').value.trim(),token:document.getElementById('token').value};
try{const d=await fetch('config',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}).then(async r=>{if(!r.ok)throw Error(await r.text());return r.json()});
render(d);document.getElementById('token').value='';say('已保存。重启 Worker 后生效。',true)}catch(e){say('保存失败：'+e.message,false)}finally{b.disabled=false}}
load();
</script></body></html>`
