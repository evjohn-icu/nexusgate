package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// quotedLabels pairs a copy string that tells the operator to click something
// with the key holding that control's actual label. Each pair is one sentence
// naming one button.
//
// Guidance copy that says «then click "X"» is only useful while X is what the
// button says. Nothing else checks that: the two strings live in the same file
// but are written at different times, and five languages are translated by five
// people who each render the quoted label independently. When this test was
// written, three of the five languages had already drifted — en-US and fr-FR
// quoted a label for the verify button that the button had never carried, and
// es-ES did it in both of its strings. Every one of those files was otherwise
// valid, complete and green.
//
// The comparison is containment rather than equality because the copy embeds
// the label in a sentence, and the label is compared with its trailing arrow
// removed: the arrow is decoration on the button, not part of what a person
// would read back.
var quotedLabels = []struct{ copyKey, labelKey string }{
	{"roots.noTerminalHandoff", "roots.verifyDone"},
	{"roots.mountpointManualHint", "roots.verifyDone"},
	{"roots.mountFailedNext", "roots.recheck"},
}

func TestRootsCopyQuotesTheLabelTheButtonActuallyCarries(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	localesDir := filepath.Join(filepath.Dir(testFile), "locales")

	checked := 0
	for _, loc := range supportedLocales {
		data, err := os.ReadFile(filepath.Join(localesDir, string(loc)+".json"))
		if err != nil {
			t.Fatalf("read catalog %s: %v", loc, err)
		}
		var catalog map[string]string
		if err := json.Unmarshal(data, &catalog); err != nil {
			t.Fatalf("decode catalog %s: %v", loc, err)
		}
		for _, pair := range quotedLabels {
			copyText, ok := catalog[pair.copyKey]
			if !ok {
				t.Errorf("%s: %q is missing", loc, pair.copyKey)
				continue
			}
			label, ok := catalog[pair.labelKey]
			if !ok {
				t.Errorf("%s: %q is missing, so %q quotes a button that has no label", loc, pair.labelKey, pair.copyKey)
				continue
			}
			label = strings.TrimSpace(strings.ReplaceAll(label, "→", ""))
			if label == "" {
				t.Errorf("%s: %q is empty once the arrow is removed, so the check below asserts nothing", loc, pair.labelKey)
				continue
			}
			if !strings.Contains(copyText, label) {
				t.Errorf("%s: %q tells the operator to click a button, but does not quote the label\n%q actually carries. The operator would look for a control that is not on the page.\n  button: %s\n  copy:   %s", loc, pair.copyKey, pair.labelKey, label, copyText)
			}
			checked++
		}
	}

	// Positive control. An empty locale list, or a catalog that decoded to
	// nothing, would satisfy every assertion above without comparing anything.
	if want := len(supportedLocales) * len(quotedLabels); checked != want {
		t.Fatalf("compared %d pairs, want %d — a locale or a key did not load, so a passing\nresult here would not mean the labels agree", checked, want)
	}
}
