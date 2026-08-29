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
		// The pills track executable state, not row existence: healthy roots,
		// a runnable video route, canonical shots and index readiness drive
		// the pending/complete verdicts.
		"s.healthy_root_count", "s.provider_ready", "s.searchable_shot_count", "s.search_index_ready",
		"tdT('setup.roots.pending')", "tdT('setup.providers.pending')",
		"tdPlural('setup.processing.searchableShots'", "tdT('setup.processing.indexPending')", "tdT('setup.processing.indexReady')",
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
	if !strings.Contains(page, `class="btn" onclick="setupLoadStatus()">[[i18n:common.refresh]]`) {
		t.Fatalf("重新检查 must reuse the shared refresh label on the same .btn loader button")
	}
}

// The wizard's CTAs moved off the legacy .button class onto the shared .btn
// surface (primary for the first-run actions), and the roots/providers empty
// states were upgraded to three-part .empty blocks with a why line.
func TestSetupWizardMigratedButtonAndEmptyClasses(t *testing.T) {
	if strings.Contains(setupHTML, `class="button"`) {
		t.Fatal("setup wizard still uses the legacy .button class; migrate to .btn")
	}
	for _, want := range []string{
		`class="btn" onclick="setupLoadStatus()">[[i18n:common.refresh]]`,
		`class="btn btn--primary" href="/library-roots"`,
		`class="btn btn--primary" href="/providers"`,
		`'<div class="empty"><b>'`,
		`tdT('setup.roots.emptyWhy')`,
		`tdT('setup.providers.emptyWhy')`,
	} {
		if !strings.Contains(setupHTML, want) {
			t.Fatalf("setup wizard missing migrated marker %q", want)
		}
	}
}

// The roots/providers pills report executable state, not row existence: an
// added-but-unhealthy root and a saved-but-disabled/keyless channel stay
// pending with the same /library-roots and /providers actions, while the
// processing panel names canonical shots and index readiness. This pins the
// wiring between the new status fields and the guide's pending verdicts.
func TestSetupWizardPillsTrackExecutableState(t *testing.T) {
	page := setupHTML
	if !strings.Contains(page, `setupPill('pill-roots',healthyRootsN>0)`) {
		t.Fatalf("roots pill must track healthy_root_count, not root_count")
	}
	if !strings.Contains(page, `setupPill('pill-providers',!!s.provider_ready)`) {
		t.Fatalf("providers pill must track provider_ready, not provider_count")
	}
	// Added-but-unhealthy roots stay pending with the manage action.
	if !strings.Contains(page, `healthyRootsN===0){roots.innerHTML='<span class="bad">⚠</span>`) {
		t.Fatalf("unhealthy roots must render a pending state, not a done state")
	}
	if !strings.Contains(page, `href="/library-roots">'+esc(tdT('setup.roots.manage'))`) {
		t.Fatalf("unhealthy roots must keep the /library-roots action")
	}
	// Saved-but-not-ready channels stay pending with the manage action.
	if !strings.Contains(page, `!s.provider_ready){prov.innerHTML='<span class="bad">⚠</span>`) {
		t.Fatalf("saved-but-unready channels must render a pending state")
	}
	if !strings.Contains(page, `href="/providers">'+esc(tdT('setup.providers.manage'))`) {
		t.Fatalf("saved-but-unready channels must keep the /providers action")
	}
	// The processing panel names canonical shots and index readiness.
	if !strings.Contains(page, `s.next_step==='scan_or_process'||s.next_step==='search'`) {
		t.Fatalf("processing panel must appear for scan_or_process and search steps")
	}
	if !strings.Contains(page, `s.search_index_ready?esc(tdT('setup.processing.indexReady')):esc(tdT('setup.processing.indexPending'))`) {
		t.Fatalf("processing panel must render index readiness through the catalog keys")
	}
}
