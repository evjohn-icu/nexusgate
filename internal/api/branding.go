package api

import "strings"

// Re:Footage is the product-facing name. Timingdex remains the binary,
// configuration namespace and protocol name so existing libraries and Workers
// stay compatible while the UI makes the product promise more memorable.
const productName = "Re:Footage"

// brandReplacements are the exact-match string replacements brandedPage
// applies to every served page. The anchors are the pre-branding text on the
// page constants; TestBrandedPageAnchorsBite requires every anchor to occur in
// at least one served page so a stale anchor fails the build instead of
// silently no-oping (the same guard TestLibraryPagePatchesApplyInOrderAndBite
// gives the library overlay).
var brandReplacements = []pagePatch{
	{anchor: "<title>Timingdex", replacement: "<title>" + productName},
	{anchor: `<span class="brand">Timingdex</span>`, replacement: `<span class="brand">` + strings.Replace(productName, ":", `<i>:</i>`, 1) + `</span>`},
}

func brandedPage(page string) string {
	var pairs []string
	for _, p := range brandReplacements {
		pairs = append(pairs, p.anchor, p.replacement)
	}
	return strings.NewReplacer(pairs...).Replace(page)
}
