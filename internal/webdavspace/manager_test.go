package webdavspace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestManagerServesLinkedFileWithAuth(t *testing.T) {
	// Build a manager with an in-memory account and a real linker.
	dir := t.TempDir()
	writeFixtureFile(t, dir, "clip-a.mov", "bytes-clip-a.mov")
	writeFixtureFile(t, dir, "clip-a-proxy.mp4", "bytes-proxy")
	l := &testLinker{dir: dir, originals: map[string]string{"a": "clip-a.mov"}, proxies: map[string]string{"a": "clip-a-proxy.mp4"}}
	accounts := NewMemAccountStore()
	if err := accounts.CreateAccount("editor", "s3cret"); err != nil {
		t.Fatal(err)
	}
	m := NewManager(l, accounts)
	space := m.CreateSpace("space-1")
	ctx := context.Background()
	if _, err := space.LinkOriginal(ctx, "a"); err != nil {
		t.Fatal(err)
	}

	h := m.Handler()
	// No auth → 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/spaces/space-1/assets/a/original.mov", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth GET = %d, want 401", rec.Code)
	}
	// Wrong password → 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/spaces/space-1/assets/a/original.mov", nil)
	req.SetBasicAuth("editor", "wrong")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-password GET = %d, want 401", rec.Code)
	}
	// Correct auth → file bytes.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/spaces/space-1/assets/a/original.mov", nil)
	req.SetBasicAuth("editor", "s3cret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authed GET = %d, body: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "bytes-clip-a.mov" {
		t.Fatalf("body = %q, want bytes-clip-a.mov", rec.Body.String())
	}
}

func TestManagerUnknownSpaceReturnsUnauthorized(t *testing.T) {
	accounts := NewMemAccountStore()
	_ = accounts.CreateAccount("e", "p")
	l := &testLinker{}
	m := NewManager(l, accounts)
	h := m.Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/spaces/ghost/assets/a/original.mov", nil)
	req.SetBasicAuth("e", "p")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown space = %d, want 401 (do not leak space existence)", rec.Code)
	}
}
