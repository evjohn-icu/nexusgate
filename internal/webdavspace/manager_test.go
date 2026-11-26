package webdavspace

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

// WEBDAV-001: a source that fails Basic Auth enough times is blocked with 429
// (and Retry-After), even for a request that would have used correct
// credentials — the block is by source, and a different source is unaffected.
func TestManagerRateLimitsFailedAuthenticationBySourceIP(t *testing.T) {
	accounts := NewMemAccountStore()
	_ = accounts.CreateAccount("e", "p")
	m := NewManager(&testLinker{}, accounts)
	h := m.Handler()

	wrongAttempt := func(ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/spaces/space-1/assets/a/original.mov", nil)
		r.RemoteAddr = ip
		r.SetBasicAuth("e", "wrong")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	for i := 0; i < webdavLoginMaxFails; i++ {
		if rec := wrongAttempt("192.0.2.10:1"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	// Now blocked: even correct credentials answer 429 with Retry-After.
	r := httptest.NewRequest("GET", "/spaces/space-1/assets/a/original.mov", nil)
	r.RemoteAddr = "192.0.2.10:1"
	r.SetBasicAuth("e", "p")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked source = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("rate-limited response missing Retry-After")
	}
	// A different source is not blocked by this source's failures.
	if rec := wrongAttempt("192.0.2.20:1"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unrelated source = %d, want 401 (must not inherit the block)", rec.Code)
	}
}

// WEBDAV-001: a successful authentication clears the source's failure budget,
// so a user who types a wrong password once is not one step away from being
// locked out forever.
func TestManagerSuccessfulAuthClearsFailures(t *testing.T) {
	accounts := NewMemAccountStore()
	_ = accounts.CreateAccount("e", "p")
	m := NewManager(&testLinker{}, accounts)
	h := m.Handler()

	wrong := httptest.NewRequest("GET", "/spaces/s/assets/a/original.mov", nil)
	wrong.RemoteAddr = "192.0.2.30:1"
	wrong.SetBasicAuth("e", "wrong")
	h.ServeHTTP(httptest.NewRecorder(), wrong)

	good := httptest.NewRequest("GET", "/spaces/s/assets/a/original.mov", nil)
	good.RemoteAddr = "192.0.2.30:1"
	good.SetBasicAuth("e", "p")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, good)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatal("correct credentials after one failure must not be rate limited")
	}
}

// WEBDAV-001: the bcrypt verify semaphore is bounded and starts empty, and a
// concurrent flood from distinct sources (each below the per-IP failure
// budget) completes without deadlock and without cross-IP blocking.
func TestManagerBoundedBcryptConcurrency(t *testing.T) {
	accounts := NewMemAccountStore()
	_ = accounts.CreateAccount("e", "p")
	m := NewManager(&testLinker{}, accounts)
	if cap(m.bcryptSlots) != webdavBcryptSlots {
		t.Fatalf("bcrypt semaphore capacity = %d, want %d", cap(m.bcryptSlots), webdavBcryptSlots)
	}
	if len(m.bcryptSlots) != 0 {
		t.Fatalf("bcrypt semaphore starts occupied: %d", len(m.bcryptSlots))
	}
	h := m.Handler()
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r := httptest.NewRequest("GET", "/spaces/s/assets/a/original.mov", nil)
			r.RemoteAddr = fmt.Sprintf("198.51.100.%d:1234", n)
			r.SetBasicAuth("e", "wrong")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("flood request %d = %d, want 401", n, rec.Code)
			}
		}(i)
	}
	wg.Wait()
	if len(m.bcryptSlots) != 0 {
		t.Fatalf("bcrypt semaphore leaked slots after the flood: %d", len(m.bcryptSlots))
	}
}

// WEBDAV-002: the verify slot is released as soon as Authenticate returns,
// not held for the rest of the request. Six transfers held mid-stream (each
// blocked inside its body write, as an editor stalling a download would)
// must leave the authentication semaphore open for the next login.
func TestBcryptSlotsReleasedWhileTransfersAreHeld(t *testing.T) {
	dir := t.TempDir()
	// 10 MiB so a stalled reader reliably blocks the handler mid-body.
	payload := bytes.Repeat([]byte("0123456789"), 1<<20)
	if err := os.WriteFile(filepath.Join(dir, "clip-a.mov"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	l := &testLinker{dir: dir, originals: map[string]string{"a": "clip-a.mov"}}
	accounts := NewMemAccountStore()
	if err := accounts.CreateAccount("editor", "s3cret"); err != nil {
		t.Fatal(err)
	}
	m := NewManager(l, accounts)
	space := m.CreateSpace("space-1")
	if _, err := space.LinkOriginal(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	heldCtx, cancelHeld := context.WithCancel(context.Background())
	defer cancelHeld()
	started := make(chan struct{}, webdavBcryptSlots)
	var wg sync.WaitGroup
	for range webdavBcryptSlots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(heldCtx, "GET", srv.URL+"/spaces/space-1/assets/a/original.mov", nil)
			req.SetBasicAuth("editor", "s3cret")
			resp, err := srv.Client().Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if _, err := resp.Body.Read(make([]byte, 1)); err != nil {
				return
			}
			started <- struct{}{}
			<-heldCtx.Done() // hold the transfer open, unread, until the test ends
		}()
	}
	for range webdavBcryptSlots {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("held transfers did not start streaming")
		}
	}
	if got := len(m.bcryptSlots); got != 0 {
		t.Fatalf("bcrypt slots held by in-flight transfers: %d", got)
	}

	// A fresh login must go through while the six transfers stay blocked.
	authCtx, cancelAuth := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAuth()
	req, _ := http.NewRequestWithContext(authCtx, "GET", srv.URL+"/spaces/space-1/assets/a/original.mov", nil)
	req.SetBasicAuth("editor", "s3cret")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("authenticating while six transfers are held: %v", err)
	}
	resp.Body.Close()
}
