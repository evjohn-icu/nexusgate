package api

import "strings"

// Re:Footage is the product-facing name. Timingdex remains the binary,
// configuration namespace and protocol name so existing libraries and Workers
// stay compatible while the UI makes the product promise more memorable.
const productName = "Re:Footage"
const productTagline = "Expired Footage, Reclaimed."

func brandedPage(page string) string {
	return strings.NewReplacer(
		"<title>Timingdex", "<title>"+productName,
		`<span class="brand">Timingdex · 素材库</span>`, `<span class="brand">`+productName+` · `+productTagline+`</span>`,
		`<span class="brand">Timingdex</span>`, `<span class="brand">`+productName+`</span>`,
	).Replace(page)
}
