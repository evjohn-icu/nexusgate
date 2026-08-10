package api

import "strings"

// The shared application shell. Every page composes the same left sidebar:
// brand, one navigation (素材/创作/系统 groups), one admin-token input
// (memory-only; page scripts read it through their existing getElementById
// helpers), and the four-cell system status strip. The shell is injected by
// shelledPage — the per-page headers this replaces were ten copies of the
// same idea with ten slightly different styles, which is how pages drifted
// into different products.
//
// The shell never touches a page's own <script>: each page keeps its script
// block, and the shared helpers below use names (getAdminToken, shellAuthHeaders,
// refreshStatus) no page already declares, so node --check keeps passing and
// a page cannot accidentally shadow a shell function.

const shellHeaderMarker = "<!--SHELL_HEADER-->"

// shelledPage injects the shared shell into one page constant: the shell CSS
// immediately before the page's own </style> close (so it wins at equal
// specificity), the shell header in place of the {{marker}}, and the shared
// script before </body>. Each page constant carries exactly one </style> and
// one </body>, so the replace-first semantics are exact per page. The CSS
// must land INSIDE the style element: appending it after </style> makes the
// browser treat it as raw text and the whole shell silently loses its
// styling — the exact silent no-op this project's anchor tests exist for.
func shelledPage(page string) string {
	page = strings.Replace(page, "</style>", shellCSS+"</style>", 1)
	page = strings.Replace(page, shellHeaderMarker, shellHeaderHTML(), 1)
	page = strings.Replace(page, "</body>", shellScriptBlock+"</body>", 1)
	return page
}

// shellHeaderHTML builds the shared sidebar. The brand span keeps the exact
// text brandedPage rewrites, so Re:Footage branding applies to every page
// through the existing mechanism. The sidebar is always the first element in
// <body> (every page constant carries the marker directly after the opening
// tag), and body padding reserves its 220px gutter on wide screens.
func shellHeaderHTML() string {
	var b strings.Builder
	b.WriteString(`<aside class="shell-sidebar" data-app-shell><div class="shell-brand-row"><span class="brand">Timingdex</span><span class="shell-tagline">本地素材智能层</span></div><nav class="shell-nav" aria-label="主导航">`)
	groups := []struct {
		title string
		links [][2]string // label, href
	}{
		{"素材", [][2]string{{"素材库", "/"}, {"收藏", "/collections"}}},
		{"创作", [][2]string{{"翻新方案", "/repurpose"}, {"Tags", "/tags"}}},
		{"系统", [][2]string{{"处理任务", "/progress"}, {"模型服务", "/providers"}, {"Workers", "/workers"}, {"素材目录", "/library-roots"}, {"启动配置", "/setup"}, {"设置", "/settings"}}},
	}
	for _, g := range groups {
		b.WriteString(`<div class="nav-group"><span class="nav-group-title">` + g.title + `</span>`)
		for _, link := range g.links {
			b.WriteString(`<a href="` + link[1] + `" data-nav="` + link[1] + `" class="nav-link">` + link[0] + `</a>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</nav><div class="shell-actions"><input id="admin-token" type="password" autocomplete="off" placeholder="Hub 管理 Token（仅存于本页内存）" oninput="refreshStatus()" aria-label="Hub 管理 Token"><div class="status-strip" data-status-strip aria-label="系统状态">`)
	cells := []struct {
		id, title, label string
	}{
		{"status-hub", "Hub 状态", "Hub"},
		{"status-pipeline", "处理队列状态", "Pipeline"},
		{"status-providers", "模型服务状态", "Providers"},
		{"status-workers", "Worker 状态", "Workers"},
	}
	for _, c := range cells {
		b.WriteString(`<a class="status-cell" data-` + c.id + ` href="` + cellHref(c.id) + `" title="` + c.title + `"><i class="dot"></i><b id="` + c.id + `">…</b></a>`)
	}
	b.WriteString(`</div></div></aside>`)
	return b.String()
}

func cellHref(id string) string {
	switch id {
	case "status-pipeline":
		return "/progress"
	case "status-providers":
		return "/providers"
	case "status-workers":
		return "/workers"
	default:
		return "/"
	}
}

// shellCSS is the shared design surface. Page-specific styles still exist and
// win for their own classes; this block supplies the shell chrome and the
// shared tokens pages can rely on. The palette matches the existing pages
// (#0c1422 family) so the shell does not clash with any page's dark theme.
//
// Layout approach: body padding, not a sibling selector. The sidebar is
// position:fixed, so it is out of flow; body{padding-left:220px} reserves the
// gutter page-agnostically — it cannot miss a page whose first sibling is not
// the content wrapper, and it does not depend on the marker staying the first
// element of <body>. All ten page constants do put the marker directly after
// <body>, but body padding keeps that guarantee from ever becoming a hidden
// dependency. Below 860px the sidebar becomes a compact in-flow top bar:
// position:relative makes it participate in layout, so body padding is zeroed
// and the content flows underneath the bar. The status strip and the
// admin-token input keep their exact classes, ids and oninput wiring.
const shellCSS = `
.shell-sidebar{position:fixed;left:0;top:0;bottom:0;width:220px;z-index:30;background:#0c1422;border-right:1px solid #273750;display:flex;flex-direction:column;padding:18px 14px;gap:18px;overflow-y:auto}
body{padding-left:220px}
.shell-brand-row{display:flex;align-items:baseline;gap:9px}
.shell-tagline{color:#93aaff;font-size:11px;font-weight:800;letter-spacing:.1em}
.shell-nav{display:flex;flex-direction:column;gap:16px}
.nav-group{display:flex;flex-direction:column;gap:3px}
.nav-group-title{color:#61759b;font-size:10px;font-weight:850;letter-spacing:.1em;margin-bottom:2px}
.nav-link{color:#b7c8eb;text-decoration:none;font-size:13px;padding:3px 6px;border-radius:6px;white-space:nowrap}
.nav-link:hover{color:#fff}
.nav-link.active{color:#fff;font-weight:800}
.shell-actions{display:flex;flex-direction:column;gap:12px;margin-top:auto}
.shell-actions input{width:100%;padding:9px 11px;border:1px solid #354965;border-radius:9px;background:#111d30;color:#fff;font:inherit}
.status-strip{display:flex;gap:8px;flex-wrap:wrap}
.status-cell{display:flex;align-items:center;gap:6px;color:#9fb0ce;text-decoration:none;border:1px solid #2b3e5b;border-radius:99px;padding:5px 11px;font-size:12px;white-space:nowrap}
.status-cell:hover{border-color:#506e9d;color:#fff}
.status-cell b{color:#edf3ff;font-weight:700}
.status-cell .dot{width:7px;height:7px;border-radius:50%;background:#47597a}
.status-cell .dot.ok{background:#70d7b1}
.status-cell .dot.warn{background:#ffc783}
.status-cell .dot.err{background:#ff7b8a}
.status-cell .dot.off{background:#47597a}
@media(max-width:860px){body{padding-left:0}.shell-sidebar{position:relative;left:auto;top:auto;bottom:auto;width:auto;height:auto;flex-direction:row;flex-wrap:wrap;align-items:center;gap:12px;padding:12px 16px;border-right:0;border-bottom:1px solid #273750;overflow:visible}.shell-nav{flex-direction:row;flex-wrap:wrap;align-items:center;gap:10px 18px}.nav-group{flex-direction:row;align-items:center;gap:10px}.nav-group-title{margin-bottom:0}.shell-actions{flex-direction:row;flex-wrap:wrap;align-items:center;gap:10px;margin-left:auto;margin-top:0}.shell-actions input{width:min(250px,38vw)}}
`

// shellScriptBlock is the shared script every page carries. It defines only
// names no page declares, so it can be injected without clashing with a
// page's own script block. Pages keep their own adminToken()/authHeaders()
// helpers — they read the same #admin-token input the shell provides, so the
// single input serves both the strip and the pages.
const shellScriptBlock = `<script>
function getAdminToken(){const el=document.getElementById('admin-token');return el?el.value.trim():''}
function shellAuthHeaders(){const headers=new Headers();const token=getAdminToken();if(token)headers.set('Authorization','Bearer '+token);return headers}
function statusCell(id,state,text){const el=document.getElementById(id);if(!el)return;el.textContent=text;const cell=el.closest('.status-cell');if(cell){const dot=cell.querySelector('.dot');if(dot)dot.className='dot '+state}}
// The old per-page headers marked the current page with nav-active; the
// shell header derives it from the URL instead of taking an argument, so a
// page can never forget to pass it. Runs at end of body, so the DOM is ready.
document.querySelectorAll('.nav-link').forEach(function(a){if(a.getAttribute('href')===location.pathname)a.classList.add('active')});
async function refreshStatus(){
  try{const r=await fetch('/api/v1/health');statusCell('status-hub',r.ok?'ok':'err',r.ok?'Hub 正常':'Hub 异常')}catch(e){statusCell('status-hub','err','Hub 异常')}
  try{const s=await fetch('/api/v1/jobs/summary',{headers:shellAuthHeaders()}).then(r=>r.ok?r.json():null);if(s){let state='idle',text='空闲';if(s.running>0){state='ok';text='运行中 '+s.running}else if(s.deferred>0){state='warn';text='等待额度 '+s.deferred}else if((s.failed||0)+(s.terminal||0)>0){state='err';text='失败 '+(s.failed+s.terminal)}else if(s.pending>0){text='排队 '+s.pending}statusCell('status-pipeline',state,text)}else{statusCell('status-pipeline','off','—')}}catch(e){statusCell('status-pipeline','off','—')}
  const token=getAdminToken();
  if(!token){statusCell('status-providers','off','需Token');statusCell('status-workers','off','需Token');return}
  try{const r=await fetch('/api/v1/admin/provider-channels/status',{headers:shellAuthHeaders()});if(r.ok){const caps=await r.json();const list=Array.isArray(caps)?caps:[];const anyData=list.some(c=>c&&c.has_runtime_data);let okN=0,degradedN=0;list.forEach(function(c){if(!c||!c.has_runtime_data||!c.snapshot||!Array.isArray(c.snapshot.channels))return;c.snapshot.channels.forEach(function(ch){if(!ch||!ch.enabled)return;if(ch.available)okN++;else degradedN++})});if(!anyData||(okN+degradedN)===0){statusCell('status-providers','off','未配置')}else if(degradedN>0){statusCell('status-providers','warn','降级 '+degradedN)}else{statusCell('status-providers','ok','正常 '+okN)}}else{statusCell('status-providers','err','异常')}}catch(e){statusCell('status-providers','off','—')}
  try{const w=await fetch('/api/v1/hub/workers',{headers:shellAuthHeaders()}).then(r=>r.ok?r.json():null);if(Array.isArray(w)){const on=w.filter(x=>x.status==='online').length;const off=w.length-on;statusCell('status-workers',off>0?'warn':'ok',on+' 在线'+(off?' / '+off+' 离线':''))}else{statusCell('status-workers','off','—')}}catch(e){statusCell('status-workers','off','—')}
}
refreshStatus();setInterval(refreshStatus,15000);
</script>`
