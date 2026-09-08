package media

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests need the real ffprobe: the property under test only exists when a
// real binary can either reject a real file or fail to start. Following
// process_integration_test.go, skip rather than fake the binary.
func requireFFprobeBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is required for this test")
	}
}

// writeRejectedMOV stands in for the real trigger (a MOV missing its moov
// atom): bytes no container parser accepts, under a .mov name. ffprobe exits
// non-zero on it, which is the only thing ErrProbeRejected is defined on.
func writeRejectedMOV(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "no-moov-atom.mov")
	if err := os.WriteFile(path, []byte("this is not a movie: no moov atom here\n"), 0o600); err != nil {
		t.Fatalf("write rejected-file fixture: %v", err)
	}
	return path
}

// Positive case plus the anti-flattening check: the sentinel must wrap
// ffprobe's ExitError, not replace it, because the ExitError is what still
// carries the original verdict and stderr.
func TestProbeRejectedFileHitsSentinelAndKeepsExitError(t *testing.T) {
	requireFFprobeBinary(t)
	path := writeRejectedMOV(t)

	_, err := Probe(context.Background(), path)
	if err == nil {
		t.Fatal("Probe accepted a file ffprobe rejects: the ErrProbeRejected path never ran")
	}
	if !errors.Is(err, ErrProbeRejected) {
		t.Fatalf("rejected file did not satisfy errors.Is(err, ErrProbeRejected): err = %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error chain lost *exec.ExitError, so a caller cannot tell how ffprobe failed (flattened %%w to %%v?): err = %v", err)
	}
	stderr := strings.Join(strings.Fields(string(exitErr.Stderr)), " ")
	if stderr == "" {
		t.Fatal("ffprobe produced no stderr for the rejected file, so the message cannot carry a snippet")
	}
	snippet := stderr
	if len(snippet) > 200 {
		snippet = snippet[:200] // stay inside the untruncated prefix of probeStderrLimit
	}
	if !strings.Contains(err.Error(), snippet) {
		t.Fatalf("error message omits ffprobe's own stderr %q, leaving callers only prose: err = %v", snippet, err)
	}
}

// A missing file makes ffprobe exit non-zero exactly like a corrupt one, which
// is why the sentinel alone proves nothing: the chain must still expose the
// ExitError so the "ffprobe actually ran" fact is not lost.
func TestProbeMissingFileStillShowsFFprobeRan(t *testing.T) {
	requireFFprobeBinary(t)

	_, err := Probe(context.Background(), filepath.Join(t.TempDir(), "does-not-exist.mov"))
	if err == nil {
		t.Fatal("Probe succeeded for a nonexistent path")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("nonexistent path did not surface *exec.ExitError, so ffprobe never ran: err = %v", err)
	}
}

// The discriminating case, and the only assertion that proves the sentinel has
// any: with no ffprobe on PATH the failure is environmental and retryable, so
// it must not be reported as a rejected (i.e. corrupt) file.
func TestProbeMissingBinaryIsNotRejected(t *testing.T) {
	requireFFprobeBinary(t)
	t.Setenv("PATH", t.TempDir())

	_, err := Probe(context.Background(), filepath.Join(t.TempDir(), "anything.mov"))
	if err == nil {
		t.Fatal("Probe succeeded with no ffprobe on PATH")
	}
	if errors.Is(err, ErrProbeRejected) {
		t.Fatalf("missing ffprobe binary was classified as a rejected file, turning an environment problem into media corruption: err = %v", err)
	}
	var execErr *exec.Error
	if !errors.As(err, &execErr) {
		t.Fatalf("missing binary did not surface *exec.Error, so the retryable cause is unidentifiable: err = %v", err)
	}
}
