package webdavspace

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/webdav"
)

// testLinker resolves fixed paths from a temp dir, keyed by asset id.
type testLinker struct {
	dir       string
	originals map[string]string // assetID -> filename
	proxies   map[string]string // assetID -> filename
}

func (t *testLinker) OriginalPath(_ context.Context, assetID string) string {
	f, ok := t.originals[assetID]
	if !ok {
		return ""
	}
	return filepath.Join(t.dir, f)
}

func (t *testLinker) ProxyPath(_ context.Context, assetID string) string {
	f, ok := t.proxies[assetID]
	if !ok {
		return ""
	}
	return filepath.Join(t.dir, f)
}

func setup(t *testing.T) (*testLinker, *Space) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"clip-a.mov", "clip-a-proxy.mp4", "clip-b.mp4"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("bytes-"+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := &testLinker{
		dir:       dir,
		originals: map[string]string{"asset-a": "clip-a.mov", "asset-b": "clip-b.mp4"},
		proxies:   map[string]string{"asset-a": "clip-a-proxy.mp4"},
	}
	s := NewSpace("space-1", l)
	return l, s
}

func TestLinkOriginalAndResolve(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()

	vp, err := s.LinkOriginal(ctx, "asset-a")
	if err != nil {
		t.Fatal(err)
	}
	if vp != "/assets/asset-a/original.mov" {
		t.Fatalf("virtual path = %q, want /assets/asset-a/original.mov", vp)
	}
	real, ok := s.Resolve(ctx, vp)
	if !ok || !strings.HasSuffix(real, "clip-a.mov") {
		t.Fatalf("Resolve(%q) = %q, %t", vp, real, ok)
	}
}

func TestUnlinkedAssetNotFound(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	if _, ok := s.Resolve(ctx, "/assets/asset-b/original.mp4"); ok {
		t.Fatal("unlinked asset resolved; space should be empty until linked")
	}
}

func TestLinkProxy(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	vp, err := s.LinkProxy(ctx, "asset-a")
	if err != nil {
		t.Fatal(err)
	}
	if vp != "/assets/asset-a/proxy.mp4" {
		t.Fatalf("proxy virtual path = %q", vp)
	}
	if _, ok := s.Resolve(ctx, vp); !ok {
		t.Fatal("proxy did not resolve")
	}
}

func TestSpaceIsReadOnly(t *testing.T) {
	_, s := setup(t)
	fs := s.NewHandlerFS("/spaces/space-1")

	if err := fs.Mkdir(context.Background(), "/newdir", 0o755); err == nil {
		t.Fatal("Mkdir on a space must be rejected")
	}
	if _, err := fs.OpenFile(context.Background(), "/x", os.O_WRONLY, 0o644); err == nil {
		t.Fatal("write open must be rejected")
	}
	if _, err := fs.OpenFile(context.Background(), "/x", os.O_CREATE|os.O_RDWR, 0o644); err == nil {
		t.Fatal("create open must be rejected")
	}
}

func TestOpenFileStreamsBytes(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	vp, err := s.LinkOriginal(ctx, "asset-a")
	if err != nil {
		t.Fatal(err)
	}
	fs := s.NewHandlerFS("/spaces/space-1")
	f, err := fs.OpenFile(ctx, vp, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 64)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		t.Fatal(err)
	}
	if string(buf[:n]) != "bytes-clip-a.mov" {
		t.Fatalf("streamed bytes = %q", buf[:n])
	}
}

func TestListingShowsOnlyLinked(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	if _, err := s.LinkOriginal(ctx, "asset-a"); err != nil {
		t.Fatal(err)
	}
	fs := s.NewHandlerFS("/spaces/space-1")
	f, err := fs.OpenFile(ctx, "/assets/asset-a", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	infos, err := f.Readdir(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name() != "original.mov" {
		t.Fatalf("listing = %v, want exactly [original.mov]", infos)
	}
}

func TestRootListsAssetDirectories(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	if _, err := s.LinkOriginal(ctx, "asset-a"); err != nil {
		t.Fatal(err)
	}
	fs := s.NewHandlerFS("/spaces/space-1")
	f, err := fs.OpenFile(ctx, "/", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.IsDir() {
		t.Fatalf("root stat = %v, %v; want dir", fi, err)
	}
	if _, err := fs.Stat(ctx, "/assets/asset-a"); err != nil {
		t.Fatalf("asset dir stat: %v", err)
	}
}

func TestSpaceCreatedAtSet(t *testing.T) {
	s := NewSpace("x", &testLinker{})
	if s.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be set")
	}
	if s.ID != "x" {
		t.Fatalf("ID = %q", s.ID)
	}
}

func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

var _ = time.Now // keep time import for future use

func TestWebDAVHandlerServesLinkedFile(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	vp, err := s.LinkOriginal(ctx, "asset-a")
	if err != nil {
		t.Fatal(err)
	}
	fs := s.NewHandlerFS("/spaces/space-1")
	h := &webdav.Handler{
		Prefix:     "/spaces/space-1",
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
	}
	req := httptest.NewRequest("GET", "/spaces/space-1"+vp, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET %s = %d, want 200; body: %s", req.URL.Path, rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "bytes-clip-a.mov" {
		t.Fatalf("body = %q, want bytes-clip-a.mov", got)
	}
}

func TestWebDAVHandlerRejectsUnlinkedAndWrites(t *testing.T) {
	_, s := setup(t)
	fs := s.NewHandlerFS("/spaces/space-1")
	h := &webdav.Handler{
		Prefix:     "/spaces/space-1",
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
	}

	// Unlinked file → 404.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/spaces/space-1/assets/asset-b/original.mp4", nil))
	if rec.Code != 404 {
		t.Fatalf("unlinked GET = %d, want 404", rec.Code)
	}

	// PUT (write) → rejected.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("PUT", "/spaces/space-1/assets/asset-a/original.mov", strings.NewReader("x")))
	if rec.Code == 200 {
		t.Fatalf("PUT to read-only space succeeded (%d)", rec.Code)
	}
}
