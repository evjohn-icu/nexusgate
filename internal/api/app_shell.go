package api

import "strings"

// The shared application shell. Every page composes the same left sidebar:
// brand, one navigation (核心/高级 groups), one admin-token input
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
func shelledPage(page string, loc locale) string {
	page = strings.Replace(page, "</style>", shellCSS+"</style>", 1)
	page = strings.Replace(page, shellHeaderMarker, shellHeaderHTML(loc), 1)
	page = strings.Replace(page, "</body>", shellScriptBlock+"</body>", 1)
	return page
}

// shellHeaderHTML builds the shared sidebar for one locale. The brand span
// keeps the exact text brandedPage rewrites, so Re:Footage branding applies
// to every page through the existing mechanism; the product mark and the
// LOCAL FOOTAGE INDEX tagline are brand copy and stay untranslated. All other
// labels are [[i18n:shell.*]] markers that serveLocalizedPage resolves with
// the selected catalog. The locale selector's option values are the canonical
// tags (asserted by the browser smoke), and the current locale is preselected.
func shellHeaderHTML(loc locale) string {
	var b strings.Builder
	b.WriteString(`<aside class="shell-sidebar" data-app-shell><div class="shell-brand-row"><span class="brand">Timingdex</span><span class="shell-tagline">LOCAL FOOTAGE INDEX</span><button type="button" class="shell-menu-toggle" id="shell-menu-toggle" aria-expanded="false" aria-controls="shell-nav">[[i18n:shell.nav.toggle]]</button></div><nav class="shell-nav" id="shell-nav" aria-label="[[i18n:shell.nav.label]]">`)
	groups := []struct {
		titleKey string
		title    string
		advanced bool
		links    [][2]string // key, href
	}{
		{"shell.nav.group.core", "", false, [][2]string{{"shell.nav.library", "/"}, {"shell.nav.collections", "/collections"}, {"shell.nav.jobs", "/progress"}}},
		{"shell.nav.group.advanced", "", true, [][2]string{{"shell.nav.providers", "/providers"}, {"shell.nav.workers", "/workers"}, {"shell.nav.workerSetup", "/worker-setup"}, {"shell.nav.mediaFolders", "/library-roots"}, {"shell.nav.tags", "/tags"}, {"shell.nav.setup", "/setup"}, {"shell.nav.settings", "/settings"}}},
		{"", "Labs", false, [][2]string{{"shell.nav.repurpose", "/repurpose"}}},
	}
	for _, g := range groups {
		className := "nav-group"
		if g.advanced {
			className += " nav-group-advanced"
		} else if g.title == "Labs" {
			className += " nav-group-labs"
		}
		title := g.title
		if g.titleKey != "" {
			title = "[[i18n:" + g.titleKey + "]]"
		}
		b.WriteString(`<div class="` + className + `"><span class="nav-group-title">` + title + `</span>`)
		for _, link := range g.links {
			b.WriteString(`<a href="` + link[1] + `" data-nav="` + link[1] + `" class="nav-link">[[i18n:` + link[0] + `]]</a>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</nav><div class="shell-actions"><div class="shell-locale"><label for="shell-locale" class="visually-hidden">[[i18n:shell.locale.label]]</label><select id="shell-locale" aria-label="[[i18n:shell.locale.label]]" onchange="changeLocale(this)">`)
	for _, opt := range shellLocaleOptions {
		selected := ""
		if opt.loc == loc {
			selected = ` selected`
		}
		b.WriteString(`<option value="` + string(opt.loc) + `" data-locale="` + string(opt.loc) + `"` + selected + `>` + opt.label + `</option>`)
	}
	b.WriteString(`</select></div><div class="shell-auth"><input id="admin-token" type="password" autocomplete="off" placeholder="[[i18n:shell.auth.tokenPlaceholder]]" aria-label="[[i18n:shell.auth.tokenPlaceholder]]"><button type="button" class="btn btn--primary btn--sm" id="admin-login" onclick="loginAdmin()">[[i18n:shell.auth.login]]</button><button type="button" class="btn btn--sm" id="admin-logout" onclick="logoutAdmin()" hidden>[[i18n:shell.auth.logout]]</button><span id="admin-session-state" class="shell-auth-state">[[i18n:shell.auth.notLoggedIn]]</span></div><div class="status-strip" data-status-strip aria-label="[[i18n:shell.status.label]]">`)
	cells := []struct {
		id, titleKey, labelKey string
	}{
		{"status-hub", "shell.status.hub.title", "shell.status.hub.label"},
		{"status-pipeline", "shell.status.pipeline.title", "shell.status.pipeline.label"},
		{"status-providers", "shell.status.providers.title", "shell.status.providers.label"},
		{"status-workers", "shell.status.workers.title", "shell.status.workers.label"},
		{"status-access", "shell.status.access.title", "shell.status.access.label"},
	}
	for _, c := range cells {
		b.WriteString(`<a class="status-cell" data-` + c.id + ` href="` + cellHref(c.id) + `" title="[[i18n:` + c.titleKey + `]]"><i class="dot"></i><span class="k">[[i18n:` + c.labelKey + `]]</span><b id="` + c.id + `">…</b></a>`)
	}
	b.WriteString(`</div></div></aside>`)
	return b.String()
}

// shellLocaleOptions are the self-identifying native names shown in the
// selector; they are intentionally not translated (a Japanese reader must be
// able to find 日本語 regardless of the current UI language).
var shellLocaleOptions = []struct {
	loc   locale
	label string
}{
	{localeZhCN, "简体中文"},
	{localeJaJP, "日本語"},
	{localeEnUS, "English (US)"},
	{localeFrFR, "Français"},
	{localeEsES, "Español"},
}

func cellHref(id string) string {
	switch id {
	case "status-pipeline":
		return "/progress"
	case "status-providers":
		return "/providers"
	case "status-workers":
		return "/workers"
	case "status-access":
		return "/settings"
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
:root{
  color-scheme:light dark;

  /* —— 字体：本地优先工具，禁用任何网络字体。三个角色。 —— */
  --font-ui:"Inter","SF Pro Text",-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Hiragino Sans GB","Hiragino Kaku Gothic ProN","Yu Gothic","Meiryo","Microsoft YaHei","Noto Sans CJK SC","Noto Sans CJK JP","Source Han Sans SC","Source Han Sans JP",sans-serif;
  --font-data:ui-monospace,"SF Mono","JetBrains Mono","Cascadia Mono","Roboto Mono",Menlo,Consolas,"PingFang SC","Hiragino Sans","Yu Gothic UI","Meiryo","Microsoft YaHei",monospace;

  --fs-display:32px; --fs-title:20px; --fs-sub:15px; --fs-body:14px;
  --fs-small:13px; --fs-label:11px; --fs-data:13px; --fs-metric:28px;
  --lh-cjk:1.75; --lh-latin:1.5; --lh-tight:1.35;

  /* —— 间距 / 形状 —— */
  --s1:4px;--s2:8px;--s3:12px;--s4:16px;--s5:24px;--s6:32px;--s7:48px;
  --r-sm:6px;--r-md:10px;--r-lg:14px;--r-pill:999px;
  --control-h:36px; --rail-w:240px; --content-max:1220px;

  /* —— 浅色：场记单 —— */
  --bg:#F4F4F1; --surface:#FFFFFF; --raised:#FAFAF8; --inset:#EDEDE8;
  --rule:#E2E2DC; --rule-strong:#C6C6BE; --graticule:#E9E9E3;
  --text:#17191B; --text-muted:#5B6469; --text-faint:#8C9298;

  --ev-confirmed:#0E7A50; --ev-attention:#8A5600; --ev-contradicted:#BE332A;
  --ev-draft:#5B45C7;      --ev-unknown:#8C9298;
  --ev-confirmed-wash:#E3F3EB; --ev-attention-wash:#FAEFDA; --ev-contradicted-wash:#FBE7E5;
  --ev-draft-wash:#ECE8FB;     --ev-unknown-wash:#EDEDE8;

  --brand:#CF1B57;
  --btn-solid-bg:#1B1E20; --btn-solid-fg:#FFFFFF;
  --nav-active:#EFEFEA; --span-fill:#E7E7E2;
  --shadow-sm:0 1px 2px rgba(18,20,22,.07);
  --shadow-lg:0 2px 4px rgba(18,20,22,.05),0 14px 34px rgba(18,20,22,.09);
}
@media (prefers-color-scheme:dark){
  :root{
    /* —— 深色：监视器。中性灰，不带色偏——界面不能污染对画面的判断。 —— */
    --bg:#101113; --surface:#17191C; --raised:#1E2125; --inset:#0B0C0E;
    --rule:#282C31; --rule-strong:#3A4046; --graticule:#212529;
    --text:#EDEEF0; --text-muted:#98A0A8; --text-faint:#6A7178;

    --ev-confirmed:#3FD08A; --ev-attention:#F0A835; --ev-contradicted:#FF6259;
    --ev-draft:#A78BFA;      --ev-unknown:#767E86;
    --ev-confirmed-wash:#12291F; --ev-attention-wash:#2E2312; --ev-contradicted-wash:#331A19;
    --ev-draft-wash:#221E33;     --ev-unknown-wash:#1C1F22;

    --brand:#FF4D7D;
    --btn-solid-bg:#EDEEF0; --btn-solid-fg:#101113;
    --nav-active:#23272B; --span-fill:#2A2E33;
    --shadow-sm:0 1px 2px rgba(0,0,0,.4);
    --shadow-lg:0 2px 4px rgba(0,0,0,.3),0 14px 34px rgba(0,0,0,.45);
  }
}

/* —— 基础 —— */
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font-family:var(--font-ui);font-size:var(--fs-body);line-height:var(--lh-cjk);-webkit-font-smoothing:antialiased}
a{color:var(--text);text-decoration:none}
a:hover{color:var(--brand)}
h1,h2,h3{margin:0;line-height:var(--lh-tight);font-weight:700}
p{margin:0}
:where(a,button,input,select,textarea,summary,[tabindex]):focus-visible{outline:2px solid var(--brand);outline-offset:2px;border-radius:var(--r-sm)}
/* [hidden] 只靠 UA 样式表的 display:none 生效，而 author 规则不分特异性一律压过 UA
   来源。下面的 button/.btn/.state/.tag 都设了 display，会把所有 hidden 元素重新显示出来
   （侧栏「登录」和「退出」同时出现就是这么来的）。全仓库有 33 处 hidden 属性用法，
   只有两个页面自己声明了 [hidden]，所以这条必须在共享层。 */
[hidden]{display:none!important}
.visually-hidden{position:absolute;width:1px;height:1px;margin:-1px;padding:0;border:0;clip-path:inset(50%);overflow:hidden;white-space:nowrap}
@media(prefers-reduced-motion:reduce){*,*::before,*::after{animation-duration:.01ms!important;animation-iteration-count:1!important;transition-duration:.01ms!important;scroll-behavior:auto!important}}

/* —— 标签体：所有元数据、状态名、栏目名都走等宽小型大写。界面的性格在这里。 —— */
.label{font-family:var(--font-data);font-size:var(--fs-label);font-weight:600;letter-spacing:.14em;text-transform:uppercase;color:var(--text-faint)}
.data{font-family:var(--font-data);font-size:var(--fs-data);font-variant-numeric:tabular-nums;letter-spacing:0}
.muted{color:var(--text-muted)}
.faint{color:var(--text-faint)}

/* ============================ 侧栏 ============================ */
.shell-sidebar{position:fixed;left:0;top:0;bottom:0;width:var(--rail-w);z-index:30;background:var(--surface);border-right:1px solid var(--rule);display:flex;flex-direction:column;padding:var(--s5) 0 var(--s4);gap:var(--s5);overflow-y:auto}
body{padding-left:var(--rail-w)}
.shell-brand-row{padding:0 var(--s5);display:flex;flex-direction:column;gap:2px}
.brand{font-family:var(--font-data);font-size:15px;font-weight:700;letter-spacing:.02em;color:var(--text)}
.brand i{font-style:normal;color:var(--brand)}
.shell-tagline{font-family:var(--font-data);font-size:10px;font-weight:600;letter-spacing:.16em;text-transform:uppercase;color:var(--text-faint)}
.shell-nav{display:flex;flex-direction:column;gap:var(--s5)}
.nav-group{display:flex;flex-direction:column;gap:1px}
.nav-group-title{display:flex;align-items:center;gap:var(--s2);margin:0 var(--s5) var(--s2);font-family:var(--font-data);font-size:var(--fs-label);font-weight:600;letter-spacing:.14em;text-transform:uppercase;color:var(--text-faint);white-space:nowrap}
.nav-group-title::after{content:"";flex:1;height:1px;background:var(--rule)}
/* 刻度：当前页是导轨上唯一被点亮的一格 */
.nav-link{position:relative;display:block;padding:7px var(--s5);font-size:var(--fs-small);color:var(--text-muted);border-left:2px solid transparent}
.nav-link:hover{color:var(--text);background:var(--raised)}
.nav-link.active,.nav-link[aria-current="page"]{color:var(--text);font-weight:600;border-left-color:var(--brand);background:var(--nav-active)}
.shell-actions{margin-top:auto;display:flex;flex-direction:column;gap:var(--s4);padding:0 var(--s5)}
/* 状态：仪表读数，四行定宽，永不换行 */
.status-strip{display:flex;flex-direction:column;border-top:1px solid var(--rule);padding-top:var(--s3)}
.status-cell{display:flex;align-items:center;gap:var(--s2);padding:5px 0;font-family:var(--font-data);font-size:12px;color:var(--text-faint);white-space:nowrap}
.status-cell .k{letter-spacing:.1em;text-transform:uppercase;font-size:var(--fs-label)}
.status-cell b{margin-left:auto;font-weight:500;color:var(--text-muted);font-variant-numeric:tabular-nums}
.status-cell:hover b{color:var(--text)}
.dot{width:6px;height:6px;border-radius:50%;background:var(--ev-unknown);flex:none}
.dot.ok{background:var(--ev-confirmed)}
.dot.warn{background:var(--ev-attention)}
.dot.err{background:var(--ev-contradicted)}
.dot.off{background:transparent;box-shadow:inset 0 0 0 1px var(--ev-unknown)}
/* Latin UI locales (en-US, fr-FR, es-ES) read better at a tighter body
   line-height; the CJK locales (zh-CN, ja-JP) keep the taller CJK spacing. */
body:lang(en),body:lang(fr),body:lang(es){line-height:var(--lh-latin)}
.shell-locale{display:flex;align-items:center}
.shell-locale select{height:28px;width:auto;max-width:150px;padding:0 26px 0 8px;font-family:var(--font-data);font-size:12px}
.shell-auth{display:flex;flex-wrap:wrap;gap:var(--s2);align-items:center}
.shell-auth input{flex:1 1 100%}
.shell-auth-state{font-family:var(--font-data);font-size:var(--fs-label);letter-spacing:.1em;text-transform:uppercase;color:var(--text-faint)}

/* ============================ 页面骨架 ============================ */
.wrap{max-width:var(--content-max);margin:0 auto;padding:var(--s6) var(--s6) 96px}
.pagehead{display:flex;align-items:flex-end;gap:var(--s5);flex-wrap:wrap;padding-bottom:var(--s4);margin-bottom:var(--s5);border-bottom:1px solid var(--rule)}
.pagehead-text{flex:1 1 420px;min-width:0}
.pagehead .eyebrow{display:block;margin-bottom:var(--s2);font-family:var(--font-data);font-size:var(--fs-label);font-weight:600;letter-spacing:.14em;text-transform:uppercase;color:var(--brand)}
.pagehead h1{font-size:var(--fs-display);font-weight:800;letter-spacing:.01em}
.pagehead p{margin-top:var(--s2);color:var(--text-muted);font-size:var(--fs-sub);max-width:76ch}
.pagehead-actions{display:flex;gap:var(--s2);flex-wrap:wrap}

/* ============================ 面板 ============================ */
.panel{background:var(--surface);border:1px solid var(--rule);border-radius:var(--r-md);margin-bottom:var(--s4)}
.panel-head{display:flex;align-items:center;gap:var(--s3);flex-wrap:wrap;padding:var(--s4) var(--s5);border-bottom:1px solid var(--rule)}
.panel-head h2{flex:1 1 auto;font-size:var(--fs-title);font-weight:700}
.panel-head .label{margin-left:auto}
.panel-body{padding:var(--s5)}
.panel-body>p+p,.panel-body>*+.hint{margin-top:var(--s3)}
.hint{color:var(--text-muted);font-size:var(--fs-small)}
.grid2{display:grid;grid-template-columns:repeat(auto-fit,minmax(320px,1fr));gap:var(--s4);align-items:start}

/* —— 搜索条 —— */
.searchbar{display:flex;gap:var(--s2);margin-bottom:var(--s4)}
.searchbar input{flex:1 1 260px;min-width:0}
@media(max-width:640px){.searchbar{flex-wrap:wrap}.searchbar input{flex:1 1 100%}}

/* ============================ 按钮 ============================ */
/* 界面本身是中性的：主按钮用高对比中性色，颜色只留给状态。 */
.btn{display:inline-flex;align-items:center;justify-content:center;gap:var(--s2);height:var(--control-h);padding:0 14px;border:1px solid var(--rule-strong);border-radius:var(--r-sm);background:var(--surface);color:var(--text);font-family:var(--font-ui);font-size:var(--fs-small);font-weight:600;line-height:1;cursor:pointer;white-space:nowrap;transition:background .12s,border-color .12s,color .12s}
.btn:hover{border-color:var(--text-faint);background:var(--raised)}
.btn--primary{background:var(--btn-solid-bg);border-color:var(--btn-solid-bg);color:var(--btn-solid-fg)}
.btn--primary:hover{background:var(--btn-solid-bg);border-color:var(--btn-solid-bg);opacity:.86}
.btn--ghost{border-color:transparent;background:transparent;color:var(--text-muted)}
.btn--ghost:hover{background:var(--inset);color:var(--text)}
.btn--danger{border-color:var(--ev-contradicted);color:var(--ev-contradicted);background:transparent}
.btn--danger:hover{background:var(--ev-contradicted-wash)}
.btn--sm{height:28px;padding:0 10px;font-size:12px}
.btn:disabled{opacity:.45;cursor:not-allowed}
.btn-row{display:flex;gap:var(--s2);flex-wrap:wrap;align-items:center}

/* ============================ 证据 / 状态 ============================ */
/* 五档权重，全站同一套。unknown 永远是灰的——「不知道」不是「没有」。 */
.state{display:inline-flex;align-items:center;gap:6px;max-width:100%;padding:3px 9px;border-radius:var(--r-pill);border:1px solid transparent;font-family:var(--font-data);font-size:12px;font-weight:600;line-height:1.4;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.state::before{content:"";width:6px;height:6px;border-radius:50%;background:currentColor;flex:none}
.state--confirmed{color:var(--ev-confirmed);background:var(--ev-confirmed-wash)}
.state--attention{color:var(--ev-attention);background:var(--ev-attention-wash)}
.state--contradicted{color:var(--ev-contradicted);background:var(--ev-contradicted-wash)}
.state--draft{color:var(--ev-draft);background:var(--ev-draft-wash);border-style:dashed;border-color:currentColor}
.state--unknown{color:var(--ev-unknown);background:transparent;border-style:dotted;border-color:var(--ev-unknown)}
.state--unknown::before{background:transparent;box-shadow:inset 0 0 0 1px currentColor}
.state--contradicted .strike{text-decoration:line-through}
.tag{display:inline-flex;align-items:center;max-width:220px;padding:2px 8px;border:1px solid var(--rule);border-radius:var(--r-sm);background:var(--inset);color:var(--text-muted);font-family:var(--font-data);font-size:12px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.tag-row{display:flex;gap:6px;flex-wrap:wrap}

/* ============================ 指标 ============================ */
.metrics{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:1px;background:var(--rule);border:1px solid var(--rule);border-radius:var(--r-md);overflow:hidden;margin-bottom:var(--s4)}
.metric{background:var(--surface);padding:var(--s4) var(--s5)}
.metric .k{display:block;font-family:var(--font-data);font-size:var(--fs-label);font-weight:600;letter-spacing:.12em;text-transform:uppercase;color:var(--text-faint)}
.metric .v{display:block;margin-top:6px;font-family:var(--font-data);font-size:var(--fs-metric);font-weight:600;font-variant-numeric:tabular-nums;line-height:1;letter-spacing:-.02em}
.metric.is-attention .v{color:var(--ev-attention)}
.metric.is-contradicted .v{color:var(--ev-contradicted)}
.metric.is-zero .v{color:var(--text-muted)}

/* ============================ 表格 ============================ */
.table{width:100%;border-collapse:collapse;font-size:var(--fs-small)}
.table th{padding:0 var(--s3) var(--s2);text-align:left;font-family:var(--font-data);font-size:var(--fs-label);font-weight:600;letter-spacing:.12em;text-transform:uppercase;color:var(--text-faint);border-bottom:1px solid var(--rule)}
.table td{padding:11px var(--s3);border-bottom:1px solid var(--graticule);vertical-align:middle}
.table tr:last-child td{border-bottom:0}
.table tbody tr:hover td{background:var(--raised)}
.table .num{font-family:var(--font-data);font-variant-numeric:tabular-nums;text-align:right}
.table-scroll{overflow-x:auto}

/* ============================ 表单 ============================ */
input,select,textarea{height:var(--control-h);width:100%;padding:0 10px;border:1px solid var(--rule-strong);border-radius:var(--r-sm);background:var(--surface);color:var(--text);font-family:var(--font-ui);font-size:var(--fs-small);line-height:normal}
textarea{height:auto;padding:9px 10px;line-height:var(--lh-cjk);resize:vertical}
input::placeholder,textarea::placeholder{color:var(--text-faint)}
input:hover,select:hover,textarea:hover{border-color:var(--text-faint)}
select{appearance:none;padding-right:30px;background-image:linear-gradient(45deg,transparent 50%,currentColor 50%),linear-gradient(135deg,currentColor 50%,transparent 50%);background-position:calc(100% - 16px) calc(50% - 1px),calc(100% - 11px) calc(50% - 1px);background-size:5px 5px,5px 5px;background-repeat:no-repeat}
input[type=checkbox]{width:16px;height:16px;accent-color:var(--brand)}
.field{display:flex;flex-direction:column;gap:6px;min-width:0}
.field>label{font-family:var(--font-data);font-size:var(--fs-label);font-weight:600;letter-spacing:.12em;text-transform:uppercase;color:var(--text-faint)}
.field .note{color:var(--text-muted);font-size:12px;line-height:1.65}
.form-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:var(--s4)}
.form-row{display:grid;grid-template-columns:200px minmax(0,1fr);gap:var(--s4);padding:var(--s4) 0;border-top:1px solid var(--graticule)}
.form-row:first-child{border-top:0;padding-top:0}
.form-row>label{padding-top:9px;font-weight:600;font-size:var(--fs-small)}
.form-row .note{margin-top:6px;color:var(--text-muted);font-size:12px;line-height:1.65}
.form-row .w-sm{max-width:170px}

/* ============================ 提示条 ============================ */
.callout{display:flex;align-items:center;gap:var(--s3);flex-wrap:wrap;padding:var(--s3) var(--s4);border:1px solid var(--rule);border-left-width:3px;border-radius:var(--r-sm);background:var(--raised);font-size:var(--fs-small);color:var(--text);margin-bottom:var(--s4)}
.callout>span{flex:1 1 320px;min-width:0}
.callout--confirmed{border-left-color:var(--ev-confirmed);background:var(--ev-confirmed-wash)}
.callout--attention{border-left-color:var(--ev-attention);background:var(--ev-attention-wash)}
.callout--contradicted{border-left-color:var(--ev-contradicted);background:var(--ev-contradicted-wash)}
.callout--draft{border-left-color:var(--ev-draft);background:var(--ev-draft-wash)}

/* ============================ 空态 ============================ */
.empty{display:flex;flex-direction:column;align-items:flex-start;gap:var(--s3);padding:var(--s6) var(--s5);border:1px dashed var(--rule-strong);border-radius:var(--r-md);color:var(--text-muted);font-size:var(--fs-small)}
.empty .label{color:var(--text-faint)}

/* ============================ 刻度：本设计的签名件 ============================ */
/* 一条带刻度的轨。凡是「整体中的一段」都用它：镜头时间轴、流水线链、
   任务进度、存储占用。它是量的，不是装饰。 */
.tickrule{display:flex;flex-direction:column;gap:6px;min-width:0}
.tickrule-track{position:relative;height:34px;border:1px solid var(--rule);border-radius:var(--r-sm);background-color:var(--surface);background-image:repeating-linear-gradient(90deg,var(--graticule) 0 1px,transparent 1px 48px);overflow:hidden}
.tickrule-track.is-thin{height:6px}
.tickrule-span{position:absolute;top:0;bottom:0;border-right:1px solid var(--surface);background:var(--span-fill);display:flex;align-items:center;padding:0 8px;overflow:hidden}
.tickrule-span>span{font-family:var(--font-data);font-size:11px;color:var(--text);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.tickrule-span.is-hit{background:var(--ev-confirmed-wash);box-shadow:inset 0 0 0 1px var(--ev-confirmed)}
.tickrule-span.is-possible{background:var(--ev-draft-wash);box-shadow:inset 0 0 0 1px var(--ev-draft)}
.tickrule-span:last-child{border-right:0}
.tickrule-scale{display:flex;justify-content:space-between;font-family:var(--font-data);font-size:11px;font-variant-numeric:tabular-nums;color:var(--text-faint)}
/* 流水线链：同一条轨，刻度是阶段 */
.chain{display:flex;align-items:stretch;border:1px solid var(--rule);border-radius:var(--r-sm);overflow:hidden}
.chain-step{flex:1;padding:9px var(--s3);border-right:1px solid var(--rule);background:var(--surface);min-width:0}
.chain-step:last-child{border-right:0}
.chain-step .k{display:block;font-family:var(--font-data);font-size:10px;letter-spacing:.12em;text-transform:uppercase;color:var(--text-faint);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.chain-step .v{display:block;margin-top:3px;font-family:var(--font-data);font-size:var(--fs-small);font-variant-numeric:tabular-nums;color:var(--text-muted)}
.chain-step.is-active{background:var(--raised);box-shadow:inset 2px 0 0 var(--brand)}
.chain-step.is-active .v{color:var(--text);font-weight:600}
.chain-step.is-done .v{color:var(--ev-confirmed)}
.chain-step.is-failed .v{color:var(--ev-contradicted)}

/* ============================ 素材卡 ============================ */
.assets{display:flex;flex-direction:column;gap:var(--s3)}
.asset{display:grid;grid-template-columns:168px minmax(220px,1fr) minmax(280px,1.4fr);gap:var(--s4);padding:var(--s4);background:var(--surface);border:1px solid var(--rule);border-radius:var(--r-md)}
.asset:hover{border-color:var(--rule-strong)}
.asset-thumb{aspect-ratio:16/9;border-radius:var(--r-sm);background:var(--inset);border:1px solid var(--rule);object-fit:cover;width:100%;display:flex;align-items:center;justify-content:center}
.asset-name{font-size:var(--fs-sub);font-weight:600;overflow-wrap:anywhere}
.asset-meta{margin-top:6px;display:flex;flex-wrap:wrap;gap:var(--s1) var(--s3);font-family:var(--font-data);font-size:12px;color:var(--text-faint)}
.asset-summary{margin-top:var(--s3);color:var(--text-muted);font-size:var(--fs-small)}
.session-head{display:flex;align-items:center;gap:var(--s3);margin:var(--s5) 0 var(--s3)}
.session-head .label{white-space:nowrap}
.session-head::after{content:"";flex:1;height:1px;background:var(--rule)}

/* ============================ 步骤 / 页签 / 键值 ============================ */
.steps{display:flex;gap:var(--s1);margin-bottom:var(--s5);flex-wrap:wrap}
.steps span{flex:1 1 140px;padding:var(--s2) var(--s3);border-top:2px solid var(--rule);font-family:var(--font-data);font-size:12px;color:var(--text-faint)}
.steps span.is-active{border-top-color:var(--brand);color:var(--text);font-weight:600}
.steps span.is-done{border-top-color:var(--ev-confirmed);color:var(--text-muted)}
.tabs{display:inline-flex;padding:2px;border:1px solid var(--rule);border-radius:var(--r-sm);background:var(--inset);gap:2px}
.tabs button{height:28px;padding:0 12px;border:0;border-radius:4px;background:transparent;color:var(--text-muted);font-family:var(--font-data);font-size:12px;font-weight:600;cursor:pointer}
.tabs button.is-active{background:var(--surface);color:var(--text);box-shadow:var(--shadow-sm)}
.kv{display:flex;gap:var(--s3);padding:7px 0;border-bottom:1px solid var(--graticule);font-size:var(--fs-small)}
.kv:last-child{border-bottom:0}
.kv .k{flex:none;width:130px;font-family:var(--font-data);font-size:var(--fs-label);letter-spacing:.1em;text-transform:uppercase;color:var(--text-faint);padding-top:2px}
.kv .v{flex:1;min-width:0;font-family:var(--font-data);overflow-wrap:anywhere}

/* ============================ 移动端 ============================ */
.shell-menu-toggle{display:none}
@media(max-width:900px){
  body{padding-left:0;overflow-x:hidden}
  .shell-sidebar{position:relative;width:auto;height:auto;flex-direction:row;flex-wrap:wrap;align-items:center;gap:var(--s3);padding:var(--s3) var(--s4);border-right:0;border-bottom:1px solid var(--rule);overflow:visible}
  .shell-brand-row{flex:1 1 auto;flex-direction:row;align-items:baseline;gap:var(--s3);padding:0;min-width:0}
  .shell-menu-toggle{display:inline-flex;margin-left:auto;height:32px;padding:0 12px;align-items:center;border:1px solid var(--rule-strong);border-radius:var(--r-sm);background:var(--surface);color:var(--text);font-family:var(--font-data);font-size:12px;cursor:pointer}
  .shell-nav,.shell-actions{display:none;flex:1 1 100%;width:100%}
  .shell-sidebar.open .shell-nav,.shell-sidebar.open .shell-actions{display:flex}
  .shell-nav{flex-direction:column;gap:var(--s4)}
  .shell-actions{margin-top:0;padding:0}
  .nav-link,.nav-group-title{padding-left:var(--s2);margin-left:0;margin-right:0}
  .wrap{padding:var(--s5) var(--s4) 64px}
  .pagehead{align-items:flex-start}
  :root{--fs-display:26px;--fs-title:18px}
  .asset{grid-template-columns:minmax(0,1fr)}
  .form-row{grid-template-columns:minmax(0,1fr);gap:var(--s2)}
  .form-row>label{padding-top:0}
  .kv{flex-direction:column;gap:2px}
  .kv .k{width:auto}
}

/* ===========================================================================
   兼容层 —— 把现存 11 个页面的旧类名接到上面的新配方上。
   它的存在只有一个理由：改 app_shell.go 一个文件，全站立刻换脸，
   页面 HTML 一行不动，18 个锚点补丁一个不碰。
   每迁完一个页面，就从这里删掉该页独有的旧类名；全部迁完时本节应当消失。
   映射依据是逐个核对过的实际用法，不是按名字猜的：
     · .danger 在 library-roots 是 div 警告框，不是按钮 —— 只映射 button.danger
     · .state 在 settings 是 div 容器，不是徽标        —— 只映射 span.state
   =========================================================================== */

/* —— 输入框：页面用了 input[type=…] 属性选择器（0,1,1），比裸 input（0,0,1）更强，
   不把类型列进来，规格里的输入框样式在设置页等页面根本不会生效。 —— */
:where(input[type=text],input[type=number],input[type=time],input[type=date],input[type=password],
input[type=search],input[type=email],input[type=url]){
  height:var(--control-h);padding:0 10px;border:1px solid var(--rule-strong);
  border-radius:var(--r-sm);background:var(--surface);color:var(--text);
  font-family:var(--font-ui);font-size:var(--fs-small);line-height:normal}
:where(input[type=checkbox]){width:16px;height:16px;accent-color:var(--brand)}

/* —— 面板 —— */
:where(.panel,.card,.coll-card,.binary-card,.node,.job,.plan-list-item,.candidate,
.revision-item,.auth,.console-hero,.timeline-card,.metric){
  background:var(--surface);border:1px solid var(--rule);border-radius:var(--r-md)}
:where(.panel h2,.plan-head h2){font-size:var(--fs-title);font-weight:700;letter-spacing:0}

/* —— 按钮：底色一律中性，颜色只留给状态 —— */
:where(button,.btn,a.button,.nav-btn,.copy-btn,.back-link){
  display:inline-flex;align-items:center;justify-content:center;gap:var(--s2);
  height:var(--control-h);padding:0 14px;border:1px solid var(--rule-strong);
  border-radius:var(--r-sm);background:var(--surface);color:var(--text);
  font-family:var(--font-ui);font-size:var(--fs-small);font-weight:600;line-height:1;
  cursor:pointer;white-space:nowrap;text-decoration:none}
:where(button:hover,.btn:hover,a.button:hover,.nav-btn:hover,.copy-btn:hover){
  border-color:var(--text-faint);background:var(--raised);color:var(--text)}
:where(button.primary,.btn.primary,.nav-btn.primary,a.button,button.approve,.btn.approve,
.hero-actions button.primary,.plan-command-bar .btn.primary){
  background:var(--btn-solid-bg);border-color:var(--btn-solid-bg);color:var(--btn-solid-fg)}
:where(button.primary:hover,.btn.primary:hover,.nav-btn.primary:hover,a.button:hover){
  background:var(--btn-solid-bg);border-color:var(--btn-solid-bg);color:var(--btn-solid-fg);opacity:.86}
:where(button.ghost,.btn.quiet,button.secondary,.nav-btn.secondary,button.arrow){
  border-color:transparent;background:transparent;color:var(--text-muted)}
:where(button.ghost:hover,.btn.quiet:hover,button.secondary:hover,.nav-btn.secondary:hover){
  background:var(--inset);color:var(--text)}
:where(button.danger,.btn.danger,button.reject){
  border-color:var(--ev-contradicted);color:var(--ev-contradicted);background:transparent}
:where(button.danger:hover,.btn.danger:hover,button.reject:hover){
  background:var(--ev-contradicted-wash);color:var(--ev-contradicted)}
:where(button.success,.btn.success){border-color:var(--ev-confirmed);color:var(--ev-confirmed);background:transparent}
:where(button:disabled,.btn:disabled){opacity:.45;cursor:not-allowed}

/* —— 徽标：五档证据。unknown 是灰虚线，永远不是红的。 —— */
:where(span.pill,span.state,.sum-pill,.fleet-pill,.health-pill){
  display:inline-flex;align-items:center;gap:6px;max-width:100%;padding:3px 9px;
  border:1px solid transparent;border-radius:var(--r-pill);background:transparent;
  color:var(--ev-unknown);border-style:dotted;border-color:var(--ev-unknown);
  font-family:var(--font-data);font-size:12px;font-weight:600;line-height:1.4;
  white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
:where(span.pill.ok,span.state.succeeded,.health-pill.ok,.pill.live,.pill.approved,.pill.compat-ok){
  color:var(--ev-confirmed);background:var(--ev-confirmed-wash);border-style:solid;border-color:transparent}
:where(span.pill.bad,span.state.failed,.health-pill.err,.pill.compat-bad){
  color:var(--ev-contradicted);background:var(--ev-contradicted-wash);border-style:solid;border-color:transparent}
:where(span.pill.warn,span.state.deferred,.pill.hold,.pill.compat-warn){
  color:var(--ev-attention);background:var(--ev-attention-wash);border-style:solid;border-color:transparent}
:where(span.state.running,.pill.draft){
  color:var(--ev-draft);background:var(--ev-draft-wash);border-style:dashed;border-color:currentColor}
:where(.health-pill.muted){color:var(--ev-unknown);background:transparent;border-style:dotted}

/* —— 图例圆点：是圆点，不是徽标 —— */
:where(.legend i){display:inline-block;width:6px;height:6px;padding:0;border:0;border-radius:50%;background:var(--text-faint)}
:where(.legend i:nth-child(2)){background:var(--ev-confirmed)}
:where(.legend i:nth-child(3)){background:var(--ev-draft)}

/* —— 标记：中性元数据，不表示状态 —— */
:where(span.chip,.tag){display:inline-flex;align-items:center;max-width:220px;padding:2px 8px;
  border:1px solid var(--rule);border-radius:var(--r-sm);background:var(--inset);
  color:var(--text-muted);font-family:var(--font-data);font-size:12px;
  white-space:nowrap;overflow:hidden;text-overflow:ellipsis}

/* —— 表格 —— */
:where(table,.health-table){width:100%;border-collapse:collapse;font-size:var(--fs-small)}
:where(th,.health-table th){padding:0 var(--s3) var(--s2);text-align:left;font-family:var(--font-data);
  font-size:var(--fs-label);font-weight:600;letter-spacing:.12em;text-transform:uppercase;
  color:var(--text-faint);border-bottom:1px solid var(--rule);background:transparent}
:where(td,.health-table td){padding:11px var(--s3);border-bottom:1px solid var(--graticule);vertical-align:middle}
:where(tr:last-child td){border-bottom:0}

/* —— 提示条 —— */
:where(.notice,.error,.auth-callout,.root-warning,.health-warn,.status.ok,.status.bad,
.hint.ok,.hint.bad,.tags-status,div.danger,.history-banner,.statusline){
  display:flex;align-items:center;gap:var(--s3);flex-wrap:wrap;padding:var(--s3) var(--s4);
  border:1px solid var(--rule);border-left-width:3px;border-radius:var(--r-sm);
  background:var(--raised);color:var(--text);font-size:var(--fs-small)}
:where(.status.ok,.hint.ok,.tags-status.ok){border-left-color:var(--ev-confirmed);background:var(--ev-confirmed-wash)}
:where(.status.bad,.hint.bad,.tags-status.bad,.error,div.danger){border-left-color:var(--ev-contradicted);background:var(--ev-contradicted-wash)}
:where(.auth-callout,.root-warning,.health-warn){border-left-color:var(--ev-attention);background:var(--ev-attention-wash)}

/* —— 空态 —— */
:where(.empty,.ui-empty,.coll-empty,.timeline-empty,.plan-list .empty){
  display:flex;flex-direction:column;align-items:flex-start;gap:var(--s3);
  padding:var(--s6) var(--s5);border:1px dashed var(--rule-strong);border-radius:var(--r-md);
  background:transparent;color:var(--text-muted);font-size:var(--fs-small)}

/* —— 向导步骤条（library-roots 与 worker-setup 逐字重复的那份） —— */
:where(.step-nav){display:flex;gap:var(--s1);flex-wrap:wrap;background:transparent;border:0;padding:0}
:where(.step-nav div){flex:1 1 140px;padding:var(--s2) var(--s3);border:0;border-top:2px solid var(--rule);
  border-radius:0;background:transparent;font-family:var(--font-data);font-size:12px;color:var(--text-faint)}
:where(.step-nav .active){border-top-color:var(--brand);color:var(--text);font-weight:600;background:transparent}

/* —— 旧版页头：统一成 .pagehead 的样子 —— */
:where(.top,.head,.repurpose-head){display:flex;align-items:flex-end;gap:var(--s5);flex-wrap:wrap;
  padding-bottom:var(--s4);margin-bottom:var(--s5);border-bottom:1px solid var(--rule)}
:where(.eyebrow){display:block;margin-bottom:var(--s2);font-family:var(--font-data);font-size:var(--fs-label);
  font-weight:600;letter-spacing:.14em;text-transform:uppercase;color:var(--brand)}
:where(h1,.top h1,.repurpose-head h1){font-size:var(--fs-display);font-weight:800;letter-spacing:.01em}

/* —— 旧版页面容器 —— */
:where(.wrap){max-width:var(--content-max);margin:0 auto;padding:var(--s6) var(--s6) 96px}
/* 注：页面里已经没有 <header> 元素了（grep 确认为 0），但各页 CSS 里仍留着
   header{} / header input{} 等规则——那是死代码，逐页清理时直接删。 */

`

// shellScriptBlock is the shared script every page carries. It defines only
// names no page declares, so it can be injected without clashing with a
// page's own script block. Browser mutations use the HttpOnly session and the
// visible CSRF cookie; the token field exists only long enough to establish a
// session.
const shellScriptBlock = `<script>
function getAdminToken(){const el=document.getElementById('admin-token');return el?el.value.trim():''}
function changeLocale(sel){const tag=sel&&sel.value;if(!tag)return;document.cookie='timingdex_locale='+encodeURIComponent(tag)+'; Path=/; Max-Age=31536000; SameSite=Lax';location.reload()}
// Mobile nav: the toggle collapses .shell-nav/.shell-actions until opened, so
// the ten nav links and status pills do not crowd every mobile page. Closing
// when a link is chosen restores the compact bar after navigation.
document.addEventListener('DOMContentLoaded',function(){const toggle=document.getElementById('shell-menu-toggle'),sidebar=document.querySelector('.shell-sidebar');if(!toggle||!sidebar)return;toggle.addEventListener('click',function(){const open=sidebar.classList.toggle('open');toggle.setAttribute('aria-expanded',String(open))});sidebar.addEventListener('click',function(e){if(e.target.closest('.nav-link')){sidebar.classList.remove('open');toggle.setAttribute('aria-expanded','false')}})});
function csrfToken(){const prefix='__Host-timingdex_csrf=';const item=document.cookie.split('; ').find(function(x){return x.indexOf(prefix)===0});return item?decodeURIComponent(item.slice(prefix.length)):''}
function shellAuthHeaders(base){const headers=new Headers(base||{});const csrf=csrfToken();if(csrf)headers.set('X-CSRF-Token',csrf);return headers}
let adminAuthMode='required';
function applyAdminAuthMode(){const auth=document.querySelector('.shell-auth');if(auth)auth.hidden=adminAuthMode!=='required';if(adminAuthMode==='required'){statusCell('status-access','ok',tdT('shell.auth.required'))}else if(adminAuthMode==='trusted_network'){statusCell('status-access','warn',tdT('shell.auth.trustedNetwork'))}else{statusCell('status-access','err',tdT('shell.auth.open'))}const callout=document.getElementById('admin-auth-callout');if(callout){if(adminAuthMode==='required'){callout.hidden=true}else if(adminAuthMode==='trusted_network'){callout.className='callout callout--attention';callout.hidden=false}else{callout.className='callout callout--contradicted';const s=callout.querySelector('span');if(s)s.textContent=tdT('shell.auth.openWarning');callout.hidden=false}}}
function shellSetAuthState(authenticated){if(adminAuthMode!=='required'){applyAdminAuthMode();return}const state=document.getElementById('admin-session-state'),input=document.getElementById('admin-token'),login=document.getElementById('admin-login'),logout=document.getElementById('admin-logout');if(state)state.textContent=authenticated?tdT('shell.auth.loggedIn'):tdT('shell.auth.notLoggedIn');if(input)input.hidden=authenticated;if(login)login.hidden=authenticated;if(logout)logout.hidden=!authenticated}
async function loginAdmin(){const token=getAdminToken();if(!token){shellSetAuthState(false);return}const button=document.getElementById('admin-login');if(button)button.disabled=true;try{const r=await fetch('/api/v1/auth/admin/session',{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json'},body:JSON.stringify({token:token})});if(!r.ok)throw Error(tdT('shell.auth.loginFailed'));const input=document.getElementById('admin-token');if(input)input.value='';shellSetAuthState(true);window.dispatchEvent(new CustomEvent('timingdex:admin-auth-changed',{detail:{authenticated:true}}));refreshStatus()}catch(e){const state=document.getElementById('admin-session-state');if(state)state.textContent=e.message}finally{if(button)button.disabled=false}}
 async function logoutAdmin(){try{await fetch('/api/v1/auth/admin/session',{method:'DELETE',credentials:'same-origin',headers:shellAuthHeaders()})}finally{shellSetAuthState(false);window.dispatchEvent(new CustomEvent('timingdex:admin-auth-changed',{detail:{authenticated:false}}));refreshStatus()}}
function statusCell(id,state,text){const el=document.getElementById(id);if(!el)return;el.textContent=text;const cell=el.closest('.status-cell');if(cell){const dot=cell.querySelector('.dot');if(dot)dot.className='dot '+state}}
// The old per-page headers marked the current page with nav-active; the
// shell header derives it from the URL instead of taking an argument, so a
// page can never forget to pass it. Runs at end of body, so the DOM is ready.
document.querySelectorAll('.nav-link').forEach(function(a){if(a.getAttribute('href')===location.pathname){a.classList.add('active');a.setAttribute('aria-current','page')}});
async function refreshStatus(){
  try{const r=await fetch('/api/v1/health');statusCell('status-hub',r.ok?'ok':'err',r.ok?tdT('shell.status.ok'):tdT('shell.status.err'))}catch(e){statusCell('status-hub','err',tdT('shell.status.err'))}
  try{const s=await fetch('/api/v1/jobs/summary',{credentials:'same-origin',headers:shellAuthHeaders()}).then(r=>r.ok?r.json():null);if(s){let state='idle',text=tdT('shell.status.idle');if(s.running>0){state='ok';text=tdPlural('shell.status.running',s.running)}else if(s.deferred>0){state='warn';text=tdPlural('shell.status.deferred',s.deferred)}else if((s.failed||0)+(s.terminal||0)>0){state='err';text=tdPlural('shell.status.failed',(s.failed+s.terminal))}else if(s.pending>0){text=tdPlural('shell.status.pending',s.pending)}statusCell('status-pipeline',state,text)}else{statusCell('status-pipeline','off','—')}}catch(e){statusCell('status-pipeline','off','—')}
  try{const r=await fetch('/api/v1/admin/provider-channels/status',{headers:shellAuthHeaders()});if(r.ok){const caps=await r.json();const list=Array.isArray(caps)?caps:[];const anyData=list.some(c=>c&&c.has_runtime_data);let okN=0,degradedN=0;list.forEach(function(c){if(!c||!c.has_runtime_data||!c.snapshot||!Array.isArray(c.snapshot.channels))return;c.snapshot.channels.forEach(function(ch){if(!ch||!ch.enabled)return;if(ch.available)okN++;else degradedN++})});if(!anyData||(okN+degradedN)===0){statusCell('status-providers','off',tdT('shell.status.notConfigured'))}else if(degradedN>0){statusCell('status-providers','warn',tdPlural('shell.status.degraded',degradedN))}else{statusCell('status-providers','ok',tdPlural('shell.status.okCount',okN))}}else{statusCell('status-providers','err',tdT('shell.status.err'))}}catch(e){statusCell('status-providers','off','—')}
  try{const w=await fetch('/api/v1/hub/workers',{headers:shellAuthHeaders()}).then(r=>r.ok?r.json():null);if(Array.isArray(w)){const on=w.filter(x=>x.status==='online').length;const off=w.length-on;statusCell('status-workers',off>0?'warn':'ok',tdPlural('shell.status.online',on)+(off?' / '+tdPlural('shell.status.offline',off):''))}else{statusCell('status-workers','off','—')}}catch(e){statusCell('status-workers','off','—')}
}
fetch('/api/v1/setup/status').then(function(r){return r.ok?r.json():null}).then(function(s){if(!s)return;adminAuthMode=s.admin_auth||'required';applyAdminAuthMode()}).catch(function(){});
fetch('/api/v1/auth/admin/session',{credentials:'same-origin'}).then(function(r){shellSetAuthState(r.ok);window.dispatchEvent(new CustomEvent('timingdex:admin-auth-changed',{detail:{authenticated:r.ok}}))}).catch(function(){shellSetAuthState(false);window.dispatchEvent(new CustomEvent('timingdex:admin-auth-changed',{detail:{authenticated:false}}))});refreshStatus();setInterval(refreshStatus,15000);
</script>`
