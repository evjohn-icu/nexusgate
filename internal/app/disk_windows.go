//go:build windows

package app

import "errors"

// freeBytes exists so the pipeline's disk preflight compiles on Windows, but
// it always reports "unknown". The guard is a safety net for NAS/local Linux
// hubs; on Windows the preflight degrades to a skip — never a false blocker —
// so a Windows Hub behaves exactly as it did without the knob.
func freeBytes(string) (int64, error) {
	return 0, errors.New("disk preflight unsupported on windows")
}
