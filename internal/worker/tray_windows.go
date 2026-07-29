//go:build windows

package worker

// The notification-area icon is built directly on Win32 through syscall rather
// than on a toolkit. A tray icon is one API (Shell_NotifyIconW) plus a
// message-only window and a message pump, and the alternatives all cost the
// property this project depends on: `GOOS=windows GOARCH=amd64 go build` from any
// machine, producing one .exe with no DLLs beside it. Qt would mean a C++
// toolchain and ~50MB of runtime libraries; the CGO tray libraries would mean a
// mingw cross-compiler. The Worker install wizard hands out a single file, and
// this keeps that true.

import (
	"context"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassEx     = user32.NewProc("RegisterClassExW")
	procCreateWindowEx      = user32.NewProc("CreateWindowExW")
	procDefWindowProc       = user32.NewProc("DefWindowProcW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procGetMessage          = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessage     = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procPostMessage         = user32.NewProc("PostMessageW")
	procLoadIcon            = user32.NewProc("LoadIconW")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procAppendMenu          = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")

	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
	procShellExecute    = shell32.NewProc("ShellExecuteW")

	procGetModuleHandle = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmDestroy       = 0x0002
	wmCommand       = 0x0111
	wmRightButtonUp = 0x0205
	wmLeftButtonUp  = 0x0202
	wmApp           = 0x8000
	wmTrayCallback  = wmApp + 1

	nimAdd     = 0x0000
	nimDelete  = 0x0002
	nifMessage = 0x0001
	nifIcon    = 0x0002
	nifTip     = 0x0004

	mfString    = 0x0000
	mfSeparator = 0x0800

	tpmLeftAlign   = 0x0000
	tpmRightButton = 0x0002

	hwndMessage    = ^uintptr(2) // (HWND)-3
	idiApplication = 32512

	swShowNormal = 1
)

const (
	menuIDSettings = 1001
	menuIDQuit     = 1002
)

type notifyIconData struct {
	Size            uint32
	Wnd             uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        [16]byte
	BalloonIcon     uintptr
}

type point struct{ X, Y int32 }

type msg struct {
	Wnd     uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	Private uint32
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

// TrayOptions is everything the icon needs. Deliberately small: the tray is a
// launcher and a quit button, not a second place where settings live.
type TrayOptions struct {
	Tooltip string
	// SettingsURL is the loopback settings page. It embeds the access token, so
	// it is handed straight to the browser and never logged.
	SettingsURL string
	// OnQuit runs before the message loop returns, so the Worker can stop its
	// lease loop and release any job it holds instead of vanishing mid-derive.
	OnQuit func()
}

// ShowTray runs the notification-area icon until the user chooses 退出 or ctx is
// cancelled. It must own its thread: Win32 message queues are per-thread, so the
// pump has to stay on the same OS thread that created the window.
func ShowTray(ctx context.Context, options TrayOptions) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	instance, _, _ := procGetModuleHandle.Call(0)
	className, err := syscall.UTF16PtrFromString("TimingdexWorkerTray")
	if err != nil {
		return err
	}

	var window uintptr
	var icon notifyIconData
	quitRequested := false

	// The callback is registered with the OS, so it must not be a closure that
	// outlives this call; it is created once here and the window is destroyed
	// before returning.
	wndProc := syscall.NewCallback(func(hwnd, message, wparam, lparam uintptr) uintptr {
		switch uint32(message) {
		case wmTrayCallback:
			switch uint32(lparam) {
			case wmRightButtonUp, wmLeftButtonUp:
				showTrayMenu(hwnd)
			}
			return 0
		case wmCommand:
			switch wparam & 0xffff {
			case menuIDSettings:
				openInBrowser(options.SettingsURL)
			case menuIDQuit:
				quitRequested = true
				procPostMessage.Call(hwnd, uintptr(wmDestroy), 0, 0)
			}
			return 0
		case wmDestroy:
			procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&icon)))
			procPostQuitMessage.Call(0)
			return 0
		}
		result, _, _ := procDefWindowProc.Call(hwnd, message, wparam, lparam)
		return result
	})

	class := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   wndProc,
		Instance:  instance,
		ClassName: className,
	}
	if atom, _, callErr := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		return fmt.Errorf("register tray window class: %w", callErr)
	}

	// HWND_MESSAGE gives a window with no presence on screen or in the taskbar,
	// which is what a tray-only program wants: the icon is the entire UI.
	window, _, callErr := procCreateWindowEx.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(className)),
		0, 0, 0, 0, 0, hwndMessage, 0, instance, 0)
	if window == 0 {
		return fmt.Errorf("create tray window: %w", callErr)
	}
	defer procDestroyWindow.Call(window)

	appIcon, _, _ := procLoadIcon.Call(0, idiApplication)
	icon = notifyIconData{
		Size:            uint32(unsafe.Sizeof(notifyIconData{})),
		Wnd:             window,
		ID:              1,
		Flags:           nifMessage | nifIcon | nifTip,
		CallbackMessage: wmTrayCallback,
		Icon:            appIcon,
	}
	copyUTF16(icon.Tip[:], options.Tooltip)
	if ok, _, callErr := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&icon))); ok == 0 {
		return fmt.Errorf("add tray icon: %w", callErr)
	}

	// Cancelling the context has to break the pump, which blocks in GetMessage.
	// Posting WM_DESTROY is the only way in: the queue is thread-affine, and
	// PostMessage is documented as safe from another thread.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			procPostMessage.Call(window, uintptr(wmDestroy), 0, 0)
		case <-done:
		}
	}()

	var message msg
	for {
		result, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		// 0 is WM_QUIT, -1 is an error; both end the loop.
		if int32(result) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
	}
	if quitRequested && options.OnQuit != nil {
		options.OnQuit()
	}
	return nil
}

func showTrayMenu(hwnd uintptr) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)
	appendMenuItem(menu, menuIDSettings, "设置…")
	procAppendMenu.Call(menu, mfSeparator, 0, 0)
	appendMenuItem(menu, menuIDQuit, "退出")

	var cursor point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
	// Without this the menu stays open after a click elsewhere: Windows only
	// dismisses a tracked popup belonging to the foreground window.
	procSetForegroundWindow.Call(hwnd)
	procTrackPopupMenu.Call(menu, tpmLeftAlign|tpmRightButton, uintptr(cursor.X), uintptr(cursor.Y), 0, hwnd, 0)
}

func appendMenuItem(menu uintptr, id uintptr, label string) {
	text, err := syscall.UTF16PtrFromString(label)
	if err != nil {
		return
	}
	procAppendMenu.Call(menu, mfString, id, uintptr(unsafe.Pointer(text)))
}

// openInBrowser hands the URL to the shell. ShellExecute is used rather than
// spawning cmd /c start, which would put the URL — and the token it carries —
// on a command line visible to every process on the machine.
func openInBrowser(target string) {
	if target == "" {
		return
	}
	verb, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return
	}
	url, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return
	}
	procShellExecute.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(url)), 0, 0, swShowNormal)
}

// copyUTF16 fills a fixed Win32 buffer, always leaving the terminating NUL.
func copyUTF16(dst []uint16, value string) {
	encoded := syscall.StringToUTF16(value)
	if len(encoded) > len(dst) {
		encoded = encoded[:len(dst)]
		encoded[len(encoded)-1] = 0
	}
	copy(dst, encoded)
}
