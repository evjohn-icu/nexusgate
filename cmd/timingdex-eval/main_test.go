package main

import (
	"strings"
	"testing"
)

// The eval CLI's flag parsing is the operator's entry point, and it regressed
// once on positional-index validation (score validated "label" when the user
// passed "--labels" and reported it missing). These tests pin the two
// commands' required flags explicitly, so a regression fails here instead of
// on a machine.
func TestParseRunArgs(t *testing.T) {
	t.Run("all flags parse", func(t *testing.T) {
		corpus, dataDir, label, err := parseRunArgs([]string{"--corpus", "./c", "--data-dir", "./d", "--label", "qwen3vl-4b"})
		if err != nil {
			t.Fatal(err)
		}
		if corpus != "./c" || dataDir != "./d" || label != "qwen3vl-4b" {
			t.Fatalf("got %q %q %q", corpus, dataDir, label)
		}
	})
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing label", []string{"--corpus", "./c", "--data-dir", "./d"}, "--label"},
		{"missing corpus", []string{"--data-dir", "./d", "--label", "l"}, "--corpus"},
		{"missing data-dir", []string{"--corpus", "./c", "--label", "l"}, "--data-dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := parseRunArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error naming %q, got %v", tc.want, err)
			}
		})
	}
	t.Run("unknown flag", func(t *testing.T) {
		if _, _, _, err := parseRunArgs([]string{"--corpus", "./c", "--data-dir", "./d", "--label", "l", "--bogus"}); err == nil {
			t.Fatal("expected error for unknown flag")
		}
	})
}

func TestParseScoreArgs(t *testing.T) {
	t.Run("labels list parses", func(t *testing.T) {
		corpus, dataDir, labels, err := parseScoreArgs([]string{"--corpus", "./c", "--data-dir", "./d", "--labels", "qwen3vl-4b,gemini-flash"})
		if err != nil {
			t.Fatal(err)
		}
		if corpus != "./c" || dataDir != "./d" || labels != "qwen3vl-4b,gemini-flash" {
			t.Fatalf("got %q %q %q", corpus, dataDir, labels)
		}
	})
	t.Run("missing labels", func(t *testing.T) {
		_, _, _, err := parseScoreArgs([]string{"--corpus", "./c", "--data-dir", "./d"})
		if err == nil || !strings.Contains(err.Error(), "--labels") {
			t.Fatalf("expected error naming --labels, got %v", err)
		}
	})
	t.Run("single label flag does not satisfy score", func(t *testing.T) {
		// The regression this pins: score previously validated the "label"
		// flag (positional index 2) while documenting "--labels" (index 3),
		// so a correct --labels invocation reported it missing and a wrong
		// --label invocation passed. score must demand --labels and reject
		// --label as unknown.
		if _, _, _, err := parseScoreArgs([]string{"--corpus", "./c", "--data-dir", "./d", "--label", "qwen3vl-4b"}); err == nil {
			t.Fatal("--label must not satisfy --labels")
		}
	})
	t.Run("unknown flag", func(t *testing.T) {
		if _, _, _, err := parseScoreArgs([]string{"--corpus", "./c", "--data-dir", "./d", "--labels", "a", "--bogus"}); err == nil {
			t.Fatal("expected error for unknown flag")
		}
	})
}
