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
	html := shellHeaderHTML()
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
