package externalalign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

func TestAlign_EmptyCommand(t *testing.T) {
	p := &Provider{Command: ""}
	_, err := p.Align(context.Background(), common.AlignRequest{})
	if err == nil {
		t.Fatal("expected error for empty command")
	}
	if !strings.Contains(err.Error(), "alignment command is empty") {
		t.Errorf("expected 'alignment command is empty', got: %v", err)
	}
}

func TestAlign_CommandNotFound(t *testing.T) {
	p := &Provider{Command: "/nonexistent/timingdex_aligner_test_cmd"}
	_, err := p.Align(context.Background(), common.AlignRequest{})
	if err == nil {
		t.Fatal("expected error for non-existent command")
	}
	if !strings.Contains(err.Error(), "aligner failed") {
		t.Errorf("expected 'aligner failed', got: %v", err)
	}
}

// helperScript writes a shell script to a temp file and returns its path.
func helperScript(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "aligner.sh")
	if err := os.WriteFile(path, []byte(content), 0700); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	return path
}

func TestAlign_InvalidJSON(t *testing.T) {
	script := helperScript(t, `#!/bin/sh
echo "not json at all"
`)
	p := &Provider{Command: script}
	_, err := p.Align(context.Background(), common.AlignRequest{})
	if err == nil {
		t.Fatal("expected error for invalid JSON output")
	}
	if !strings.Contains(err.Error(), "decode aligner output") {
		t.Errorf("expected 'decode aligner output', got: %v", err)
	}
}

func TestAlign_EmptyWords(t *testing.T) {
	script := helperScript(t, `#!/bin/sh
echo '{"words":[]}'
`)
	p := &Provider{Command: script}
	_, err := p.Align(context.Background(), common.AlignRequest{})
	if err == nil {
		t.Fatal("expected error for empty words array")
	}
	if !strings.Contains(err.Error(), "no words") {
		t.Errorf("expected 'no words', got: %v", err)
	}
}

func TestAlign_Success(t *testing.T) {
	script := helperScript(t, `#!/bin/sh
echo '{"words":[{"start_ms":0,"end_ms":500,"text":"hello"},{"start_ms":600,"end_ms":900,"text":"world"}]}'
`)
	p := &Provider{Command: script, ModelName: "test-aligner"}
	if p.Name() != "external_command" {
		t.Errorf("expected Name 'external_command', got %q", p.Name())
	}
	if p.Model() != "test-aligner" {
		t.Errorf("expected Model 'test-aligner', got %q", p.Model())
	}

	result, err := p.Align(context.Background(), common.AlignRequest{
		AudioPath: "/tmp/test.wav",
		Language:  "en",
		Transcript: domain.Transcript{
			Text:     "hello world",
			Segments: []domain.TranscriptSegment{{Text: "hello world", StartMS: 0, EndMS: 1000}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Words) != 2 {
		t.Fatalf("expected 2 words, got %d", len(result.Words))
	}
	if result.Words[0].Text != "hello" || result.Words[0].StartMS != 0 || result.Words[0].EndMS != 500 {
		t.Errorf("first word mismatch: %+v", result.Words[0])
	}
	if result.Words[1].Text != "world" || result.Words[1].StartMS != 600 || result.Words[1].EndMS != 900 {
		t.Errorf("second word mismatch: %+v", result.Words[1])
	}
	if !strings.Contains(result.RawResponse, "hello") {
		t.Errorf("RawResponse should contain 'hello', got %q", result.RawResponse)
	}
}

func TestAlign_DefaultModel(t *testing.T) {
	// The default model identity is derived from the command line, so swapping
	// the aligner binary re-keys the align job and the old word timeline can
	// never keep being served as the current one.
	p := &Provider{Command: "/bin/true", Args: []string{"-a"}}
	if got := p.Model(); !strings.HasPrefix(got, "forced-aligner-") {
		t.Errorf("expected derived model id, got %q", got)
	}
	q := &Provider{Command: "/bin/true", Args: []string{"-b"}}
	if p.Model() == q.Model() {
		t.Error("different aligner command lines must yield different model identities")
	}
	if p.Model() != p.Model() {
		t.Error("model identity must be deterministic")
	}
}
