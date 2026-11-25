//go:build !windows

package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// Media entries that are not regular files — a symlink (possibly pointing
// outside the root), a FIFO, a device — are rejected before fingerprinting
// or registration: they are not footage, and fingerprinting would open
// whatever the entry names.
func TestScanRejectsNonRegularMediaEntries(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "real.mov")

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "elsewhere.mov")
	if err := os.WriteFile(outsideFile, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "linked.mov")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real.mov"), filepath.Join(root, "in-tree.mov")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.mov"), 0o600); err != nil {
		t.Fatal(err)
	}

	repo := &stubScanRepo{}
	s := NewScanner(repo)
	result, err := s.Scan(context.Background(), domain.LibraryRoot{ID: "root-1", Path: root})
	if err != nil {
		t.Fatal(err)
	}

	// Only the regular file was fingerprinted and registered.
	if len(repo.upsertCalls) != 1 || repo.upsertCalls[0] != "real.mov" {
		t.Fatalf("upsert calls = %v, want only real.mov", repo.upsertCalls)
	}
	if len(result.SeenRelativePaths) != 1 || result.SeenRelativePaths[0] != "real.mov" {
		t.Fatalf("seen list = %v, want only real.mov", result.SeenRelativePaths)
	}
	if result.Discovered != 1 {
		t.Fatalf("Discovered = %d, want 1", result.Discovered)
	}
	// Every rejected entry is named, and the scan is not complete so the
	// reconciliation gate never runs on a root whose walk saw one.
	if result.Complete {
		t.Fatalf("scan complete despite rejected entries: %v", result.Errors)
	}
	rejected := 0
	for _, msg := range result.Errors {
		if strings.Contains(msg, "in-tree.mov") || strings.Contains(msg, "linked.mov") || strings.Contains(msg, "pipe.mov") {
			rejected++
		}
	}
	if rejected != 3 {
		t.Fatalf("expected rejections for symlink, in-tree symlink and fifo, got %v", result.Errors)
	}
}
