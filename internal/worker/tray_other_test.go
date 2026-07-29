//go:build !windows

package worker

import (
	"context"
	"strings"
	"testing"
)

// The stub must fail loudly. A caller that asked for a tray and got a running
// process with no icon would have no way to quit it and no way to reach settings.
func TestShowTrayIsUnavailableOffWindows(t *testing.T) {
	err := ShowTray(context.Background(), TrayOptions{Tooltip: "x"})
	if err == nil {
		t.Fatal("ShowTray must report that there is no notification area here")
	}
	if !strings.Contains(err.Error(), "Windows") {
		t.Fatalf("error should name the platform requirement: %v", err)
	}
}
