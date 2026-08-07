package api

import (
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/normalize"
)

// enhanceLibraryPage is a chain of exact-match string replacements over a
// legacy constant, and every later anchor sits inside text an earlier
// replacement inserted. A stale anchor makes strings.Replace no-op silently:
// the page still builds and serves and the feature is just gone. This test
// replays the slice the way enhanceLibraryPage does and requires each anchor
// to match exactly once at the moment it is applied — zero is the silent
// no-op, two is an ambiguous patch. The replay must reproduce libraryIndexHTML
// exactly, so the test is coupled to the shipped page, not to data it built.
func TestLibraryPagePatchesApplyInOrderAndBite(t *testing.T) {
	page := legacyLibraryIndexHTML
	for i, patch := range libraryPagePatches {
		n := strings.Count(page, patch.anchor)
		if n != 1 {
			name := patch.anchor
			if len(name) > 80 {
				name = name[:80] + "…"
			}
			if n == 0 {
				t.Fatalf("patch %d: anchor not found (silent no-op): %q", i, name)
			}
			t.Fatalf("patch %d: anchor matches %d times (ambiguous patch): %q", i, n, name)
		}
		page = strings.Replace(page, patch.anchor, patch.replacement, 1)
	}
	if page != libraryIndexHTML {
		t.Fatal("replayed patches do not reproduce libraryIndexHTML: enhanceLibraryPage and the patch slice have drifted")
	}
}

// Every vocabulary value needs a Chinese label, or the select renders a raw
// English slug into a Chinese UI. The map and the vocabularies are separate
// sources of truth; this pins the seam so a value added to a vocabulary fails
// loudly instead of rendering as its slug.
func TestFacetLabelsCoverEveryVocabularyValue(t *testing.T) {
	for _, field := range []struct {
		name   string
		values []string
	}{
		{"asset_type", normalize.AssetTypeValues},
		{"shot_size", normalize.ShotSizeValues},
		{"camera_motion", normalize.MotionValues},
		{"audio_type", normalize.AudioTypeValues},
		{"quality", normalize.QualityValues},
		{"usable_as", normalize.UsableAsValues},
	} {
		for _, value := range field.values {
			if strings.TrimSpace(facetLabels[value]) == "" {
				t.Fatalf("facet value %q (%s) has no Chinese label", value, field.name)
			}
		}
	}
}

// The facet controls must render from the normalize vocabularies with Chinese
// labels, filterQuery() must send them under the API's parameter names, and
// clearFilters() must reset every one of them — the single easiest thing to
// forget when a control is added.
func TestLibraryPageRendersSemanticFacetControls(t *testing.T) {
	page := libraryIndexHTML
	if !strings.Contains(page, `aria-label="语义筛选"`) {
		t.Fatalf("library page missing the semantic filter section")
	}
	selects := []string{
		"asset-type-select", "shot-size-select", "camera-motion-select",
		"audio-type-select", "quality-select", "usable-as-select",
	}
	for _, id := range selects {
		if !strings.Contains(page, `<select id="`+id+`"`) {
			t.Fatalf("library page missing facet select %q", id)
		}
	}
	// asset-type-select stays single-select, cleared via .value=''.
	if !strings.Contains(page, "document.getElementById('asset-type-select').value=''") {
		t.Fatalf("clearFilters() does not reset asset-type-select")
	}
	// shot-size, camera-motion, audio-type, quality, usable-as are multi-select,
	// cleared via selectedIndex=-1.
	if !strings.Contains(page, ".selectedIndex=-1") {
		t.Fatalf("clearFilters() must use selectedIndex=-1 to reset multi-select facets")
	}
	for _, id := range []string{"shot-size-select", "camera-motion-select", "audio-type-select", "quality-select", "usable-as-select"} {
		if !strings.Contains(page, id) {
			t.Fatalf("clearFilters() does not reference multi-select facet %q", id)
		}
	}
	for _, id := range []string{"min-duration", "max-duration"} {
		if !strings.Contains(page, `<input id="`+id+`"`) {
			t.Fatalf("library page missing duration input %q", id)
		}
		if !strings.Contains(page, "document.getElementById('"+id+"').value=''") {
			t.Fatalf("clearFilters() does not reset duration input %q", id)
		}
	}
	if !strings.Contains(page, `<label for="min-duration">最短时长（秒）</label>`) || !strings.Contains(page, `<label for="max-duration">最长时长（秒）</label>`) {
		t.Fatalf("duration inputs must be labelled in seconds")
	}
	// Options come from the vocabularies, never typed into JS: an English slug
	// shown as a menu item in a Chinese UI is the failure this catches.
	for _, values := range [][]string{
		normalize.AssetTypeValues,
		normalize.ShotSizeValues,
		normalize.MotionValues,
		normalize.AudioTypeValues,
		normalize.QualityValues,
		normalize.UsableAsValues,
	} {
		for _, value := range values {
			if strings.Contains(page, `<option value="`+value+`">`+value+`</option>`) {
				t.Fatalf("facet option %q rendered as its raw English slug", value)
			}
		}
	}
	if !strings.Contains(page, `<option value="extreme_close_up">特写</option>`) {
		t.Fatalf("facet options are not rendered from the vocabulary with Chinese labels")
	}
	// The select ids must be wired to the API's asset_* facet parameter names
	// (the un-prefixed names are legacy aliases, kept only for old callers).
	for _, param := range []string{
		"'asset_type'", "'asset_shot_size'", "'asset_camera_motion'", "'asset_audio_type'", "'asset_quality'", "'asset_usable_as'",
		"'min_duration_ms'", "'max_duration_ms'",
	} {
		if !strings.Contains(page, param) {
			t.Fatalf("filterQuery() does not send facet param %s", param)
		}
	}
	// A 400 from parseFacetFilter must surface as an error, not as an empty
	// library: the old fetch chain mapped any non-ok response to [], which is
	// the exact failure the 400 was written to prevent.
	if !strings.Contains(page, `if(!r.ok)throw Error(await r.text());return r.json()`) {
		t.Fatalf("load() must surface the server message on a non-ok response")
	}
	if strings.Contains(page, `'assets?limit=300'+filterQuery()).then(r=>r.ok?r.json():[])`) {
		t.Fatalf("load() still swallows a non-ok response into the empty-library state")
	}
	// Duration inputs must accept fractional seconds (step="any") so an
	// editor can type 0.5 for 500 ms. step="1" (the old default) and the
	// old Number.isInteger guard both silently dropped fractional input.
	if !strings.Contains(page, `step="any"`) {
		t.Fatalf("duration inputs must use step=\"any\" to accept fractional seconds")
	}
	if strings.Contains(page, `step="1"`) {
		t.Fatalf("duration inputs must not use step=\"1\" — rejects decimal seconds in browser UI")
	}
	// filterQuery() must accept finite floats, not just integers:
	// Number.isInteger(0.5) is false, which silently dropped the input.
	if !strings.Contains(page, `!Number.isFinite(sec)`) {
		t.Fatalf("filterQuery() must use !Number.isFinite(sec) to accept float seconds")
	}
	if strings.Contains(page, `Number.isInteger(sec)`) {
		t.Fatalf("filterQuery() must not use Number.isInteger(sec) — it rejects fractional seconds")
	}
	// The ms conversion must round to avoid float-artefact strings like
	// \"333.3333333333333\" from 0.333 * 1000.
	if !strings.Contains(page, `Math.round(sec*1000)`) {
		t.Fatalf("filterQuery() must use Math.round(sec*1000) for correct millisecond conversion")
	}
}

// TestLibraryPageSearchNarrowsByFacetsAndLoadsHitsByID pins task O — the
// pre-facet page fetched up to 300 cards and intersected them client-side
// against up to 100 search ids (two independently capped windows whose
// overlap silently shrank on a large library, and nothing on screen
// distinguished that from "no match"). search() must now forward the same
// facets the card listing uses, and load(ids) must ask for exactly those ids
// instead of re-deriving the intersection from a second capped listing.
func TestLibraryPageSearchNarrowsByFacetsAndLoadsHitsByID(t *testing.T) {
	page := libraryIndexHTML
	if !strings.Contains(page, `'/api/v1/search?q='+encodeURIComponent(q)+filterQuery()`) {
		t.Fatalf("search() must forward filterQuery() to /api/v1/search, so a facet narrows search the same way it narrows the card listing")
	}
	if !strings.Contains(page, `'/api/v1/assets?ids='+ids.map(encodeURIComponent).join(',')`) {
		t.Fatalf("load(ids) must fetch cards by the search hit ids instead of pulling a capped listing and intersecting")
	}
	if strings.Contains(page, `if(ids)data=data.filter(x=>ids.includes(x.id));`) {
		t.Fatalf("load(ids) still intersects a capped card listing against search ids client-side")
	}
	// A search that matched nothing must render as "no matches", not fall
	// through to /api/v1/assets with ids= empty, which the server reads as
	// unset (match everything) — see parseAssetIDs.
	if !strings.Contains(page, `ids&&!ids.length?Promise.resolve([])`) {
		t.Fatalf("load(ids) must not call /api/v1/assets with an empty ids list")
	}
}
