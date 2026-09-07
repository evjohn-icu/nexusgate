package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// UI copy for the roots page lives in two files, and only one of them ships:
// i18n.go embeds locales/*.json, which does not match locales/fragments/. The
// fragment is a hand-synced authoring copy — one file per page, holding all five
// languages — and editing one of the pair looks half-green, because two
// different tests read the two files. This test is the one that compares them,
// so a key added or reworded on one side and forgotten on the other is caught
// where it happens rather than as a raw key name rendered to an operator.
//
// The comparison is scoped to the roots.* prefix because that is what the
// fragment declares; the runtime catalog is the union of every page. Equality
// rather than mere presence is deliberate: the failure that motivated this was
// a translation that had drifted from its source, not a missing key.
func TestRootsFragmentMatchesRuntimeCatalog(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	localesDir := filepath.Join(filepath.Dir(testFile), "locales")

	fragmentData, err := os.ReadFile(filepath.Join(localesDir, "fragments", "roots.json"))
	if err != nil {
		t.Fatalf("read roots fragment: %v", err)
	}
	var fragment struct {
		Keys map[string]map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(fragmentData, &fragment); err != nil {
		t.Fatalf("decode roots fragment: %v", err)
	}

	compared := 0
	for _, loc := range supportedLocales {
		block, ok := fragment.Keys[string(loc)]
		if !ok {
			t.Fatalf("roots fragment declares no block for %s, which ships in supportedLocales", loc)
		}
		catalogData, err := os.ReadFile(filepath.Join(localesDir, string(loc)+".json"))
		if err != nil {
			t.Fatalf("read runtime catalog %s: %v", loc, err)
		}
		var catalog map[string]string
		if err := json.Unmarshal(catalogData, &catalog); err != nil {
			t.Fatalf("decode runtime catalog %s: %v", loc, err)
		}

		for key, want := range block {
			got, ok := catalog[key]
			if !ok {
				t.Errorf("%s: %q is in the authoring fragment but not in the shipped catalog, so the\npage renders the raw key name", loc, key)
				continue
			}
			if got != want {
				t.Errorf("%s: %q differs between the two files — the operator sees the catalog value:\n  catalog:  %s\n  fragment: %s", loc, key, got, want)
			}
			compared++
		}
		for key := range catalog {
			if !strings.HasPrefix(key, "roots.") {
				continue
			}
			if _, ok := block[key]; !ok {
				t.Errorf("%s: %q ships in the catalog but the authoring fragment does not carry it, so the\nnext edit made through the fragment will drop it", loc, key)
			}
		}
	}

	// Positive control: a locale list that came back empty, or a fragment whose
	// blocks decoded to nothing, would satisfy every assertion above in silence.
	if compared == 0 {
		t.Fatal("compared no keys at all — the fragment or the locale list did not load")
	}
}
