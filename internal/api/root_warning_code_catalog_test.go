package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestRootWarningCodesHaveCatalogEntries(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	apiDir := filepath.Dir(testFile)

	serviceSource, err := os.ReadFile(filepath.Join(apiDir, "..", "app", "service.go"))
	if err != nil {
		t.Fatalf("read service source: %v", err)
	}
	// This source scan catches literal Code: "root.*" assignments in service.go,
	// but cannot see dynamically assembled codes or codes emitted from other files.
	codeRE := regexp.MustCompile(`Code:\s*"(root\.[a-z_]+)"`)
	codes := make(map[string]struct{})
	for _, match := range codeRE.FindAllSubmatch(serviceSource, -1) {
		codes[string(match[1])] = struct{}{}
	}
	// Positive control: a broken regex must not make this coverage test vacuous.
	if len(codes) == 0 {
		t.Fatal("source scan found no root warning codes")
	}

	for _, loc := range supportedLocales {
		path := filepath.Join(apiDir, "locales", string(loc)+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read runtime catalog %s: %v", loc, err)
		}
		var catalog map[string]string
		if err := json.Unmarshal(data, &catalog); err != nil {
			t.Fatalf("decode runtime catalog %s: %v", loc, err)
		}
		for code := range codes {
			if _, ok := catalog["roots.warning."+code]; !ok {
				t.Errorf("runtime catalog %s lacks roots.warning.%s", loc, code)
			}
		}
	}

	fragmentData, err := os.ReadFile(filepath.Join(apiDir, "locales", "fragments", "roots.json"))
	if err != nil {
		t.Fatalf("read roots fragment: %v", err)
	}
	var fragment struct {
		Keys map[string]map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(fragmentData, &fragment); err != nil {
		t.Fatalf("decode roots fragment: %v", err)
	}
	for _, loc := range supportedLocales {
		catalog, ok := fragment.Keys[string(loc)]
		if !ok {
			t.Fatalf("roots fragment lacks locale %s", loc)
		}
		for code := range codes {
			if _, ok := catalog["roots.warning."+code]; !ok {
				t.Errorf("roots fragment locale %s lacks roots.warning.%s", loc, code)
			}
		}
	}
}

// TestShareNameWarningCodesReachTheShareNameAction is the second half of the
// coverage above, and it exists because the failure it catches is silent. A
// warning code the page's guideActionKey does not recognise does not render an
// error — it falls through to roots.noGuidanceAction, which tells the operator
// to change the MOUNT POINT. For a share-name refusal that advice is wrong in
// the specific way this wizard keeps being wrong: it hands the operator a
// problem with a field they cannot fix, and blames them for a value the page
// itself proposed. The family grows — root.share_name_unsupported_character
// was added the moment discovery started returning names like IPC$ — so the
// guard is written against the family rather than against a list someone has
// to remember to extend.
func TestShareNameWarningCodesReachTheShareNameAction(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	apiDir := filepath.Dir(testFile)

	serviceSource, err := os.ReadFile(filepath.Join(apiDir, "..", "app", "service.go"))
	if err != nil {
		t.Fatalf("read service source: %v", err)
	}
	codeRE := regexp.MustCompile(`Code:\s*"(root\.[a-z_]+)"`)
	var shareNameCodes []string
	for _, match := range codeRE.FindAllSubmatch(serviceSource, -1) {
		code := string(match[1])
		if strings.Contains(code, "share_name") {
			shareNameCodes = append(shareNameCodes, code)
		}
	}
	// Positive control, for the same reason the catalog test has one: a regex
	// that matches nothing would make this pass while checking nothing.
	if len(shareNameCodes) < 2 {
		t.Fatalf("source scan found %d share-name warning code(s), want at least the two service.go emits", len(shareNameCodes))
	}

	action := libraryRootsHTML
	start := strings.Index(action, "function guideActionKey(inspection)")
	if start < 0 {
		t.Fatal("library roots page has no guideActionKey")
	}
	end := strings.Index(action[start:], "\nfunction ")
	if end < 0 {
		t.Fatal("guideActionKey is not followed by another function; the slice below would cover the whole page")
	}
	body := action[start : start+end]

	for _, code := range shareNameCodes {
		if !strings.Contains(body, "'"+code+"'") {
			t.Errorf("guideActionKey does not branch on %s, so it falls through to roots.noGuidanceAction and blames the mount point", code)
		}
	}
}
