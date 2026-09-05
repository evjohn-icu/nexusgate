//go:build playwrightfixture

package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/evjohn-icu/nexusgate/internal/api/browserfixture"
)

func main() {
	addr := os.Getenv("NEXUSGATE_PLAYWRIGHT_ADDR")
	if addr == "" {
		addr = "127.0.0.1:4173"
	}
	fixture, err := browserfixture.New(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	defer fixture.Close()

	server := &http.Server{Addr: addr, Handler: fixture.Handler}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		_ = server.Shutdown(context.Background())
	}()
	log.Printf("fixture server listening on https://%s", addr)
	if err := server.ListenAndServeTLS(fixture.TLSCertificate, fixture.TLSKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
