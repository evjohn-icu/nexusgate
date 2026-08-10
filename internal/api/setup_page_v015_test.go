package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The setup page is a first-run wizard, not the old static three-step guide:
// it must carry the wizard panels, the read-only guarantee, the security
// note, and the CTA links that close the gap for a first-time operator. The
// CTAs are rendered by the page script, so the anchors live in the served
// page's JavaScript — string assertions on the served body cover both.
func TestSetupWizardPageServesFirstRunWizard(t *testing.T) {
	response := httptest.NewRecorder()
	service := providerChannelTestService(t, "setup-wizard-page.db")
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/setup", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, marker := range []string{
		"启动配置",
		"第一次使用向导",
		"环境检查",
		"素材目录",
		"模型能力",
		"Timingdex 从不写入原始素材。素材只读打开，派生文件放在缓存目录。",
		"页面不会生成含 API Key 的终端命令",
		`fetch('/api/v1/setup/status')`,
		`href="/library-roots"`,
		`href="/providers"`,
		"打开素材目录向导",
		"配置模型",
		"重新检查",
		"setupLoadStatus",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("setup wizard missing %q", marker)
		}
	}
}

// The status payload drives every panel: one env row per field (ExifTool
// marked optional), a done/not-done pill per panel, and the panel-4 mapping
// from next_step to its recommended action. These markers pin the wiring the
// rendered rows depend on.
func TestSetupWizardPageRendersEnvRowsAndNextStepMapping(t *testing.T) {
	page := setupHTML
	for _, marker := range []string{
		`id="env-ffmpeg"`, `id="env-ffprobe"`, `id="env-exiftool"`,
		`id="env-datadir"`, `id="env-cache"`, `id="env-disk"`, `id="env-db"`,
		`id="pill-env"`, `id="pill-roots"`, `id="pill-providers"`, `id="pill-status"`,
		"✓ Ready", "✗ Needs action", "⚠ Optional",
		"已完成", "待处理",
		`'add_footage'`, `'configure_providers'`, `'scan_or_process'`, `'search'`, `'ready'`,
		"下一步：添加素材目录", "下一步：配置模型", "下一步：扫描并处理素材", "可以去搜索素材了", "一切就绪",
		"无法连接状态接口", // fetch failure must degrade to a retry hint, never a broken page
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("setup wizard page missing %q", marker)
		}
	}
}

// The endpoint this wizard reads is a trusted, unauthenticated status read:
// the fetch must not carry an admin token, and the 重新检查 button must reuse
// the same loader so a retry is one click.
func TestSetupWizardStatusFetchIsUnauthenticatedAndRecheckable(t *testing.T) {
	page := setupHTML
	if !strings.Contains(page, `fetch('/api/v1/setup/status')`) {
		t.Fatalf("setup wizard must fetch the status endpoint")
	}
	if strings.Contains(page, `fetch('/api/v1/setup/status',{`) {
		t.Fatalf("setup wizard must not send credentials with the status read")
	}
	if !strings.Contains(page, `onclick="setupLoadStatus()"`) {
		t.Fatalf("重新检查 must re-run the same loader")
	}
}
