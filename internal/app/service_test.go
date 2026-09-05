package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/smbdiscover"
)

func TestNewServiceRejectsUnknownVideoProviderInsteadOfSilentlyUsingMock(t *testing.T) {
	service, err := NewService(nil, config.Config{Providers: config.ProvidersConfig{VisionPrimary: "not-configured"}})
	if err == nil {
		t.Fatalf("service=%v, expected provider configuration error", service)
	}
	if service != nil {
		t.Fatalf("service must not be constructed after provider initialization failure: %#v", service)
	}
	if !strings.Contains(err.Error(), "not-configured") {
		t.Fatalf("error must identify the invalid provider: %v", err)
	}
}

func TestDiscoverSMBIsSingleFlight(t *testing.T) {
	service := &Service{}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	service.SetSMBDiscoverer(func(context.Context) (smbdiscover.Result, error) {
		calls.Add(1)
		close(started)
		<-release
		return smbdiscover.Result{}, nil
	})

	firstDone := make(chan error, 1)
	go func() {
		_, err := service.DiscoverSMB(context.Background())
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first discovery did not start")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := service.DiscoverSMB(context.Background())
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if !errors.Is(err, ErrDiscoverInProgress) {
			t.Fatalf("second discovery error = %v, want ErrDiscoverInProgress", err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("second discovery blocked instead of returning ErrDiscoverInProgress")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("discoverer calls = %d, want 1", got)
	}

	close(release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first discovery error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first discovery did not finish after release")
	}
}
