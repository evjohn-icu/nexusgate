package api

import (
	"strings"
	"testing"
)

func TestLibraryRootsWizardTranslatesInspectionWarnings(t *testing.T) {
	page := libraryRootsHTML
	if !strings.Contains(page, `var warningHTML=rootWarningsHTML(inspection,'callout');`) {
		t.Fatal("wizard inspection warnings do not use the shared warning renderer")
	}
	if strings.Contains(page, `(inspection.warnings||[]).forEach(function(w)`) {
		t.Fatal("wizard still renders inspection warnings directly from the API")
	}
	if !strings.Contains(page, `tdT(rootWarningKeyPrefix+d.code,d.params||{})`) {
		t.Fatal("shared warning renderer does not look up warning details in the catalog")
	}
	// This string-presence test catches a missing wizard call site and raw
	// warning loop, but it cannot execute JavaScript or verify a rendered
	// locale; PageScripts covers syntax and browser behavior needs an end-to-end
	// test.
}
