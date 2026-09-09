package api

import (
	"strings"
	"testing"
)

// The timeline block is sized by how long its shot is, not by how long its
// description is. Putting the description inside the block therefore made the
// text readable only when an asset had exactly one shot: at five shots each
// block is about seventy pixels wide and every one of them renders a truncated
// stub. The words now live in a list under the track, where width is a function
// of the card rather than of the shot, and the block keeps only what it is
// actually good at — showing where in the asset the shot sits.
func TestTimelineDescriptionsRenderOutsideTheProportionalBlocks(t *testing.T) {
	page := libraryIndexHTML

	if !strings.Contains(page, `<ol class="shot-lines">`) {
		t.Fatal("the timeline renders no shot list; the descriptions have nowhere readable to go")
	}
	if !strings.Contains(page, `shot-line-time`) || !strings.Contains(page, `shot-line-text`) {
		t.Fatal("a shot list row is missing its time or its text half")
	}
	// The block itself must stay wordless. A description inside it is the bug
	// this replaced, and it would come back invisibly: the page still builds,
	// still serves, and only looks wrong at more than one shot per asset.
	if strings.Contains(page, `'"><span>'+esc(description)+'</span></div>'`) ||
		strings.Contains(page, `'"><span>'+esc(description)+'</span></button>'`) {
		t.Fatal("the shot description is being rendered inside the proportional block again")
	}
}

// The list is only readable if its long descriptions wrap. Inheriting the
// block's nowrap/ellipsis rules would move the text without fixing anything.
func TestShotListWrapsInsteadOfTruncating(t *testing.T) {
	page := libraryIndexHTML
	start := strings.Index(page, ".shot-line-text{")
	if start < 0 {
		t.Fatal(".shot-line-text has no rule of its own, so it inherits whatever the block used")
	}
	rule := page[start:]
	if end := strings.Index(rule, "}"); end >= 0 {
		rule = rule[:end]
	}
	if strings.Contains(rule, "nowrap") || strings.Contains(rule, "ellipsis") {
		t.Fatalf("the shot list truncates like the block it replaced: %q", rule)
	}
	if !strings.Contains(rule, "overflow-wrap") && !strings.Contains(rule, "word-break") {
		t.Fatalf("the shot list has no wrapping rule, so an unbroken description overflows the card: %q", rule)
	}
}

// The block stays clickable and stays labelled. Emptying it is a layout change,
// not an accessibility regression: the drawer still opens from it, and a screen
// reader still gets the description that is no longer painted inside it.
func TestEmptiedTimelineBlockKeepsItsLabelAndPayload(t *testing.T) {
	page := libraryIndexHTML
	for _, needed := range []string{
		`aria-label="'+esc(tdT('library.ariaTimeRange'`,
		`data-shot="'+encodeURIComponent(JSON.stringify(s))+'"`,
		`</button>'`,
	} {
		if !strings.Contains(page, needed) {
			t.Errorf("the timeline block lost %q when its text was removed", needed)
		}
	}
}
