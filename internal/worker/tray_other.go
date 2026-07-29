//go:build !windows

package worker

import (
	"context"
	"errors"
)

// TrayOptions mirrors the Windows definition so callers compile everywhere. The
// notification area is a Windows concept; on other platforms the Worker is run
// from a terminal or a service manager, and there is nothing to put an icon in.
type TrayOptions struct {
	Tooltip     string
	SettingsURL string
	OnQuit      func()
}

// ShowTray reports that there is no tray here rather than silently succeeding: a
// caller that asked for one and got a process with no way to quit would be worse
// than an explicit error.
func ShowTray(_ context.Context, _ TrayOptions) error {
	return errors.New("the notification-area icon is only available on Windows")
}
