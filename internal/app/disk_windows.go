//go:build windows

package app

import (
	"math"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// freeBytes reports the space available to this account, including quotas.
func freeBytes(path string) (int64, error) {
	if path == "" {
		return 0, os.ErrNotExist
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	if strings.HasPrefix(absolute, `\\`) && !strings.HasSuffix(absolute, `\`) {
		absolute += `\`
	}
	name, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return 0, err
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(name, &available, nil, nil); err != nil {
		return 0, err
	}
	return int64(min(available, math.MaxInt64)), nil
}
