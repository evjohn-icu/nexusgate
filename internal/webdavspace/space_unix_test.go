//go:build !windows

package webdavspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A symlink inside the root pointing outside it, and a FIFO named like
// media, must both appear unavailable at serve time — never followed to the
// target, never blocked on, never served.
func TestNonRegularAndEscapingEntriesAppearUnavailable(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "elsewhere.mov")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(dir, "linked-out.mov")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "in-tree.mov"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("in-tree.mov", filepath.Join(dir, "linked-in.mov")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo.mov"), 0o600); err != nil {
		t.Fatal(err)
	}

	l := &testLinker{dir: dir, originals: map[string]string{
		"out":  "linked-out.mov",
		"in":   "linked-in.mov",
		"fifo": "fifo.mov",
	}}
	s := NewSpace("space-1", l)
	fs := s.NewHandlerFS("/spaces/space-1")
	ctx := context.Background()

	for _, asset := range []string{"out", "fifo"} {
		vp, err := s.LinkOriginal(ctx, asset)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fs.OpenFile(ctx, vp, os.O_RDONLY, 0); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("open %s = %v, want not-exist", vp, err)
		}
		if _, err := fs.Stat(ctx, vp); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat %s = %v, want not-exist", vp, err)
		}
		if children := s.childrenOf(ctx, "/assets/"+asset); len(children) != 0 {
			t.Fatalf("listing of /assets/%s = %v, want no children", asset, children)
		}
	}

	// An in-tree symlink to a regular file still resolves — os.Root follows
	// it only within the root — and serves the target's bytes.
	vp, err := s.LinkOriginal(ctx, "in")
	if err != nil {
		t.Fatal(err)
	}
	f, err := fs.OpenFile(ctx, vp, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open in-tree symlink: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 8)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		t.Fatal(err)
	}
	if string(buf[:n]) != "x" {
		t.Fatalf("in-tree symlink streamed %q, want x", buf[:n])
	}
}
