package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
