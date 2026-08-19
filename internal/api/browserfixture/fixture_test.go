//go:build playwrightfixture

package browserfixture

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
)

func TestFixtureCloseRemovesTemporaryDirectory(t *testing.T) {
	f, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dir := f.Dir
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("fixture directory still exists after Close: %v", err)
	}
}

func TestServePlaywrightFixture(t *testing.T) {
	if os.Getenv("TIMINGDEX_PLAYWRIGHT_SERVE") != "1" {
		t.Skip("fixture server is opt-in")
	}
	f, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	addr := os.Getenv("TIMINGDEX_PLAYWRIGHT_ADDR")
	if addr == "" {
		addr = "127.0.0.1:4173"
	}
	t.Logf("fixture server listening on https://%s", addr)
	server := &http.Server{Addr: addr, Handler: f.Handler}
	if err := server.ListenAndServeTLS(f.TLSCertificate, f.TLSKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}
