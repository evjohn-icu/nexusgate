package webdavspace

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/cachecoord"
	"golang.org/x/net/webdav"
)

// testLinker resolves fixed paths from a temp dir, keyed by asset id.
type testLinker struct {
	dir       string
	originals map[string]string // assetID -> filename
	proxies   map[string]string // assetID -> filename
}

func (t *testLinker) OriginalLocation(_ context.Context, assetID string) (string, string) {
	f, ok := t.originals[assetID]
	if !ok {
		return "", ""
	}
	return t.dir, f
}

func (t *testLinker) ProxyLocation(_ context.Context, assetID string) (string, string) {
	f, ok := t.proxies[assetID]
	if !ok {
		return "", ""
	}
	return t.dir, f
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
	l, s := setup(t)
	ctx := context.Background()

	vp, err := s.LinkOriginal(ctx, "asset-a")
	if err != nil {
		t.Fatal(err)
	}
	if vp != "/assets/asset-a/original.mov" {
		t.Fatalf("virtual path = %q, want /assets/asset-a/original.mov", vp)
	}
	root, relative, ok := s.Resolve(ctx, vp)
	if !ok || root != l.dir || relative != "clip-a.mov" {
		t.Fatalf("Resolve(%q) = %q, %q, %t; want %q, clip-a.mov", vp, root, relative, ok, l.dir)
	}
}

func TestUnlinkedAssetNotFound(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	if _, _, ok := s.Resolve(ctx, "/assets/asset-b/original.mp4"); ok {
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
	if _, _, ok := s.Resolve(ctx, vp); !ok {
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

func TestOpenFileSharedLockProtectsGCExclusiveDeletionUntilClose(t *testing.T) {
	dataDir := t.TempDir()
	cacheDir := filepath.Join(dataDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cacheDir, "asset-a", "proxy-sw.mp4")
	writeFixtureFile(t, filepath.Dir(path), filepath.Base(path), "cache bytes")
	l := &testLinker{dir: filepath.Dir(path), originals: map[string]string{"asset-a": filepath.Base(path)}}
	s := NewSpace("space-1", l, dataDir)
	vp, err := s.LinkOriginal(context.Background(), "asset-a")
	if err != nil {
		t.Fatal(err)
	}
	fs := s.NewHandlerFS("/spaces/space-1")
	file, err := fs.OpenFile(context.Background(), vp, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan *cachecoord.Lock, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		lock, lockErr := cachecoord.AcquireExclusive(dataDir)
		if lockErr != nil {
			return
		}
		acquired <- lock
	}()
	<-started
	select {
	case lock := <-acquired:
		_ = lock.Release()
		t.Fatal("GC exclusive lock acquired while WebDAV file was open")
	default:
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case lock := <-acquired:
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := lock.Release(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exclusive lock did not acquire after WebDAV file close")
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

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

func TestReaddirPagingOffsetAndEOF(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	if _, err := s.LinkOriginal(ctx, "asset-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkProxy(ctx, "asset-a"); err != nil {
		t.Fatal(err)
	}
	fs := s.NewHandlerFS("/spaces/space-1")
	f, err := fs.OpenFile(ctx, "/assets/asset-a", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// A positive count pages with an offset: one entry per call until the
	// final page, which reports io.EOF alongside what it served.
	first, err := f.Readdir(1)
	if err != nil || len(first) != 1 {
		t.Fatalf("Readdir(1) #1 = %v, %v; want one entry, no error", first, err)
	}
	second, err := f.Readdir(1)
	if len(second) != 1 || err != io.EOF {
		t.Fatalf("Readdir(1) #2 = %v, %v; want last entry with io.EOF", second, err)
	}
	if exhausted, err := f.Readdir(1); exhausted != nil || err != io.EOF {
		t.Fatalf("exhausted Readdir(1) = %v, %v; want nil, io.EOF", exhausted, err)
	}

	// Fresh open: a count larger than the directory serves everything that
	// remains with io.EOF; nonpositive counts serve the whole remainder with
	// a nil error, and repeat as empty.
	for _, count := range []int{5, 0, -1} {
		fresh, err := fs.OpenFile(ctx, "/assets/asset-a", os.O_RDONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		infos, err := fresh.Readdir(count)
		if err != nil && err != io.EOF {
			t.Fatalf("Readdir(%d) = %v", count, err)
		}
		if len(infos) != 2 {
			t.Fatalf("Readdir(%d) = %d entries, want 2", count, len(infos))
		}
		if names := []string{infos[0].Name(), infos[1].Name()}; names[0] != "original.mov" || names[1] != "proxy.mp4" {
			t.Fatalf("Readdir(%d) order = %v, want sorted [original.mov proxy.mp4]", count, names)
		}
		fresh.Close()
	}
}

func TestListingReportsOnlyExistingChildren(t *testing.T) {
	l, s := setup(t)
	ctx := context.Background()
	if _, err := s.LinkOriginal(ctx, "asset-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkOriginal(ctx, "asset-b"); err != nil {
		t.Fatal(err)
	}
	fs := s.NewHandlerFS("/spaces/space-1")
	if _, err := fs.OpenFile(ctx, "/assets/asset-a", os.O_RDONLY, 0); err != nil {
		t.Fatalf("asset dir listing before removal: %v", err)
	}

	// The NAS loses clip-a.mov: its virtual entry and its asset directory
	// must disappear rather than show up empty; asset-b is untouched.
	if err := os.Remove(filepath.Join(l.dir, "clip-a.mov")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.OpenFile(ctx, "/assets/asset-a/original.mov", os.O_RDONLY, 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open of vanished file = %v, want not-exist", err)
	}
	if _, err := fs.OpenFile(ctx, "/assets/asset-a", os.O_RDONLY, 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("listing of emptied asset dir = %v, want not-exist", err)
	}
	if _, err := fs.Stat(ctx, "/assets/asset-a"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat of emptied asset dir = %v, want not-exist", err)
	}
	f, err := fs.OpenFile(ctx, "/assets", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("assets dir with one existing asset = %v", err)
	}
	defer f.Close()
	infos, err := f.Readdir(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name() != "asset-b" {
		t.Fatalf("assets listing = %v, want only asset-b", infos)
	}
}

func TestLinkEscapingRootAppearsUnavailable(t *testing.T) {
	dir := t.TempDir()
	l := &testLinker{dir: dir, originals: map[string]string{"escapee": "../outside.mov"}}
	s := NewSpace("space-1", l)
	ctx := context.Background()
	vp, err := s.LinkOriginal(ctx, "escapee")
	if err != nil {
		t.Fatal(err)
	}
	fs := s.NewHandlerFS("/spaces/space-1")
	if _, _, ok := s.Resolve(ctx, vp); !ok {
		t.Fatal("Resolve hands out the trusted pair; containment is enforced at open")
	}
	if _, err := fs.OpenFile(ctx, vp, os.O_RDONLY, 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open of escaping link = %v, want not-exist", err)
	}
	if _, err := fs.Stat(ctx, vp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat of escaping link = %v, want not-exist", err)
	}
}

func TestStatMatchesOpenFilePredicate(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	if _, err := s.LinkOriginal(ctx, "asset-a"); err != nil {
		t.Fatal(err)
	}
	fs := s.NewHandlerFS("/spaces/space-1")
	for _, dir := range []string{"/", "/assets", "/assets/asset-a"} {
		fi, err := fs.Stat(ctx, dir)
		if err != nil || !fi.IsDir() {
			t.Fatalf("Stat(%q) = %v, %v; want dir", dir, fi, err)
		}
		f, err := fs.OpenFile(ctx, dir, os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("OpenFile(%q) = %v; want a directory node", dir, err)
		}
		st, err := f.Stat()
		if err != nil || !st.IsDir() {
			t.Fatalf("OpenFile(%q).Stat() = %v, %v; want dir", dir, st, err)
		}
		f.Close()
	}
	fi, err := fs.Stat(ctx, "/assets/asset-a/original.mov")
	if err != nil || fi.IsDir() {
		t.Fatalf("Stat(file) = %v, %v; want regular file", fi, err)
	}
	if fi.Size() != int64(len("bytes-clip-a.mov")) {
		t.Fatalf("Stat(file).Size() = %d, want the real media size", fi.Size())
	}
}

func TestDepthOnePropfindSeesEachEntryOnce(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "clip-a.mov", "bytes-clip-a.mov")
	writeFixtureFile(t, dir, "clip-a-proxy.mp4", "bytes-proxy")
	l := &testLinker{dir: dir, originals: map[string]string{"asset-a": "clip-a.mov"}, proxies: map[string]string{"asset-a": "clip-a-proxy.mp4"}}
	accounts := NewMemAccountStore()
	if err := accounts.CreateAccount("editor", "s3cret"); err != nil {
		t.Fatal(err)
	}
	m := NewManager(l, accounts)
	space := m.CreateSpace("space-1")
	ctx := context.Background()
	if _, err := space.LinkOriginal(ctx, "asset-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := space.LinkProxy(ctx, "asset-a"); err != nil {
		t.Fatal(err)
	}
	h := m.Handler()

	propfind := func(path, depth string) string {
		t.Helper()
		req := httptest.NewRequest("PROPFIND", path, nil)
		req.Header.Set("Depth", depth)
		req.SetBasicAuth("editor", "s3cret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 207 {
			t.Fatalf("PROPFIND %s = %d, want 207; body: %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	// Root listing: the assets directory appears.
	rootBody := propfind("/spaces/space-1/", "1")
	if !strings.Contains(rootBody, "/spaces/space-1/assets") {
		t.Fatalf("root listing missing assets dir: %s", rootBody)
	}
	// The assets directory lists the linked asset exactly once, although
	// original and proxy both link beneath it.
	assetsBody := propfind("/spaces/space-1/assets", "1")
	if got := strings.Count(assetsBody, "/spaces/space-1/assets/asset-a"); got != 1 {
		t.Fatalf("assets listing mentions asset-a %d times, want exactly 1: %s", got, assetsBody)
	}
	// The asset lists original and proxy exactly once each.
	assetBody := propfind("/spaces/space-1/assets/asset-a", "1")
	for _, want := range []string{"/spaces/space-1/assets/asset-a/original.mov", "/spaces/space-1/assets/asset-a/proxy.mp4"} {
		if got := strings.Count(assetBody, want); got != 1 {
			t.Fatalf("asset listing mentions %s %d times, want exactly 1: %s", want, got, assetBody)
		}
	}
}

func TestOrdinaryFileURLStreamsAndRangeSeeks(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	vp, err := s.LinkOriginal(ctx, "asset-a")
	if err != nil {
		t.Fatal(err)
	}
	h := &webdav.Handler{Prefix: "/spaces/space-1", FileSystem: s.NewHandlerFS("/spaces/space-1"), LockSystem: webdav.NewMemLS()}

	get := func(rangeHeader string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/spaces/space-1"+vp, nil)
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	full := get("")
	if full.Code != 200 || full.Body.String() != "bytes-clip-a.mov" {
		t.Fatalf("full GET = %d %q", full.Code, full.Body.String())
	}
	partial := get("bytes=2-6")
	if partial.Code != 206 || partial.Body.String() != "tes-c" {
		t.Fatalf("range GET = %d %q, want 206 tes-c", partial.Code, partial.Body.String())
	}
	suffix := get("bytes=-3")
	if suffix.Code != 206 || suffix.Body.String() != "mov" {
		t.Fatalf("suffix range GET = %d %q, want 206 mov", suffix.Code, suffix.Body.String())
	}
}

func TestFailedOpensReleaseRootAndLockHandles(t *testing.T) {
	dataDir := t.TempDir()
	l := &testLinker{dir: dataDir, originals: map[string]string{
		"gone":    "missing.mov",
		"escapee": "../outside.mov",
	}}
	s := NewSpace("space-1", l, dataDir)
	fs := s.NewHandlerFS("/spaces/space-1")
	ctx := context.Background()

	countFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skip("fd accounting requires /proc")
		}
		return len(entries)
	}
	before := countFDs()
	for range 200 {
		for _, vp := range []string{"/assets/gone/original.bin", "/assets/escapee/original.bin"} {
			if _, err := fs.OpenFile(ctx, vp, os.O_RDONLY, 0); err == nil {
				t.Fatalf("open %s unexpectedly succeeded", vp)
			}
			if _, err := fs.Stat(ctx, vp); err == nil {
				t.Fatalf("stat %s unexpectedly succeeded", vp)
			}
		}
	}
	if after := countFDs(); after > before+2 {
		t.Fatalf("failed opens leaked handles: %d fds before, %d after 400 attempts", before, after)
	}
}
