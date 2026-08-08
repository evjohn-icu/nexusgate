package search

import "testing"

func TestRouterIntents(t *testing.T) {
	cases := []struct {
		raw  string
		want SearchIntent
	}{
		{"谁说过我们明天出发", IntentSpeech},
		{`他说“明天见”`, IntentSpeech},
		{"类似 shot_abc123xyz 的镜头", IntentSimilar},
		{"没有人的海边空镜", IntentFact}, // negative
		{"孤独压抑的夜晚", IntentSemantic},
		{"夜晚下雨", IntentSemantic}, // scene/weather only
		{"汽车经过街道", IntentFact},
		{"夜晚下雨，有人撑伞走过街道", IntentFact},
		{"给我找 10 个广州城市生活镜头", IntentCreative},
		{"car", IntentFact},
	}
	for _, c := range cases {
		q := Compile(c.raw)
		if q.Intent != c.want {
			t.Errorf("router(%q) = %s, want %s", c.raw, q.Intent, c.want)
		}
	}
}

func TestNormalizeMode(t *testing.T) {
	cases := []struct {
		mode string
		want SearchIntent
		ok   bool
	}{
		{"", IntentAuto, true},
		{"auto", IntentAuto, true},
		{"fact", IntentFact, true},
		{"speech", IntentSpeech, true},
		{"semantic", IntentSemantic, true},
		{"similar", IntentSimilar, true},
		{"creative", IntentCreative, true},
		{"bogus", IntentAuto, false},
	}
	for _, c := range cases {
		got, ok := normalizeMode(c.mode)
		if got != c.want || ok != c.ok {
			t.Errorf("normalizeMode(%q) = %s,%v want %s,%v", c.mode, got, ok, c.want, c.ok)
		}
	}
}
