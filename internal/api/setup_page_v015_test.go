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
// page's JavaScript — string assertions on the served body cover both. The
// copy itself is marker-driven: the raw source carries [[i18n:setup.*]]
// markers and tdT calls so the served page localizes with the request.
func TestSetupWizardPageServesFirstRunWizard(t *testing.T) {
	response := httptest.NewRecorder()
	service := providerChannelTestService(t, "setup-wizard-page.db")
	NewServer("", service).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/setup", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, marker := range []string{
		`fetch('/api/v1/setup/status')`,
		`href="/library-roots"`,
		`href="/providers"`,
		"setupLoadStatus",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("setup wizard missing %q", marker)
		}
	}
	for _, marker := range []string{
		"[[i18n:setup.title]]",
		"[[i18n:setup.subtitle]]",
		"[[i18n:setup.intro]]",
		"[[i18n:setup.section.env]]",
		"[[i18n:setup.section.roots]]",
		"[[i18n:setup.section.providers]]",
		"[[i18n:setup.note.readOnly]]",
		"[[i18n:setup.note.security]]",
	} {
		if !strings.Contains(setupHTML, marker) {
			t.Fatalf("setup wizard source missing %q", marker)
		}
	}
}

// The status payload drives every panel: one env row per field (ExifTool
// marked optional), a done/not-done pill per panel, and the panel-4 mapping
// from next_step to its recommended action. These markers pin the wiring the
// rendered rows depend on: binary names stay verbatim data, labels resolve
// through [[i18n:setup.*]] markers, and every status string routes through
// tdT with the wire codes left as object keys.
func TestSetupWizardPageRendersEnvRowsAndNextStepMapping(t *testing.T) {
	page := setupHTML
	for _, marker := range []string{
		`id="env-ffmpeg"`, `id="env-ffprobe"`, `id="env-exiftool"`,
		`id="env-datadir"`, `id="env-cache"`, `id="env-disk"`, `id="env-db"`,
		`id="pill-env"`, `id="pill-roots"`, `id="pill-providers"`, `id="pill-status"`,
		"FFmpeg", "FFprobe", "ExifTool", // binary names are verbatim data
		"[[i18n:setup.env.dataDirWritable]]", "[[i18n:setup.env.cacheWritable]]",
		"[[i18n:setup.env.diskSpace]]", "[[i18n:setup.env.dbHealth]]", "[[i18n:setup.env.optional]]",
		"'setup.steps.addFootage.lead'", "'setup.steps.configureProviders.lead'",
		"'setup.steps.scanOrProcess.lead'", "'setup.steps.search.lead'", "'setup.steps.ready.lead'",
		"tdT('setup.status.fetchFailed')", // fetch failure must degrade to a retry hint, never a broken page
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("setup wizard page missing %q", marker)
		}
	}
}

// The endpoint this wizard reads is a trusted, unauthenticated status read:
// the fetch must not carry an admin token, and the 重新检查 button must reuse
// the same loader so a retry is one click. The button label is the shared
// refresh action so it stays consistent with the shell and the failure hint.
func TestSetupWizardStatusFetchIsUnauthenticatedAndRecheckable(t *testing.T) {
	page := setupHTML
	if !strings.Contains(page, `fetch('/api/v1/setup/status')`) {
		t.Fatalf("setup wizard must fetch the status endpoint")
	}
	if strings.Contains(page, `fetch('/api/v1/setup/status',{`) {
		t.Fatalf("setup wizard must not send credentials with the status read")
	}
	if !strings.Contains(page, `onclick="setupLoadStatus()">[[i18n:common.refresh]]`) {
		t.Fatalf("重新检查 must reuse the shared refresh label on the same loader button")
	}
}
