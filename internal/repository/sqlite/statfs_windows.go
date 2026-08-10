//go:build windows

package sqlite

import "errors"

// freeBytesWindows refuses the free-space probe. Windows hubs skip the disk
// preflight entirely: the check must never be a false blocker on a platform
// where it is not implemented, so checkPreMigrationConditions treats any probe
// error as "measurement unavailable" and continues.
func freeBytesWindows(string) (uint64, error) {
	return 0, errors.New("disk preflight unsupported on windows")
}

func init() {
	statfsFreeBytes = freeBytesWindows
}
