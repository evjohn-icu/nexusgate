package app

import (
	"errors"
	"strings"
	"testing"
)

func TestPersistedErrorMessageIsBounded(t *testing.T) {
	err := errors.New(strings.Repeat("x", 4096))
	got := persistedErrorMessage(err)
	if len(got) > 2048+len("…(truncated)") {
		t.Fatalf("persisted error length = %d, want at most %d", len(got), 2048+len("…(truncated)"))
	}
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Fatalf("persisted error is missing truncation marker: %q", got)
	}
}

func TestPersistedRawIsBounded(t *testing.T) {
	got := persistedRaw(strings.Repeat("raw", 2048))
	if len(got) > 2048+len("…(truncated)") {
		t.Fatalf("persisted raw length = %d, want at most %d", len(got), 2048+len("…(truncated)"))
	}
}
