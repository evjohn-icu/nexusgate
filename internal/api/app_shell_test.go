package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// shellScriptBlockRE isolates the JavaScript inside shellScriptBlock so it can
// be syntax-checked on its own. The block is a single Go string literal, so
// the anchor match cannot fail across a newline.
var shellScriptBlockRE = regexp.MustCompile(`(?s)^<script>(.*)</script>$`)

// The providers cell of the shared status strip read `(d&&d.channels)||[]`
// off GET /api/v1/admin/provider-channels/status, but that endpoint returns a
// JSON ARRAY of ProviderChannelCapabilityStatus objects (capability,
// has_runtime_data, has_route, snapshot), so ch was always empty and the strip
// permanently showed "未配置". These tests pin the fix: the branch must walk
// the array shape through has_runtime_data and snapshot.channels[].available,
// and the old d.channels object read must stay gone.
func TestShellStripProvidersParsesCapabilityArray(t *testing.T) {
	if !strings.Contains(shellScriptBlock, "has_runtime_data") {
		t.Error("shell providers branch does not reference has_runtime_data: capability statuses with no runtime data must not count as healthy")
	}
	if !strings.Contains(shellScriptBlock, "available") {
		t.Error("shell providers branch does not reference available: enabled channels are split into 正常/降级 by it")
	}
	if strings.Contains(shellScriptBlock, "d.channels") {
		t.Error("shell providers branch still reads d.channels: provider-channels/status returns an array of capability statuses, not an object with a channels key")
	}
	if !strings.Contains(shellScriptBlock, "provider-channels/status") {
		t.Error("shell providers branch no longer fetches provider-channels/status")
	}
}

// The four status cells are the contract the rest of the UI (and the parallel
// shell rework) depends on; a rename here must be deliberate.
func TestShellStripHasAllFourCells(t *testing.T) {
	html := shellHeaderHTML(localeZhCN)
	for _, id := range []string{"status-hub", "status-pipeline", "status-providers", "status-workers"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("strip is missing cell id %s", id)
		}
	}
}

// The shell script is served on every page, and the existing page test only
// catches a dead script if the page actually serves it; syntax-checking the
// constant directly catches the same class of mistake for the strip itself.
// Guarded by node being installed, exactly like TestPageScriptsParseWithNode.
func TestShellScriptParsesWithNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to syntax-check the inline shell script")
	}
	block := shellScriptBlockRE.FindStringSubmatch(shellScriptBlock)
	if block == nil {
		t.Fatal("shellScriptBlock does not match <script>...</script>")
	}
	path := filepath.Join(t.TempDir(), "shell-strip.js")
	if err := os.WriteFile(path, []byte(block[1]), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("shell script does not parse: %v\n%s", err, output)
	}
}

// The shared surface helpers define the one dialog/overlay contract: native
// <dialog> uses showModal/close, custom overlays toggle .open and aria-hidden
// and gain role="dialog"/aria-modal with an Escape + contained-Tab handler,
// and every surface restores focus to its opener on close. All the named
// openers (admin, Provider beginner/advanced, Repurpose new-plan/revision,
// Library filter/shot, add-to-collection) route through them, and shot-result
// cards are keyboard-activatable while ignoring nested controls.
func TestDialogFocusContract(t *testing.T) {
	// The helpers live in the shared shell script so every page can use them.
	if !strings.Contains(shellScriptBlock, "function tdOpenSurface(id,opener,initialFocusSelector)") {
		t.Fatal("shell must define tdOpenSurface")
	}
	if !strings.Contains(shellScriptBlock, "function tdCloseSurface(id)") {
		t.Fatal("shell must define tdCloseSurface")
	}
	// Native dialog path and custom-overlay path.
	if !strings.Contains(shellScriptBlock, "if(typeof d.showModal==='function'){d.showModal()}else{d.classList.add('open')") {
		t.Fatal("tdOpenSurface must use showModal for dialogs and .open for custom overlays")
	}
	if !strings.Contains(shellScriptBlock, "d.setAttribute('role','dialog');d.setAttribute('aria-modal','true')") {
		t.Fatal("custom overlays must gain role=dialog and aria-modal=true")
	}
	// Opener is remembered and restored on close.
	if !strings.Contains(shellScriptBlock, "tdSurfaceOpeners.set(id,opener)") || !strings.Contains(shellScriptBlock, "opener.focus();tdSurfaceOpeners.delete(id)") {
		t.Fatal("surfaces must store and restore their opener")
	}
	// Escape + contained Tab loop for open custom surfaces.
	if !strings.Contains(shellScriptBlock, "e.key==='Escape'") || !strings.Contains(shellScriptBlock, "[aria-modal=\"true\"][aria-hidden=\"false\"]") {
		t.Fatal("custom surfaces must get an Escape handler")
	}
	if !strings.Contains(shellScriptBlock, "if(e.shiftKey&&document.activeElement===first)") || !strings.Contains(shellScriptBlock, "last.focus()}else if(!e.shiftKey&&document.activeElement===last){e.preventDefault();first.focus()") {
		t.Fatal("custom surfaces must contain the Tab loop")
	}
	// Admin dialog migrated.
	if !strings.Contains(shellScriptBlock, "function openAdminDialog(opener)") || !strings.Contains(shellScriptBlock, "tdOpenSurface('admin-dialog',el,'#admin-token')") {
		t.Fatal("admin dialog must open through tdOpenSurface with #admin-token focus")
	}
	if !strings.Contains(shellScriptBlock, "function closeAdminDialog(){tdCloseSurface('admin-dialog')}") {
		t.Fatal("admin dialog must close through tdCloseSurface")
	}
	if !strings.Contains(shellHeaderHTML(localeZhCN), `id="admin-trigger" onclick="openAdminDialog(this)"`) {
		t.Fatal("admin trigger must pass itself as the surface opener")
	}

	// Provider beginner/advanced dialogs.
	if !strings.Contains(providersHTML, "function openDialog(id,opener,focus){tdOpenSurface(id,opener,focus)}") {
		t.Fatal("providers openDialog must delegate to tdOpenSurface")
	}
	if !strings.Contains(providersHTML, "openDialog('add-provider-dialog',document.getElementById('new-channel-btn'),'#wizard-capability')") {
		t.Fatal("provider wizard must open with its opener and focus the capability select")
	}
	if !strings.Contains(providersHTML, "openDialog('provider-dialog',document.getElementById('advanced-btn'),'#label')") {
		t.Fatal("advanced provider editor must open with its opener and focus the label")
	}

	// Repurpose new-plan / revision / shot dialogs.
	if !strings.Contains(repurposeWorkspaceHTML, "openDialog('new-plan-dialog',document.getElementById('new-plan'),'#brief')") {
		t.Fatal("repurpose new-plan dialog must open with its opener and focus the brief")
	}
	if !strings.Contains(repurposeWorkspaceHTML, "openDialog('revision-dialog',document.getElementById('revision-history'))") {
		t.Fatal("repurpose revision dialog must open with its opener")
	}

	// Library filter/shot drawers and add-to-collection modal.
	if !strings.Contains(libraryIndexHTML, "function openFilterDrawer(){tdOpenSurface('filter-drawer',document.getElementById('filters-toggle'),'select')}") {
		t.Fatal("library filter drawer must open through tdOpenSurface")
	}
	if !strings.Contains(libraryIndexHTML, "tdOpenSurface('shot-drawer',null,null);v.play()") {
		t.Fatal("library shot drawer must open through tdOpenSurface")
	}
	if !strings.Contains(libraryIndexHTML, "overlay.setAttribute('role','dialog');overlay.setAttribute('aria-modal','true');overlay.dataset.shot=encodeURIComponent(JSON.stringify(shot));tdOpenSurface('add-shot-modal',addShotSourceBtn,'input')") {
		t.Fatal("add-to-collection modal must open through tdOpenSurface with role/aria-modal")
	}
	// Custom overlays carry the dialog semantics in their static markup too.
	if !strings.Contains(libraryIndexHTML, `id="filter-drawer" data-filter-drawer role="dialog" aria-modal="true"`) {
		t.Fatal("filter drawer markup must carry role=dialog aria-modal")
	}
	if !strings.Contains(libraryIndexHTML, `id="shot-drawer" data-shot-drawer role="dialog" aria-modal="true"`) {
		t.Fatal("shot drawer markup must carry role=dialog aria-modal")
	}

	// Shot-result cards are keyboard-activatable while ignoring nested controls.
	if !strings.Contains(libraryIndexHTML, `role="button" tabindex="0" data-shot-result`) {
		t.Fatal("shot-result cards must be role=button with tabindex")
	}
	if !strings.Contains(libraryIndexHTML, `onkeydown="if(event.target===this&&(event.key===\'Enter\'||event.key===\' \'))`) {
		t.Fatal("shot-result cards must open on Enter/Space only when the card itself is the target")
	}
}

// The shell Labs group is localized through the catalog and its CSS class keys
// off the group's labs flag, never off rendered text: the sidebar must not
// carry a raw mixed-language "Labs" title that ignores the selected locale.
func TestShellLabsGroupLocalized(t *testing.T) {
	served := shellHeaderHTML(localeZhCN)
	if !strings.Contains(served, "[[i18n:shell.nav.group.labs]]") {
		t.Fatal("the Labs group title must resolve through the shell.nav.group.labs catalog key")
	}
	if !strings.Contains(served, `class="nav-group nav-group-labs"`) {
		t.Fatal("the Labs group must carry the nav-group-labs class from its labs flag")
	}
	if strings.Contains(served, ">Labs<") {
		t.Fatal("the shell must not carry a raw Labs title")
	}
	for _, loc := range supportedLocales {
		cat := catalogs[loc]
		if !cat.has("shell.nav.group.labs") {
			t.Fatalf("locale %s must carry shell.nav.group.labs", loc)
		}
	}
	// zh-CN renders the localized label in a served page.
	if !strings.Contains(serveShellHeaderFor(localeZhCN), "实验区") {
		t.Fatal("zh-CN shell must render the Labs group as 实验区")
	}
}

func serveShellHeaderFor(loc locale) string {
	page := shelledPage("<html><body><!--SHELL_HEADER--></body></html>", loc)
	page = catalogs[loc].resolveMarkers(page)
	return brandedPage(page)
}
