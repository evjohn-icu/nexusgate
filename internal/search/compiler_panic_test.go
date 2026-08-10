package search

import (
	"strings"
	"testing"
)

// Compile must never panic, whatever the input: the speech-phrase extractor
// used to slice raw[start+len(pair[0]):] before checking start >= 0, so a
// short string with a multi-byte opener like 「 (len 3) overflowed the slice
// bounds. These inputs are the adversarial corpus that regression-tests the
// boundary handling; each call is wrapped so a panic fails the test with the
// offending input rather than a bare stack trace.
func TestCompileNeverPanics(t *testing.T) {
	inputs := []string{
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
	}
	for _, input := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Compile(%q) panicked: %v", input, r)
				}
			}()
			Compile(input)
		}()
	}
}
