package app

import (
	"strings"
	"testing"

	"github.com/ev/timingdex/internal/config"
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
