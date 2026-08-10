package webdavspace

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
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

func TestManagerConcurrentRequestsStayInTheirSpaces(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "clip-a.mov", "space-a-body")
	writeFixtureFile(t, dir, "clip-b.mov", "space-b-body")
	l := &testLinker{dir: dir, originals: map[string]string{"a": "clip-a.mov", "b": "clip-b.mov"}}
	accounts := NewMemAccountStore()
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	accounts.accounts["editor"] = &Account{Username: "editor", PasswordHash: string(hash)}
	m := NewManager(l, accounts)
	spaceA := m.CreateSpace("space-a")
	spaceB := m.CreateSpace("space-b")
	if _, err := spaceA.LinkOriginal(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := spaceB.LinkOriginal(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	h := m.Handler()

	for round := 0; round < 100; round++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		for _, tc := range []struct {
			space, asset, want string
		}{
			{"space-a", "a", "space-a-body"},
			{"space-b", "b", "space-b-body"},
		} {
			tc := tc
			go func() {
				defer wg.Done()
				<-start
				req := httptest.NewRequest("GET", fmt.Sprintf("/spaces/%s/assets/%s/original.mov", tc.space, tc.asset), nil)
				req.SetBasicAuth("editor", "s3cret")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("round %d %s status = %d, body: %s", round, tc.space, rec.Code, rec.Body.String())
				}
				if rec.Body.String() != tc.want {
					t.Errorf("round %d %s body = %q, want %q", round, tc.space, rec.Body.String(), tc.want)
				}
			}()
		}
		close(start)
		wg.Wait()
	}

	for _, method := range []string{"PUT", "MKCOL", "DELETE"} {
		req := httptest.NewRequest(method, "/spaces/space-a/assets/a/original.mov", nil)
		req.SetBasicAuth("editor", "s3cret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code >= http.StatusOK && rec.Code < http.StatusMultipleChoices {
			t.Errorf("%s linked file status = %d, writes must be rejected", method, rec.Code)
		}
	}

	req := httptest.NewRequest("GET", "/spaces/space-a/assets/b/original.mov", nil)
	req.SetBasicAuth("editor", "s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unlinked path status = %d, want 404", rec.Code)
	}
}
