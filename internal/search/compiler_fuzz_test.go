package search

import (
	"strings"
	"testing"
)

// FuzzCompile seeds the heuristic corpus plus valid queries and asserts that
// Compile never panics on any mutation a fuzzer can throw at it. The speech-
// phrase extractor once sliced raw[start+len(pair[0]):] before checking
// start >= 0; a panic here would surface as a fuzzer crash. The body recovers
// panics so they fail the test with the input rather than killing the worker.
func FuzzCompile(f *testing.F) {
	seeds := []string{
		"a",
		"1",
		"?",
		"「",
		"“",
		"hello 「unfinished",
		"",
		" ",
		"人",
		"a「",
		"「a",
		"：“",
		"said “hello",
		`"x`,
		`'`,
		"😀🎬🎥",
		strings.Repeat("a", 100000),
		"\x00",
		strings.Repeat("「", 10000),
		"车窗上的雨滴",
		"夜晚下雨 有人撑伞",
		"他说“明天见”",
		"no",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Compile(%q) panicked: %v", s, r)
			}
		}()
		Compile(s)
	})
}
