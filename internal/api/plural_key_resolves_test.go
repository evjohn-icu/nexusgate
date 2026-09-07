package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

var tdPluralCall = regexp.MustCompile(`tdPlural\('([a-zA-Z0-9_.]+)'`)

// Every tdPlural key must resolve in every catalog — and "resolve" includes the
// bare key, not only the .one/.other forms.
//
// This exists because eight of them did not, on every page of the app, in all
// five languages, and nothing went red. tdPlural derives key+".one"/".other" at
// RUNTIME, so a catalog holding only the bare key satisfies every test that
// checks referenced keys are present while the browser renders the literal
// string "shell.status.failed" in the status strip. It took opening the page in
// a real browser to see it, which is the point: a key assembled at runtime is
// invisible to a source-level check unless that check assembles it too.
//
// Chinese and Japanese have no plural distinction, so requiring .one/.other for
// keys like "{count} online" would mean five languages duplicating one sentence
// into two identical entries. The fallback to the bare key is what makes that
// unnecessary; this test is what keeps the fallback honest.
func TestEveryPluralKeyResolvesInEveryCatalog(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	dir := filepath.Dir(testFile)

	sources, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		body, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range tdPluralCall.FindAllStringSubmatch(string(body), -1) {
			keys[match[1]] = true
		}
	}
	// Positive control: the scan is worthless if it matched nothing, which is
	// exactly what a changed call spelling would cause.
	if len(keys) < 5 {
		t.Fatalf("found only %d tdPlural keys in the page sources; the scan is not matching real call sites", len(keys))
	}

	locales, err := filepath.Glob(filepath.Join(dir, "locales", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(locales) != 5 {
		t.Fatalf("expected five catalogs, found %d", len(locales))
	}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)

	for _, locale := range locales {
		body, err := os.ReadFile(locale)
		if err != nil {
			t.Fatal(err)
		}
		var catalog map[string]string
		if err := json.Unmarshal(body, &catalog); err != nil {
			t.Fatalf("%s: %v", locale, err)
		}
		name := strings.TrimSuffix(filepath.Base(locale), ".json")
		for _, key := range sorted {
			_, bare := catalog[key]
			_, other := catalog[key+".other"]
			_, one := catalog[key+".one"]
			if !bare && !other && !one {
				t.Errorf("%s: tdPlural(%q) resolves to nothing — the page will render the key name itself", name, key)
			}
			// A key that supplies .one without .other has no fallback for any
			// other plural rule, which is most of them.
			if one && !other && !bare {
				t.Errorf("%s: %q has .one but neither .other nor a bare form", name, key)
			}
		}
	}
}
