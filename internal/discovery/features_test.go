package discovery

import "testing"

// TestSemanticTokensMatchAliasesByWholeWord pins the substring-matching
// regression: "car" is an alias word for traffic, and strings.Contains made
// "carefree"/"careful"/"carpet" match it — a search for "car" then scored
// shots whose text shares no vehicle evidence. ASCII alias words must match
// whole tokens; CJK alias words stay substring-based because Chinese text
// has no word boundaries ("车" inside "红色轿车" is the bridge).
func TestSemanticTokensMatchAliasesByWholeWord(t *testing.T) {
	if got := TokensForText("carefree people on a carpet"); contains(got, "traffic") {
		t.Fatalf("substring 'car' matched 'carefree'/'carpet': tokens %v", got)
	}
	if got := TokensForText("a train arriving on the platform"); contains(got, "traffic") {
		t.Fatalf("substring 'rain' matched 'train': tokens %v", got)
	}
	if got := TokensForText("brainstorming in a boardroom"); contains(got, "rain") {
		t.Fatalf("substring 'rain' matched 'brainstorming': tokens %v", got)
	}
	// Whole-word alias matches must survive.
	if got := TokensForText("red car crossing"); !contains(got, "traffic") {
		t.Fatalf("whole word 'car' lost the traffic alias: tokens %v", got)
	}
	if got := TokensForText("wet street at night"); !contains(got, "night") || !contains(got, "street") {
		t.Fatalf("whole-word aliases lost: tokens %v", got)
	}
	// CJK alias words are substring by design.
	if got := TokensForText("红色轿车驶过广场"); !contains(got, "traffic") {
		t.Fatalf("CJK '车' inside '轿车' must bridge to traffic: tokens %v", got)
	}
	if got := TokensForText("雨中的海滩"); !contains(got, "rain") || !contains(got, "beach") {
		t.Fatalf("CJK aliases lost: tokens %v", got)
	}
}

func contains(tokens []string, want string) bool {
	for _, t := range tokens {
		if t == want {
			return true
		}
	}
	return false
}
