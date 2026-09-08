package api

import (
	"strings"
	"testing"
)

func TestLibraryRootsWizardHasRecoveryExits(t *testing.T) {
	page := libraryRootsHTML
	checks := []struct {
		name   string
		marker string
	}{
		{
			name:   "failed share mount blocks add",
			marker: `tdT('roots.mountFailedNext')`,
		},
		{
			name:   "unraid mount point hint",
			marker: `hint.textContent=tdT('roots.mountpointManualHint');`,
		},
		{
			name: "missing guidance recovery",
			// The empty-guidance exit still renders roots.noGuidanceAction; it is
			// now reached through a variable so a share-name refusal can
			// substitute its own text (see guideActionKey). Only the indirection
			// moved — the exit itself, and its fallback, are unchanged.
			marker: `esc(tdT(action.key,action.vars))`,
		},
		{
			// Pinned separately from the line above, because the indirection is
			// exactly what could lose the fallback without anything failing: a
			// guideActionKey that returned undefined for an unknown code would
			// render the empty string into the callout, and the wizard's last
			// exit would silently become a blank box.
			name:   "missing guidance fallback text",
			marker: "  return {key:'roots.noGuidanceAction'};\n}",
		},
		{
			name:   "no terminal handoff",
			marker: `esc(tdT('roots.noTerminalHandoff'))`,
		},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if !strings.Contains(page, check.marker) {
				t.Fatalf("library roots page missing exit marker %q", check.marker)
			}
		})
	}
	// The gate reads mountingShare, not inspection.is_share, and that difference
	// is the whole point: verifyMount inspects the MOUNT POINT, a local path, so
	// is_share on that response is always false and a gate built on it would be
	// dead code that silently never fires. mountingShare is set by
	// renderGuidance, which runs only when step 1 classified the operator's
	// input as a share.
	if !strings.Contains(page, `if(mountingShare&&inspection.looks_unmounted){`) {
		t.Fatal("the mount-failure add-button gate is missing, or was rebuilt on inspection.is_share,\nwhich is always false for the mount point verifyMount actually inspects")
	}
	if !strings.Contains(page, "mountingShare=true;") {
		t.Fatal("nothing sets mountingShare, so the gate above can never fire")
	}
	// The handoff offers to pass commands to someone with a terminal, so it must
	// appear only where commands exist. Unraid's guidance is a click sequence in
	// its own web UI and carries none.
	if !strings.Contains(page, `guidance.steps.some(function(s){return s.commands&&s.commands.length})`) {
		t.Fatal("the no-terminal handoff is not gated on the guidance actually carrying commands")
	}
	// A string-presence test catches a missing branch or translation lookup in
	// the page constant, but it cannot execute JavaScript or prove the rendered
	// HTML is visible in a browser; PageScripts catches syntax and an end-to-end
	// browser test would be needed for that behavior.
}

// A scan that outlives its request is only an improvement if the page stops
// calling the dropped connection a failed scan. json() attaches .status solely
// when an HTTP response arrived, so its absence is the discriminator: no
// answer came back, and since scanRoot detaches the walk from the request's
// context the scan is still running. The two branches must stay distinct —
// collapsing them is what made the wizard report failure for work that
// succeeds, on exactly the NAS libraries the wizard exists to set up.
func TestLibraryRootsScanSeparatesADroppedConnectionFromAFailedScan(t *testing.T) {
	page := libraryRootsHTML
	for _, marker := range []string{
		// The discriminator itself.
		"e.status===undefined",
		// The honest answer for a dropped connection...
		"roots.scanStillRunning",
		// ...and the real failure path, which must survive alongside it.
		"roots.scanFailed",
		// The wait is announced before the request, not after it returns.
		"roots.scanLongHint",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("scan feedback marker %q is missing; the page cannot tell a dropped connection from a failed scan without it", marker)
		}
	}
	// roots.scanLongHint has to be rendered BEFORE the fetch is awaited, or it
	// describes a wait the operator has already finished. Anchor on order
	// rather than presence: a hint set in the result handler would satisfy a
	// contains-check and show up only once there was nothing left to wait for.
	hint := strings.Index(page, "roots.scanLongHint")
	post := strings.Index(page, "/scan',{method:'POST'}")
	if hint < 0 || post < 0 || hint > post {
		t.Fatalf("roots.scanLongHint must be rendered before the scan POST is issued (hint=%d, post=%d)", hint, post)
	}
}
