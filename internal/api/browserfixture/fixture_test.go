//go:build playwrightfixture

package browserfixture

import (
	"context"
	"net/http"
	"os"
	"testing"
)

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
	t.Logf("fixture server listening on http://%s", addr)
	if err := http.ListenAndServe(addr, f.Handler); err != nil {
		t.Fatal(err)
	}
}
