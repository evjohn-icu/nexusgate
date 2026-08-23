package api

import (
	"strings"
	"testing"
)

func TestServedPagesCarryBranding(t *testing.T) {
	pages := []string{libraryIndexHTML, progressHTML, providersHTML, settingsHTML, workersPageHTML, tagsHTML, repurposeWorkspaceHTML, libraryRootsHTML, setupHTML, workerSetupPageHTML}
	for _, page := range pages {
		served := brandedPage(shelledPage(page, localeZhCN))
		if !strings.Contains(served, `<span class="brand">Re<i>:</i>Footage</span>`) {
			t.Errorf("served page shows un-branded shell header (order bug): brand=%q", extractBrand(served))
		}
		if !strings.Contains(served, `data-app-shell`) {
			t.Errorf("served page missing shell header")
		}
	}
}

func extractBrand(page string) string {
	i := strings.Index(page, `<span class="brand">`)
	if i < 0 {
		return "(no brand span)"
	}
	j := strings.Index(page[i:], `</span>`)
	if j < 0 {
		return "(unclosed)"
	}
	return page[i : i+j+len(`</span>`)]
}
